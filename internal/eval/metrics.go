package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unicode"
)

// MetricResult is one metric outcome. Diagnosis explains an imperfect
// score and is the seam reserved for automated failure analysis.
type MetricResult struct {
	Score     float64
	Diagnosis string
}

// MetricFunc scores a raw model output against the expected value.
// Implementations strip fences themselves (stripFence) so a metric can
// opt out when its input is not expected to be JSON-embedded.
type MetricFunc func(output string, expected any) MetricResult

// metricRegistry is the eval-side metric registry (roadmap V7 §2.3,
// mirroring the optimizer registry pattern of docs/plugins.md): one
// metric = one function + one registration line. Writes happen at
// init/plugin-registration time only; evaluation workers read it
// concurrently, hence the RWMutex.
var (
	metricMu       sync.RWMutex
	metricRegistry = map[string]MetricFunc{}
)

// RegisterMetric installs fn under name and returns an error — instead
// of the optimizer registry's panic — so registration sites can decide
// their own failure policy; the in-repo init sites fail fast through
// mustRegisterMetric. llm_judge is a reserved name: the engine
// dispatches it outside the registry (it needs the provider and cannot
// run inside the pure Evaluate path), so registering it is rejected.
// Duplicate names are rejected to keep "registration = visible" honest.
func RegisterMetric(name string, fn MetricFunc) error {
	if name == "" {
		return errors.New("metric name is empty")
	}
	if fn == nil {
		return fmt.Errorf("metric %q: nil function", name)
	}
	if name == MetricLLMJudge {
		return fmt.Errorf("metric %q is reserved: the engine dispatches it via the judge provider, it cannot run inside Evaluate", name)
	}
	metricMu.Lock()
	defer metricMu.Unlock()
	if _, dup := metricRegistry[name]; dup {
		return fmt.Errorf("metric %q already registered", name)
	}
	metricRegistry[name] = fn
	return nil
}

// mustRegisterMetric is the init-time registration helper: a conflict
// here is a programming error, so fail fast like optimizers.Register.
func mustRegisterMetric(name string, fn MetricFunc) {
	if err := RegisterMetric(name, fn); err != nil {
		panic(err)
	}
}

// Evaluate scores a raw model output against the expected value with
// the named metric, looked up in the metric registry. Outputs are
// fence-stripped by the metric itself; unknown metrics score zero with
// the historical diagnosis, byte for byte.
func Evaluate(metric, output string, expected any) MetricResult {
	metricMu.RLock()
	fn, ok := metricRegistry[metric]
	metricMu.RUnlock()
	if ok {
		return fn(output, expected)
	}
	return MetricResult{Diagnosis: fmt.Sprintf("unknown metric %q", metric)}
}

// Builtin deterministic metrics: same bodies as the pre-registry
// switch, now registered so the registry is the single dispatch table.
func init() {
	mustRegisterMetric("json_validator", func(output string, _ any) MetricResult {
		return evalJSONValidator(output)
	})
	mustRegisterMetric("exact_match", evalExactMatch)
	mustRegisterMetric("f1", evalF1)
}

// evalJSONValidator scores 1 iff the whole (fence-stripped) output
// unmarshals as JSON. Structural comparison against expected is out of
// scope for this slice.
func evalJSONValidator(output string) MetricResult {
	s := stripFence(output)
	if s == "" {
		return MetricResult{Diagnosis: "empty output"}
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return MetricResult{Diagnosis: "output is not valid JSON: " + truncate(err.Error(), 120)}
	}
	return MetricResult{Score: 1}
}

// evalExactMatch compares strings after trimming (case-sensitive) and
// structured values after JSON canonicalization, so key order never
// matters.
func evalExactMatch(output string, expected any) MetricResult {
	s := stripFence(output)
	if exp, ok := expected.(string); ok {
		if strings.TrimSpace(s) == strings.TrimSpace(exp) {
			return MetricResult{Score: 1}
		}
		return MetricResult{Diagnosis: "string mismatch (whitespace-trimmed, case-sensitive)"}
	}
	want, err := canonicalize(expected)
	if err != nil {
		return MetricResult{Diagnosis: "expected value is not canonicalizable: " + err.Error()}
	}
	var got any
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		return MetricResult{Diagnosis: "output is not valid JSON: " + truncate(err.Error(), 120)}
	}
	if reflect.DeepEqual(got, want) {
		return MetricResult{Score: 1}
	}
	return MetricResult{Diagnosis: "structured value mismatch"}
}

// evalF1 computes a multiset F1 whose element choice depends on the
// expected kind: arrays compare elements, objects compare leaf values
// (strings and array elements), strings compare whitespace/CJK tokens.
func evalF1(output string, expected any) MetricResult {
	s := stripFence(output)
	switch exp := expected.(type) {
	case string:
		return multisetF1(tokenize(s), tokenize(exp))
	case []any:
		var got any
		if err := json.Unmarshal([]byte(s), &got); err != nil {
			return MetricResult{Diagnosis: "output is not valid JSON: " + truncate(err.Error(), 120)}
		}
		gotArr, ok := got.([]any)
		if !ok {
			return MetricResult{Diagnosis: "expected a JSON array, output is a different JSON kind"}
		}
		return multisetF1(canonicalElements(gotArr), canonicalElements(exp))
	case map[string]any:
		var got any
		if err := json.Unmarshal([]byte(s), &got); err != nil {
			return MetricResult{Diagnosis: "output is not valid JSON: " + truncate(err.Error(), 120)}
		}
		return multisetF1(collectLeaves(got, nil), collectLeaves(exp, nil))
	default:
		return MetricResult{Diagnosis: fmt.Sprintf("unsupported expected kind %T for f1", expected)}
	}
}

// canonicalize normalizes a decoded YAML/JSON value through JSON so
// both sides compare with identical number types and unordered maps.
func canonicalize(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// canonicalElements renders array elements as canonical JSON strings so
// multiset comparison is key-order and number-format insensitive.
func canonicalElements(elems []any) []string {
	out := make([]string, len(elems))
	for i, e := range elems {
		b, err := json.Marshal(e)
		if err != nil {
			out[i] = fmt.Sprintf("%v", e)
			continue
		}
		out[i] = string(b)
	}
	return out
}

// collectLeaves gathers leaf values for F1 over objects: strings as-is
// (recursing through arrays and nested objects), other scalars in
// their JSON form.
func collectLeaves(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = collectLeaves(e, out)
		}
	case map[string]any:
		for _, val := range x {
			out = collectLeaves(val, out)
		}
	default:
		if b, err := json.Marshal(x); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}

// multisetF1 computes precision/recall/F1 over element multisets. Two
// empty multisets match perfectly.
func multisetF1(got, want []string) MetricResult {
	if len(got) == 0 && len(want) == 0 {
		return MetricResult{Score: 1}
	}
	counts := make(map[string]int, len(want))
	for _, w := range want {
		counts[w]++
	}
	inter := 0
	for _, g := range got {
		if counts[g] > 0 {
			counts[g]--
			inter++
		}
	}
	if inter == 0 {
		return MetricResult{Diagnosis: "no overlapping elements"}
	}
	p := float64(inter) / float64(len(got))
	r := float64(inter) / float64(len(want))
	return MetricResult{Score: 2 * p * r / (p + r)}
}

// tokenize splits on whitespace; CJK characters become single tokens
// so Chinese text compares at character granularity. A naive \S+ split
// would treat a whole Chinese sentence as one token.
func tokenize(s string) []string {
	var toks []string
	var run []rune
	flush := func() {
		if len(run) > 0 {
			toks = append(toks, string(run))
			run = run[:0]
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isCJK(r):
			flush()
			toks = append(toks, string(r))
		default:
			run = append(run, r)
		}
	}
	flush()
	return toks
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// stripFence removes a single enclosing Markdown code fence: the whole
// trimmed output must be one fenced block. Text outside the fence is
// preserved so validators fail loudly on mixed output.
func stripFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	lines := strings.Split(t, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[len(lines)-1]) != "```" {
		return t
	}
	return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
