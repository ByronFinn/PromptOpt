package harness

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// Markers embedded in the synthesis meta-prompts. They are the seam
// fake LLM servers key on to tell synthesis stages apart from
// evaluation calls (evaluation prompts never contain them).
const (
	MarkerSpec    = "设计一个可自动评估的任务规格"
	MarkerSamples = "合成评测样本"
	MarkerProbes  = "语义等价的探针变体"
	MarkerRepair  = "修复上述问题为合法 JSON"
)

// Synthesizer produces the task spec, the evaluation samples and the
// probe variants with three independent optimizer-role LLM calls.
// Call traces (including failed calls and repairs) land in
// Dir/calls/NNN-<stage>.json. Not safe for concurrent use: the
// pipeline drives it sequentially.
type Synthesizer struct {
	Provider  provider.Provider
	Model     string
	MaxTokens int    // effective value floors at config.DefaultSynthMaxTokens
	Dir       string // synth/<run_id>

	// floor is the token cap learned from a successful escalation:
	// once a stage had to double its cap to escape a reasoning burn,
	// later stages start there instead of burning the floor again.
	floor int
	seq   int
	usage core.Usage
}

// Usage returns the cumulative token usage of every successful synth
// call, for the shared budget snapshot.
func (s *Synthesizer) Usage() core.Usage { return s.usage }

// SynthesizeSpec turns the user's natural-language prompt into a
// validated task specification.
func (s *Synthesizer) SynthesizeSpec(ctx context.Context, userPrompt string) (core.Task, error) {
	raw, err := s.call(ctx, "spec", buildSpecPrompt(userPrompt))
	if err != nil {
		return core.Task{}, err
	}
	type specPayload struct {
		Name           string   `json:"name"`
		Description    string   `json:"description"`
		PromptTemplate string   `json:"prompt_template"`
		Metrics        []string `json:"metrics"`
		PrimaryMetric  string   `json:"primary_metric"`
	}
	payload, _, err := defendWithRepair[specPayload](ctx, s, "spec", raw, func(p specPayload) ([]string, error) {
		task := core.Task{
			Name: p.Name, Description: p.Description, PromptTemplate: p.PromptTemplate,
			Metrics: p.Metrics, PrimaryMetric: p.PrimaryMetric,
		}
		return nil, task.Validate()
	})
	if err != nil {
		return core.Task{}, err
	}
	return core.Task{
		Name: payload.Name, Description: payload.Description,
		PromptTemplate: payload.PromptTemplate, Metrics: payload.Metrics,
		PrimaryMetric: payload.PrimaryMetric,
	}, nil
}

// SynthesizeSamples synthesizes n evaluation samples for the task.
// Splits are train/dev only: an out-of-range "test" is forced to dev
// and recorded as a warning, any other violation goes through the
// repair call. Sample ids missing or duplicated in the response are
// renumbered synth-NNN in order.
func (s *Synthesizer) SynthesizeSamples(ctx context.Context, spec core.Task, n int) ([]core.Sample, []string, error) {
	raw, err := s.call(ctx, "samples", buildSamplesPrompt(spec, n))
	if err != nil {
		return nil, nil, err
	}
	type samplesPayload struct {
		Samples []core.Sample `json:"samples"`
	}
	payload, warnings, err := defendWithRepair[samplesPayload](ctx, s, "samples", raw, func(p samplesPayload) ([]string, error) {
		if len(p.Samples) == 0 {
			return nil, errors.New("响应中没有样本")
		}
		samples, warnings, problems := normalizeSamples(p.Samples)
		if len(problems) > 0 {
			return nil, errors.New(strings.Join(problems, "; "))
		}
		if len(samples) != n {
			warnings = append(warnings, fmt.Sprintf("期望 %d 条样本，实际 %d 条", n, len(samples)))
		}
		return warnings, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return payload.Samples, warnings, nil
}

// SynthesizeProbes synthesizes k semantically equivalent prompt
// variants, each carrying the {input} placeholder, for the p¹ filter.
func (s *Synthesizer) SynthesizeProbes(ctx context.Context, spec core.Task, k int) ([]string, error) {
	raw, err := s.call(ctx, "probes", buildProbesPrompt(spec.PromptTemplate, k))
	if err != nil {
		return nil, err
	}
	type probesPayload struct {
		Probes []string `json:"probes"`
	}
	payload, _, err := defendWithRepair[probesPayload](ctx, s, "probes", raw, func(p probesPayload) ([]string, error) {
		if len(p.Probes) != k {
			return nil, fmt.Errorf("期望 %d 条探针变体，实际 %d 条", k, len(p.Probes))
		}
		for i, probe := range p.Probes {
			if !strings.Contains(probe, core.InputPlaceholder) {
				return nil, fmt.Errorf("探针变体 %d 缺少 %s 占位符", i+1, core.InputPlaceholder)
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return payload.Probes, nil
}

// maxSynthAttempts bounds the empty-content escalation ladder: a
// reasoning model can spend the whole completion cap on reasoning and
// return no content (finish_reason=length), so the cap doubles per
// attempt (floor → 2× → 4×) before the stage fails.
const maxSynthAttempts = 3

// call performs one traced LLM call. Every attempt — including failed
// ones and escalations — leaves a trace under Dir/calls/, keeping the
// synth artifact tree replayable.
func (s *Synthesizer) call(ctx context.Context, stage, prompt string) (string, error) {
	if err := os.MkdirAll(filepath.Join(s.Dir, "calls"), 0o755); err != nil {
		return "", fmt.Errorf("create calls dir: %w", err)
	}
	floor := max(s.MaxTokens, config.DefaultSynthMaxTokens, s.floor)
	var last provider.ChatResponse
	for attempt := range maxSynthAttempts {
		tokenCap := floor << attempt
		resp, err := s.chat(ctx, stage, prompt, tokenCap)
		if err != nil {
			return "", err
		}
		last = resp
		if resp.Content != "" {
			if attempt > 0 {
				// Remember the cap that worked so later stages skip the
				// reasoning burn instead of re-escalating from the floor.
				s.floor = tokenCap
			}
			return resp.Content, nil
		}
		if resp.FinishReason != "length" && resp.ReasoningContent == "" {
			return "", fmt.Errorf("synth call %s: 模型返回空内容（finish_reason=%s）",
				stage, cmp.Or(resp.FinishReason, "unknown"))
		}
		// The completion cap was consumed by reasoning: escalate.
	}
	return "", fmt.Errorf("synth call %s: 连续 %d 次空内容（finish_reason=%s，completion=%d tokens）— 推理模型耗尽了全部 completion 预算，请提高 --max-tokens",
		stage, maxSynthAttempts, cmp.Or(last.FinishReason, "unknown"), last.Usage.CompletionTokens)
}

// chat performs a single traced round trip; each attempt gets its own
// sequence number, so escalated retries write distinct trace files.
func (s *Synthesizer) chat(ctx context.Context, stage, prompt string, tokenCap int) (provider.ChatResponse, error) {
	s.seq++
	req := provider.ChatRequest{
		Model:     s.Model,
		MaxTokens: tokenCap,
		Role:      core.RoleOptimizer,
		Messages:  []provider.Message{{Role: "user", Content: prompt}},
	}
	start := time.Now()
	resp, err := s.Provider.Chat(ctx, req)
	trace := eval.CallTrace{
		Seq:       s.seq,
		SampleID:  stage,
		Role:      core.RoleOptimizer,
		Request:   req,
		Response:  resp,
		LatencyMS: time.Since(start).Milliseconds(),
		Time:      start,
	}
	if err != nil {
		trace.Error = err.Error()
	}
	path := filepath.Join(s.Dir, "calls", fmt.Sprintf("%03d-%s.json", s.seq, stage))
	if werr := SaveJSON(path, trace); werr != nil && err == nil {
		err = fmt.Errorf("write synth call trace: %w", werr)
	}
	if err != nil {
		return provider.ChatResponse{}, fmt.Errorf("synth call %s: %w", stage, err)
	}
	s.usage.PromptTokens += resp.Usage.PromptTokens
	s.usage.CompletionTokens += resp.Usage.CompletionTokens
	return resp, nil
}

// defendWithRepair runs the three-level JSON defense for one stage:
// local extraction of the payload, then a single LLM repair call
// covering both parse and semantic failures, then an error carrying a
// raw excerpt. check returns the stage's warnings or a violation
// error; a nil error means the payload is accepted.
func defendWithRepair[T any](ctx context.Context, s *Synthesizer, stage, raw string, check func(T) ([]string, error)) (T, []string, error) {
	var zero T
	var problems []string
	if strings.TrimSpace(raw) == "" {
		// An empty response cannot be repaired — the escalation ladder
		// in call() already handled the reasoning-burn case; this guard
		// keeps a nonsense repair call from ever firing.
		return zero, nil, errors.New("模型响应内容为空")
	}
	if payload, ok := tryParse[T](raw); ok {
		warnings, err := check(payload)
		if err == nil {
			return payload, warnings, nil
		}
		problems = []string{err.Error()}
	} else {
		problems = []string{"响应不是合法的 JSON 对象"}
	}
	repaired, err := s.call(ctx, stage+"-repair", buildRepairPrompt(raw, problems...))
	if err != nil {
		return zero, nil, s.excerptError(stage, raw, err)
	}
	payload, ok := tryParse[T](repaired)
	if !ok {
		return zero, nil, s.excerptError(stage, raw, errors.New("修复后仍不是合法 JSON"))
	}
	warnings, err := check(payload)
	if err != nil {
		return zero, nil, s.excerptError(stage, raw, fmt.Errorf("修复后仍不合规: %w", err))
	}
	return payload, warnings, nil
}

// tryParse extracts and decodes a JSON object from raw model output.
// A response that is already valid JSON passes through byte-for-byte;
// only a failed strict parse enters the clip/repair path, so healing
// can never rewrite a payload that was fine.
func tryParse[T any](raw string) (T, bool) {
	var zero T
	var out T
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &out); err == nil {
		return out, true
	}
	payload, ok := extractJSON(raw)
	if !ok {
		return zero, false
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return zero, false
	}
	return out, true
}

// excerptError attaches a raw-output excerpt so a failed synthesis can
// be diagnosed from the error alone.
func (s *Synthesizer) excerptError(stage, raw string, cause error) error {
	return fmt.Errorf("synth %s: %w（原文摘录：%.200s）", stage, cause, strings.TrimSpace(raw))
}

// extractJSON recovers a JSON object from a raw model response: it
// strips one enclosing code fence, clips the outermost brace span and
// repairs trailing commas. ok=false when no brace span exists.
func extractJSON(raw string) (string, bool) {
	s := stripOneFence(strings.TrimSpace(raw))
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return "", false
	}
	return stripTrailingCommas(s[start : end+1]), true
}

// stripTrailingCommas drops commas that directly precede a closing
// brace or bracket. It scans with string-literal and escape awareness,
// so byte sequences inside JSON strings (a value like "a, }") are
// copied verbatim — the regex it replaced would silently corrupt them.
func stripTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
			b.WriteByte(c)
		case inString:
			b.WriteByte(c)
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case c == '"':
			inString = true
			b.WriteByte(c)
		case c == ',':
			j := i + 1
			for j < len(s) && isJSONSpace(s[j]) {
				j++
			}
			if !(j < len(s) && (s[j] == '}' || s[j] == ']')) {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// stripOneFence removes a single enclosing Markdown code fence.
func stripOneFence(s string) string {
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

// normalizeSamples enforces the synthesis sample contract in place:
// splits are train/dev only (test is forced to dev and recorded),
// missing or duplicate ids are renumbered synth-NNN in order, and any
// remaining constraint violation is reported for the repair pass.
func normalizeSamples(samples []core.Sample) ([]core.Sample, []string, []string) {
	var warnings, problems []string
	seen := make(map[string]struct{}, len(samples))
	next := 1
	nextID := func() string {
		for {
			id := fmt.Sprintf("synth-%03d", next)
			next++
			if _, dup := seen[id]; !dup {
				return id
			}
		}
	}
	for i := range samples {
		s := &samples[i]
		if s.ID == "" {
			s.ID = nextID()
			warnings = append(warnings, fmt.Sprintf("第 %d 条样本缺少 id，已规整为 %s", i+1, s.ID))
		} else if _, dup := seen[s.ID]; dup {
			old := s.ID
			s.ID = nextID()
			warnings = append(warnings, fmt.Sprintf("样本 id %q 重复，已规整为 %s", old, s.ID))
		}
		seen[s.ID] = struct{}{}
		switch s.Split {
		case "train", "dev":
		case "test":
			s.Split = "dev"
			warnings = append(warnings, fmt.Sprintf("样本 %q split=test 越界，已强转为 dev", s.ID))
		default:
			problems = append(problems, fmt.Sprintf("样本 %q split=%q 非法（仅允许 train/dev）", s.ID, s.Split))
			continue
		}
		if err := s.Validate(); err != nil {
			problems = append(problems, fmt.Sprintf("样本 %q 非法: %v", s.ID, err))
		}
	}
	return samples, warnings, problems
}

// --- meta prompts ---------------------------------------------------------

func buildSpecPrompt(userPrompt string) string {
	return fmt.Sprintf(`你是评测集构造专家。请根据用户的任务描述，%s。
严格输出 JSON，除 JSON 外不要输出任何其他内容，不要使用代码围栏：
{"name": "任务英文短名", "description": "任务一句话描述", "prompt_template": "完整提示词模板", "metrics": ["..."], "primary_metric": "..."}

要求：
- prompt_template 必须包含 {input} 占位符，评测时样本输入将原样替换进该位置
- metrics 从 exact_match / f1 / json_validator 中选择 1~3 个，与任务输出形态匹配
- primary_metric 必须是 metrics 之一

用户任务描述：
%s`, MarkerSpec, userPrompt)
}

func buildSamplesPrompt(spec core.Task, n int) string {
	specJSON, _ := json.Marshal(spec)
	return fmt.Sprintf(`你是评测集构造专家。任务规格如下（JSON）：
%s

请为该任务%s，共 %d 条。严格输出 JSON，除 JSON 外不要输出任何其他内容，不要使用代码围栏：
{"samples": [{"id": "唯一英文数字短 id", "input": "真实多样的任务输入", "expected": 参考答案, "split": "train 或 dev"}]}

要求：
- input 贴合任务领域，覆盖不同难度与表述，禁止相互重复
- expected 是该输入下的理想参考答案，类型为字符串、数组或对象，能被任务指标直接评分
- split 只能取 train 或 dev`, specJSON, MarkerSamples, n)
}

func buildProbesPrompt(template string, k int) string {
	return fmt.Sprintf(`你是提示词工程专家。下面是一个提示词模板：
%s

请生成 %d 条与它%s（措辞、结构与顺序不同，任务要求不变）。严格输出 JSON，除 JSON 外不要输出任何其他内容，不要使用代码围栏：
{"probes": ["变体 1", "变体 2"]}

每个变体都必须包含 {input} 占位符。`, template, k, MarkerProbes)
}

func buildRepairPrompt(raw string, problems ...string) string {
	return fmt.Sprintf(`下面这段内容本应是符合要求的 JSON，但存在问题：
%s

问题列表：
%s

请%s，严格输出 JSON 本身，除 JSON 外不要输出任何其他内容，不要使用代码围栏。`, raw, strings.Join(problems, "\n"), MarkerRepair)
}
