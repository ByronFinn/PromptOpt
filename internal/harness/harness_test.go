package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- fake OpenAI backend ----------------------------------------------------

// startLLM runs a fake OpenAI-compatible server recording raw request
// bodies; respond decides status and body per call.
func startLLM(t *testing.T, respond func(call int, body string) (int, string)) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(b))
		call := len(*bodies)
		mu.Unlock()
		status, respBody := respond(call, string(b))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

// completion builds a fixed chat-completion response body.
func completion(content string) string {
	return fmt.Sprintf(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, content)
}

// --- synthesized fixtures (jiuwei-tcm 中医领域) ------------------------------

const specJSON = `{"name":"tcm_zhenghou","description":"从中医医案文本中判断证候名称","prompt_template":"你是中医辨证助手。阅读下面的文本，判断证候名称，只输出证候名称本身。文本：{input}","metrics":["exact_match"],"primary_metric":"exact_match"}`

const samplesJSON = `{"samples":[
 {"id":"s1","input":"恶寒发热，无汗，头身疼痛，脉浮紧。","expected":"风寒束表","split":"train"},
 {"id":"s2","input":"发热微恶风寒，咽痛口渴，咳嗽痰黄，脉浮数。","expected":"风热犯表","split":"dev"},
 {"id":"s3","input":"心烦不寐，心悸不安，腰膝酸软，脉细数。","expected":"心肾不交","split":"train"},
 {"id":"","input":"五心烦热，潮热盗汗，口咽干燥，舌红少苔。","expected":"阴虚火旺","split":"test"}
]}`

const probesJSON = `{"probes":["变体甲：阅读下列文本，直接输出其证候名称：{input}","变体乙：仅输出下面文本对应的证候名称：{input}"]}`

// probeScript drives the evaluation calls: per sample the score each
// probe variant and the baseline produce.
type probeScript struct {
	input, expected string
	p1, p2          int
}

var pipelineScript = []probeScript{
	{"恶寒发热，无汗，头身疼痛，脉浮紧。", "风寒束表", 1, 1},     // dead easy
	{"发热微恶风寒，咽痛口渴，咳嗽痰黄，脉浮数。", "风热犯表", 0, 0}, // dead hard
	{"心烦不寐，心悸不安，腰膝酸软，脉细数。", "心肾不交", 1, 0},   // keep
	{"五心烦热，潮热盗汗，口咽干燥，舌红少苔。", "阴虚火旺", 0, 1},  // keep
}

// scriptRouter routes a fake LLM by the synthesis markers, falling
// back to the scripted evaluation scores.
func scriptRouter(t *testing.T, specResp, samplesResp, probesResp func() string, eval func(body string) string) func(int, string) (int, string) {
	return func(_ int, body string) (int, string) {
		switch {
		case strings.Contains(body, MarkerSpec):
			return http.StatusOK, completion(specResp())
		case strings.Contains(body, MarkerSamples):
			return http.StatusOK, completion(samplesResp())
		case strings.Contains(body, MarkerProbes):
			return http.StatusOK, completion(probesResp())
		case strings.Contains(body, MarkerRepair):
			t.Errorf("unexpected repair call: %.200s", body)
			return http.StatusOK, completion("{}")
		default:
			return http.StatusOK, completion(eval(body))
		}
	}
}

func newSynthesizer(t *testing.T, srv *httptest.Server) *Synthesizer {
	t.Helper()
	return &Synthesizer{
		Provider:  provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Model:     "jiuwei-tcm",
		MaxTokens: 1024,
		Dir:       t.TempDir(),
	}
}

// --- JSON defense (three levels) --------------------------------------------

func TestSynthesizeSpecLocalRepair(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"fenced only", "```json\n" + specJSON + "\n```"},
		{"fence with trailing comma", "```json\n" + strings.Replace(specJSON, `"exact_match"}`, `"exact_match",}`, 1) + "\n```"},
		{"prose and fence and trailing comma", "好的，规格如下：\n```json\n" + strings.Replace(specJSON, `"exact_match"}`, `"exact_match",}`, 1) + "\n```\n以上。"},
		{"bare with stray whitespace", "\n\n  " + specJSON + "  \n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, bodies := startLLM(t, func(int, string) (int, string) {
				return http.StatusOK, completion(tc.content)
			})
			s := newSynthesizer(t, srv)
			task, err := s.SynthesizeSpec(context.Background(), "从中医医案判断证候")
			if err != nil {
				t.Fatalf("SynthesizeSpec: %v", err)
			}
			if task.Name != "tcm_zhenghou" || task.Primary() != "exact_match" ||
				!strings.Contains(task.PromptTemplate, "{input}") {
				t.Errorf("task = %+v", task)
			}
			if got := len(*bodies); got != 1 {
				t.Errorf("llm calls = %d, want 1 (repair must not fire)", got)
			}
			entries, err := os.ReadDir(filepath.Join(s.Dir, "calls"))
			if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), "001-spec.json") {
				t.Errorf("call traces = %v (%v), want 001-spec.json", entries, err)
			}
			if u := s.Usage(); u.PromptTokens != 10 || u.CompletionTokens != 5 {
				t.Errorf("usage = %+v, want 10/5", u)
			}
		})
	}
}

func TestSynthesizeTruncatedJSONTriggersRepairCall(t *testing.T) {
	truncated := `{"name":"tcm_zhenghou","description":"从中医`
	srv, bodies := startLLM(t, func(call int, _ string) (int, string) {
		if call == 1 {
			return http.StatusOK, completion(truncated)
		}
		return http.StatusOK, completion(specJSON)
	})
	s := newSynthesizer(t, srv)
	task, err := s.SynthesizeSpec(context.Background(), "从中医医案判断证候")
	if err != nil {
		t.Fatalf("SynthesizeSpec: %v", err)
	}
	if task.Name != "tcm_zhenghou" {
		t.Errorf("task = %+v, want the repaired spec", task)
	}
	if got := len(*bodies); got != 2 {
		t.Errorf("llm calls = %d, want 2 (initial + one repair)", got)
	}
	for _, name := range []string{"001-spec.json", "002-spec-repair.json"} {
		if _, err := os.Stat(filepath.Join(s.Dir, "calls", name)); err != nil {
			t.Errorf("missing call trace %s: %v", name, err)
		}
	}
}

func TestSynthesizeTwoFailuresErrorCarriesExcerpt(t *testing.T) {
	truncated := `{"name":"tcm_zhenghou","desc`
	srv, bodies := startLLM(t, func(call int, _ string) (int, string) {
		if call == 1 {
			return http.StatusOK, completion(truncated)
		}
		return http.StatusOK, completion("抱歉，我无法输出 JSON。")
	})
	s := newSynthesizer(t, srv)
	_, err := s.SynthesizeSpec(context.Background(), "从中医医案判断证候")
	if err == nil {
		t.Fatal("want an error after two consecutive failures")
	}
	if !strings.Contains(err.Error(), "原文摘录") || !strings.Contains(err.Error(), "tcm_zhenghou") {
		t.Errorf("error = %v, want a raw excerpt of the original output", err)
	}
	if got := len(*bodies); got != 2 {
		t.Errorf("llm calls = %d, want 2 (exactly one repair attempt)", got)
	}
}

func TestSynthesizeSamplesNormalizesIDsAndSplits(t *testing.T) {
	srv, bodies := startLLM(t, func(_ int, body string) (int, string) {
		if strings.Contains(body, MarkerSamples) {
			return http.StatusOK, completion(samplesJSON)
		}
		t.Errorf("unexpected call: %.200s", body)
		return http.StatusOK, completion("{}")
	})
	s := newSynthesizer(t, srv)
	spec := mustSpec(t)
	samples, warnings, err := s.SynthesizeSamples(context.Background(), spec, 4)
	if err != nil {
		t.Fatalf("SynthesizeSamples: %v", err)
	}
	if got := len(*bodies); got != 1 {
		t.Errorf("llm calls = %d, want 1 (test→dev and id fixes are local)", got)
	}
	if len(samples) != 4 {
		t.Fatalf("samples = %d, want 4", len(samples))
	}
	// The last sample lost its id and declared split=test: the id is
	// renumbered to the first free synth-NNN slot and test is forced
	// to dev.
	last := samples[3]
	if last.ID != "synth-001" || last.Split != "dev" {
		t.Errorf("renormalized sample = %+v, want id synth-001 split dev", last)
	}
	joined := strings.Join(warnings, "; ")
	if !strings.Contains(joined, "synth-001") || !strings.Contains(joined, "dev") {
		t.Errorf("warnings = %v, want the id and split normalizations recorded", warnings)
	}
	for _, s := range samples {
		if s.Split != "train" && s.Split != "dev" {
			t.Errorf("sample %s split = %q, want train/dev only", s.ID, s.Split)
		}
	}
}

// TestSynthesizeEscalatesOnReasoningBurn pins the empty-content
// escalation: a reasoning model can spend the entire completion cap on
// reasoning and return empty content (finish_reason=length) — the
// observed jiuwei-tcm e2e failure. The synthesizer must retry the same
// prompt with a doubled cap instead of feeding empty text into the
// JSON repair, remember the working cap for later stages, and put the
// escalated cap on the wire.
func TestSynthesizeEscalatesOnReasoningBurn(t *testing.T) {
	emptyBurn := `{"choices":[{"finish_reason":"length","message":{"role":"assistant","reasoning_content":"很长的推理过程……","content":""}}],"usage":{"prompt_tokens":10,"completion_tokens":4096,"total_tokens":4106}}`
	srv, bodies := startLLM(t, func(call int, body string) (int, string) {
		floor := strconv.Itoa(config.DefaultSynthMaxTokens)
		doubled := strconv.Itoa(config.DefaultSynthMaxTokens * 2)
		switch call {
		case 1:
			if !strings.Contains(body, `"max_tokens":`+floor) {
				t.Errorf("first attempt must carry the floor cap %s: %.200s", floor, body)
			}
			return http.StatusOK, emptyBurn
		case 2:
			if !strings.Contains(body, `"max_tokens":`+doubled) {
				t.Errorf("escalated attempt must double the cap to %s: %.200s", doubled, body)
			}
			return http.StatusOK, completion(specJSON)
		default:
			if !strings.Contains(body, `"max_tokens":`+doubled) {
				t.Errorf("later stages must start at the learned cap %s: %.200s", doubled, body)
			}
			return http.StatusOK, completion(probesJSON)
		}
	})
	s := newSynthesizer(t, srv)
	task, err := s.SynthesizeSpec(context.Background(), "从中医医案判断证候")
	if err != nil {
		t.Fatalf("SynthesizeSpec: %v", err)
	}
	if task.Name != "tcm_zhenghou" {
		t.Errorf("task = %+v", task)
	}
	if s.floor != config.DefaultSynthMaxTokens*2 {
		t.Errorf("learned floor = %d, want %d", s.floor, config.DefaultSynthMaxTokens*2)
	}
	probes, err := s.SynthesizeProbes(context.Background(), task, 2)
	if err != nil || len(probes) != 2 {
		t.Fatalf("SynthesizeProbes = %v, %v", probes, err)
	}
	if got := len(*bodies); got != 3 {
		t.Errorf("llm calls = %d, want 3 (burn + escalated spec + probes)", got)
	}
	// Both attempts of the spec stage leave distinct traces.
	for _, name := range []string{"001-spec.json", "002-spec.json", "003-probes.json"} {
		if _, err := os.Stat(filepath.Join(s.Dir, "calls", name)); err != nil {
			t.Errorf("missing call trace %s: %v", name, err)
		}
	}
	// The burned attempt still costs usage.
	if u := s.Usage(); u.PromptTokens != 30 || u.CompletionTokens != 4096+5+5 {
		t.Errorf("usage = %+v, want 3 calls with the burn included", u)
	}
}

// TestSynthesizeEmptyContentExhaustsEscalation: when every escalation
// still returns empty reasoning-burned content, the stage fails with
// an explicit, actionable attribution instead of a repair detour.
func TestSynthesizeEmptyContentExhaustsEscalation(t *testing.T) {
	emptyBurn := `{"choices":[{"finish_reason":"length","message":{"role":"assistant","reasoning_content":"很长的推理过程……","content":""}}],"usage":{"prompt_tokens":10,"completion_tokens":8192,"total_tokens":8202}}`
	srv, bodies := startLLM(t, func(int, string) (int, string) {
		return http.StatusOK, emptyBurn
	})
	s := newSynthesizer(t, srv)
	_, err := s.SynthesizeSpec(context.Background(), "从中医医案判断证候")
	if err == nil {
		t.Fatal("exhausted escalation must fail the stage")
	}
	if !strings.Contains(err.Error(), "请提高 --max-tokens") || !strings.Contains(err.Error(), "length") {
		t.Errorf("error = %v, want explicit reasoning-burn attribution", err)
	}
	if got := len(*bodies); got != maxSynthAttempts {
		t.Errorf("llm calls = %d, want %d (the escalation ladder)", got, maxSynthAttempts)
	}
}

// TestDefendSkipsRepairOnEmptyRaw: an empty raw response must fail
// immediately instead of asking the LLM to "repair" nothing (the
// observed wasted {} repair call).
func TestDefendSkipsRepairOnEmptyRaw(t *testing.T) {
	srv, bodies := startLLM(t, func(int, string) (int, string) {
		t.Error("repair call must not fire on an empty raw")
		return http.StatusOK, completion("{}")
	})
	s := newSynthesizer(t, srv)
	type probesPayload struct {
		Probes []string `json:"probes"`
	}
	_, _, err := defendWithRepair[probesPayload](context.Background(), s, "probes", "",
		func(p probesPayload) ([]string, error) { return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "内容为空") {
		t.Fatalf("error = %v, want an explicit empty-content failure", err)
	}
	if got := len(*bodies); got != 0 {
		t.Errorf("llm calls = %d, want 0 (no repair on empty raw)", got)
	}
}

func TestSynthesizeSamplesIllegalSplitTriggersRepair(t *testing.T) {
	broken := `{"samples":[{"id":"s1","input":"恶寒发热，无汗。","expected":"风寒束表","split":"validation"}]}`
	srv, bodies := startLLM(t, func(call int, body string) (int, string) {
		switch {
		case strings.Contains(body, MarkerRepair):
			return http.StatusOK, completion(samplesJSON)
		case strings.Contains(body, MarkerSamples):
			return http.StatusOK, completion(broken)
		default:
			t.Errorf("unexpected call: %.200s", body)
			return http.StatusOK, completion("{}")
		}
	})
	s := newSynthesizer(t, srv)
	spec := mustSpec(t)
	samples, _, err := s.SynthesizeSamples(context.Background(), spec, 4)
	if err != nil {
		t.Fatalf("SynthesizeSamples: %v", err)
	}
	if got := len(*bodies); got != 2 {
		t.Errorf("llm calls = %d, want 2 (initial + one repair)", got)
	}
	if len(samples) != 4 {
		t.Errorf("repaired samples = %d, want 4", len(samples))
	}
}

// --- pipeline end to end (autopilot) ----------------------------------------

func TestPipelineAutopilotEndToEnd(t *testing.T) {
	dir := t.TempDir()
	runsDir := filepath.Join(dir, "runs")
	runID := "20260929-090000-test"
	synthDir := filepath.Join(filepath.Join(dir, "synth"), runID)

	evalBody := func(body string) string {
		for _, sc := range pipelineScript {
			if !strings.Contains(body, sc.input) {
				continue
			}
			score := 1 // baseline always answers correctly
			switch {
			case strings.Contains(body, "变体甲"):
				score = sc.p1
			case strings.Contains(body, "变体乙"):
				score = sc.p2
			}
			if score == 1 {
				return sc.expected
			}
			return "无法辨证"
		}
		t.Errorf("evaluation call matched no scripted sample: %.300s", body)
		return "{}"
	}
	srv, bodies := startLLM(t, scriptRouter(t,
		func() string { return specJSON },
		func() string { return samplesJSON },
		func() string { return probesJSON },
		evalBody,
	))

	budget := eval.NewBudget(0, 0)
	var mu sync.Mutex
	var events []eval.Event
	p := &Pipeline{
		RunID: runID, RunsDir: runsDir, SynthDir: synthDir,
		Prompt:   "从中医医案文本判断证候",
		Provider: provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Model:    "jiuwei-tcm", MaxTokens: 256, SynthMaxTokens: 256,
		SamplesN: 4, ProbeVariants: 2, Workers: 1,
		Budget: budget, Mode: ModeAutopilot,
		OnEvent: func(ev eval.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
	}
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}
	if res.ExitCode != 0 || res.Status != core.StatusCompleted {
		t.Fatalf("result = %s/exit %d, want completed/0", res.Status, res.ExitCode)
	}
	if got := len(*bodies); got != 13 { // 3 synth + 2×4 probes + 2 baseline
		t.Errorf("llm calls = %d, want 13", got)
	}
	if means := res.MetricMeans["exact_match"]; means != 1 {
		t.Errorf("baseline exact_match mean = %v, want 1", means)
	}

	// Synth artifact tree.
	if _, err := LoadManifest(synthDir); err != nil {
		t.Errorf("manifest: %v", err)
	}
	specFile, err := LoadSpec(synthDir)
	if err != nil || len(specFile.Probes) != 2 {
		t.Errorf("spec.json = %+v (%v), want 2 probes", specFile, err)
	}
	sf, err := LoadSamples(synthDir)
	if err != nil || len(sf.Samples) != 4 {
		t.Fatalf("samples.json = %d samples (%v), want 4", len(sf.Samples), err)
	}
	raw, err := os.ReadFile(filepath.Join(synthDir, "samples.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"anchors"`) || !strings.Contains(string(raw), `"anchors": []`) {
		t.Errorf("samples.json must always serialize the anchors slot, got:\n%.400s", raw)
	}

	report, err := LoadFilterReport(synthDir)
	if err != nil {
		t.Fatalf("filter.json: %v", err)
	}
	if report.Variants != 2 || report.Primary != "exact_match" || report.Kept != 2 {
		t.Errorf("report header = %+v, want 2 variants, exact_match primary, 2 kept", report)
	}
	if report.Thresholds != DefaultThresholds() {
		t.Errorf("thresholds = %+v, want defaults", report.Thresholds)
	}
	wantVerdicts := []Verdict{VerdictDeadEasy, VerdictDeadHard, VerdictKeep, VerdictKeep}
	for i, pv := range report.PerSample {
		if pv.Verdict != wantVerdicts[i] || len(pv.Scores) != 2 {
			t.Errorf("per_sample[%d] = %+v, want verdict %s with 2 scores", i, pv, wantVerdicts[i])
		}
	}
	if report.DroppedByVerdict[VerdictDeadEasy] != 1 || report.DroppedByVerdict[VerdictDeadHard] != 1 {
		t.Errorf("dropped_by_verdict = %v", report.DroppedByVerdict)
	}

	cp, err := LoadCheckpoint(synthDir)
	if err != nil || cp.Status != CheckpointApproved || cp.Mode != ModeAutopilot {
		t.Errorf("checkpoint.json = %+v (%v), want approved/autopilot", cp, err)
	}

	// Baseline artifacts land under runs/<run_id>/ with only the kept
	// samples evaluated.
	samplesDir := filepath.Join(runsDir, runID, "samples")
	entries, err := os.ReadDir(samplesDir)
	if err != nil || len(entries) != 2 {
		t.Errorf("baseline sample traces = %v (%v), want 2", entries, err)
	}

	// Probe traces live per variant.
	for _, v := range []string{"v1", "v2"} {
		entries, err := os.ReadDir(filepath.Join(synthDir, "probes", v, "samples"))
		if err != nil || len(entries) != 4 {
			t.Errorf("probe %s sample traces = %v (%v), want 4", v, entries, err)
		}
	}

	// Budget accounting: optimizer usage snapshot, executor evals.
	evals, usage := budget.Snapshot()
	if evals != 10 { // 2 probes × 4 samples + 2 baseline
		t.Errorf("budget evals = %d, want 10", evals)
	}
	if u := usage[core.RoleOptimizer]; u.PromptTokens != 30 || u.CompletionTokens != 15 {
		t.Errorf("optimizer usage = %+v, want 30/15", u)
	}

	// Event stream: the three harness event types, in order.
	mu.Lock()
	defer mu.Unlock()
	var synthDone, filterDone, checkpoints []eval.Event
	for _, ev := range events {
		switch ev.Type {
		case EventSynthDone:
			synthDone = append(synthDone, ev)
		case EventFilterDone:
			filterDone = append(filterDone, ev)
		case EventCheckpoint:
			checkpoints = append(checkpoints, ev)
		}
	}
	if len(synthDone) != 1 || synthDone[0].Detail["samples"] != 4 || synthDone[0].Detail["probes"] != 2 {
		t.Errorf("synth_done events = %+v", synthDone)
	}
	if len(filterDone) != 1 || filterDone[0].Detail["kept"] != 2 || filterDone[0].Detail["dropped"] != 2 {
		t.Errorf("filter_done events = %+v", filterDone)
	}
	if len(checkpoints) != 2 || checkpoints[0].Detail["status"] != "pending" || checkpoints[1].Detail["status"] != "approved" {
		t.Errorf("checkpoint events = %+v, want pending then approved", checkpoints)
	}
}

// --- filter -----------------------------------------------------------------

func TestFilterUnmeasuredOnBudgetExhaustion(t *testing.T) {
	// budget-evals covers probe-1 only: probe-2 gets nothing and every
	// sample ends with incomplete evidence → unmeasured and kept, never
	// deleted because of provider/budget jitter.
	srv, _ := startLLM(t, func(_ int, body string) (int, string) {
		if strings.Contains(body, "恶寒发热") {
			return http.StatusOK, completion("风寒束表")
		}
		return http.StatusOK, completion("无法辨证")
	})
	synthDir := t.TempDir()
	f := &Filter{
		RunID: "run", SynthDir: synthDir, Model: "jiuwei-tcm", MaxTokens: 64, Workers: 1,
		Provider: provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Budget:   eval.NewBudget(0, 3),
	}
	spec := SpecFile{
		Task:   core.Task{Name: "t", PromptTemplate: "判断证候：{input}", Metrics: []string{"exact_match"}},
		Probes: []string{"变体甲：{input}", "变体乙：{input}"},
	}
	samples := []core.Sample{
		{ID: "s1", Input: "恶寒发热，无汗。", Expected: "风寒束表", Split: "train"},
		{ID: "s2", Input: "心烦不寐，腰膝酸软。", Expected: "心肾不交", Split: "dev"},
		{ID: "s3", Input: "五心烦热，舌红少苔。", Expected: "阴虚火旺", Split: "train"},
	}
	report, err := f.Apply(context.Background(), spec, samples, DefaultThresholds())
	if err != nil {
		t.Fatalf("Filter.Apply: %v", err)
	}
	if report.Kept != 3 || report.Variants != 2 {
		t.Errorf("report = kept %d variants %d, want 3/2", report.Kept, report.Variants)
	}
	for _, pv := range report.PerSample {
		if pv.Verdict != VerdictUnmeasured || len(pv.Scores) != 1 {
			t.Errorf("per_sample %s = %+v, want unmeasured with 1 recorded score", pv.ID, pv)
		}
	}
	if _, err := os.Stat(filepath.Join(synthDir, "probes", "v1")); err != nil {
		t.Errorf("probe v1 traces missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(synthDir, "probes", "v2")); err != nil {
		t.Errorf("probe v2 traces missing: %v", err)
	}
}

func TestFilterRejectsInvalidThresholds(t *testing.T) {
	f := &Filter{Budget: eval.NewBudget(0, 0)}
	if _, err := f.Apply(context.Background(), SpecFile{}, nil, Thresholds{Low: 0.9, High: 0.1}); err == nil {
		t.Error("invalid thresholds accepted")
	}
}

func TestSelectKept(t *testing.T) {
	report := &FilterReport{PerSample: []SampleVerdict{
		{ID: "s1", Verdict: VerdictKeep},
		{ID: "s2", Verdict: VerdictDeadEasy},
		{ID: "s3", Verdict: VerdictNoisy},
		{ID: "s4", Verdict: VerdictUnmeasured},
		{ID: "s5", Verdict: VerdictDeadHard},
	}}
	samples := []core.Sample{
		{ID: "s1"}, {ID: "s2"}, {ID: "s3"}, {ID: "s4"}, {ID: "s6"}, // s5 deleted, s6 added
	}
	kept := SelectKept(samples, report)
	got := make([]string, 0, len(kept))
	for _, s := range kept {
		got = append(got, s.ID)
	}
	if strings.Join(got, ",") != "s1,s4,s6" {
		t.Errorf("SelectKept = %v, want s1,s4,s6 (keep+unmeasured+new; dead/noisy dropped)", got)
	}
}

// --- checkpoint --------------------------------------------------------------

func eventRecorder() (func(eval.Event), func() []eval.Event) {
	var mu sync.Mutex
	var events []eval.Event
	return func(ev eval.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		}, func() []eval.Event {
			mu.Lock()
			defer mu.Unlock()
			return append([]eval.Event(nil), events...)
		}
}

func TestCheckpointAutopilot(t *testing.T) {
	dir := t.TempDir()
	record, snapshot := eventRecorder()
	c := &Checkpoint{Dir: dir, RunID: "r", OnEvent: record}
	if err := c.Gate(context.Background(), ModeAutopilot); err != nil {
		t.Fatalf("Gate: %v", err)
	}
	st, err := LoadCheckpoint(dir)
	if err != nil || st.Status != CheckpointApproved || st.Mode != ModeAutopilot {
		t.Errorf("checkpoint = %+v (%v), want approved/autopilot", st, err)
	}
	evs := snapshot()
	if len(evs) != 2 || evs[0].Detail["status"] != "pending" || evs[1].Detail["status"] != "approved" {
		t.Errorf("events = %+v, want pending then approved", evs)
	}
}

func TestCheckpointInteractiveWaitsForApproval(t *testing.T) {
	dir := t.TempDir()
	record, snapshot := eventRecorder()
	c := &Checkpoint{Dir: dir, RunID: "r", Interval: 5 * time.Millisecond, OnEvent: record}
	done := make(chan error, 1)
	go func() { done <- c.Gate(context.Background(), ModeInteractive) }()

	// Wait for the pending artifact, then corrupt it first: parse
	// failures must count as unresolved, not approved.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "checkpoint.json")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("checkpoint.json never appeared")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("gate returned early on a corrupt artifact: %v", err)
	default:
	}
	if err := SaveCheckpoint(dir, CheckpointState{Status: CheckpointApproved, Mode: ModeInteractive, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Gate: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gate never observed the manual approval")
	}
	if st, err := LoadCheckpoint(dir); err != nil || st.Status != CheckpointApproved {
		t.Errorf("checkpoint = %+v (%v), want approved", st, err)
	}
	last := snapshot()
	if last[len(last)-1].Detail["status"] != "approved" {
		t.Errorf("last checkpoint event = %+v, want approved", last[len(last)-1])
	}
}

func TestCheckpointInteractiveCtxAbort(t *testing.T) {
	dir := t.TempDir()
	c := &Checkpoint{Dir: dir, RunID: "r", Interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Gate(ctx, ModeInteractive) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "checkpoint.json")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("checkpoint.json never appeared")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ctx cancellation must abort the interactive gate")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gate did not abort after cancellation")
	}
}

// --- pipeline checkpoint integration ----------------------------------------

func TestPipelineInteractiveGateProceedsAfterApproval(t *testing.T) {
	dir := t.TempDir()
	runsDir := filepath.Join(dir, "runs")
	runID := "20260929-091000-i"
	synthDir := filepath.Join(filepath.Join(dir, "synth"), runID)
	evalBody := func(body string) string {
		for _, sc := range pipelineScript {
			if !strings.Contains(body, sc.input) {
				continue
			}
			score := 1 // baseline always answers correctly
			switch {
			case strings.Contains(body, "变体甲"):
				score = sc.p1
			case strings.Contains(body, "变体乙"):
				score = sc.p2
			}
			if score == 1 {
				return sc.expected
			}
			return "无法辨证"
		}
		return "{}"
	}
	srv, _ := startLLM(t, scriptRouter(t,
		func() string { return specJSON },
		func() string { return samplesJSON },
		func() string { return probesJSON },
		evalBody,
	))
	p := &Pipeline{
		RunID: runID, RunsDir: runsDir, SynthDir: synthDir,
		Prompt:   "从中医医案文本判断证候",
		Provider: provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Model:    "jiuwei-tcm", MaxTokens: 256, SynthMaxTokens: 256,
		SamplesN: 4, ProbeVariants: 2, Workers: 1,
		Budget: eval.NewBudget(0, 0), Mode: ModeInteractive,
	}
	go func() {
		// Approve via the artifact once the gate has written pending.
		cpPath := filepath.Join(synthDir, "checkpoint.json")
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(cpPath); err == nil {
				_ = SaveCheckpoint(synthDir, CheckpointState{Status: CheckpointApproved, Mode: ModeInteractive, UpdatedAt: time.Now()})
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
}

func TestPipelineInteractiveGateAbortsOnCtxCancel(t *testing.T) {
	dir := t.TempDir()
	runID := "20260929-092000-c"
	synthDir := filepath.Join(filepath.Join(dir, "synth"), runID)
	srv, _ := startLLM(t, scriptRouter(t,
		func() string { return specJSON },
		func() string { return samplesJSON },
		func() string { return probesJSON },
		func(string) string { return "风寒束表" },
	))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &Pipeline{
		RunID: runID, RunsDir: filepath.Join(dir, "runs"), SynthDir: synthDir,
		Prompt:   "从中医医案文本判断证候",
		Provider: provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Model:    "jiuwei-tcm", MaxTokens: 256, SynthMaxTokens: 256,
		SamplesN: 4, ProbeVariants: 2, Workers: 1,
		Budget: eval.NewBudget(0, 0), Mode: ModeInteractive,
	}
	go func() {
		cpPath := filepath.Join(synthDir, "checkpoint.json")
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(cpPath); err == nil {
				cancel()
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	if _, err := p.Run(ctx); err == nil {
		t.Fatal("canceled interactive gate must fail the pipeline")
	}
}

// TestPipelineReloadedSpecMustValidate pins the reload defense: a
// spec.json hand edit during the checkpoint pause that injects an
// unknown metric (or drops {input}) must fail the pipeline instead of
// driving a silent all-zero baseline with exit 0.
func TestPipelineReloadedSpecMustValidate(t *testing.T) {
	dir := t.TempDir()
	runID := "20260929-093000-v"
	synthDir := filepath.Join(filepath.Join(dir, "synth"), runID)
	srv, _ := startLLM(t, scriptRouter(t,
		func() string { return specJSON },
		func() string { return samplesJSON },
		func() string { return probesJSON },
		func(string) string { return "风寒束表" },
	))
	p := &Pipeline{
		RunID: runID, RunsDir: filepath.Join(dir, "runs"), SynthDir: synthDir,
		Prompt:   "从中医医案文本判断证候",
		Provider: provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Model:    "jiuwei-tcm", MaxTokens: 256, SynthMaxTokens: 256,
		SamplesN: 4, ProbeVariants: 2, Workers: 1,
		Budget: eval.NewBudget(0, 0), Mode: ModeInteractive,
	}
	go func() {
		cpPath := filepath.Join(synthDir, "checkpoint.json")
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(cpPath); err == nil {
				_ = SaveSpec(synthDir, SpecFile{
					Task: core.Task{
						Name: "t", Description: "d",
						PromptTemplate: "判断证候：{input}", Metrics: []string{"made_up_metric"},
					},
					Probes: []string{"a {input}", "b {input}"},
				})
				_ = SaveCheckpoint(synthDir, CheckpointState{
					Status: CheckpointApproved, Mode: ModeInteractive, UpdatedAt: time.Now(),
				})
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("invalid hand-edited spec must fail the pipeline (exit 1), not run an all-zero baseline")
	}
}

// --- artifacts ---------------------------------------------------------------

// TestExtractJSONPreservesStringContent is the regression for the
// regex-based comma cleaner, which rewrote ", }" inside JSON string
// values into still-valid JSON ("a, }" became "a}"), silently
// polluting synthesized inputs/expecteds downstream.
func TestExtractJSONPreservesStringContent(t *testing.T) {
	valid := `{"samples":[{"id":"s1","input":"…如 a, } 与 b, ]","expected":"风寒束表","split":"train"}]}`
	if got, ok := extractJSON(valid); !ok || got != valid {
		t.Errorf("valid JSON was rewritten:\n got  %s\n want %s", got, valid)
	}

	broken := `{"samples":[{"id":"s1","input":"…如 a, } 与 b, ]","expected":"风寒束表","split":"train"},]}`
	want := `{"samples":[{"id":"s1","input":"…如 a, } 与 b, ]","expected":"风寒束表","split":"train"}]}`
	got, ok := extractJSON(broken)
	if !ok || got != want {
		t.Errorf("repair corrupted string content:\n got  %s\n want %s", got, want)
	}
	var v any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Errorf("repaired payload does not parse: %v", err)
	}

	// tryParse passes already-valid JSON through byte-for-byte and
	// still repairs the broken one, keeping the string content.
	type payload struct {
		Samples []core.Sample `json:"samples"`
	}
	p, ok := tryParse[payload](valid)
	if !ok || len(p.Samples) != 1 || p.Samples[0].Input != "…如 a, } 与 b, ]" {
		t.Errorf("tryParse(valid) = %+v ok=%v", p, ok)
	}
	p, ok = tryParse[payload](broken)
	if !ok || len(p.Samples) != 1 || p.Samples[0].Input != "…如 a, } 与 b, ]" {
		t.Errorf("tryParse(broken) = %+v ok=%v", p, ok)
	}
}

// TestSaveJSONConcurrentWriters pins the unique-temp-file contract:
// concurrent writers of one artifact path must all succeed (the fixed
// ".tmp" name made renames fail with ENOENT and could publish
// truncated content).
func TestSaveJSONConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact.json")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if err := SaveJSON(path, map[string]int{"n": 1}); err != nil {
					t.Errorf("SaveJSON: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	var dst map[string]int
	if err := LoadJSON(path, &dst); err != nil {
		t.Fatalf("final artifact unreadable: %v", err)
	}
}

func TestArtifactRoundTripKeepsAnchorsSlot(t *testing.T) {
	dir := t.TempDir()
	f := SampleFile{Samples: []core.Sample{
		{ID: "s1", Input: "恶寒发热", Expected: "风寒束表", Split: "train"},
		{ID: "s2", Input: "心烦不寐", Expected: map[string]any{"证候": "心肾不交"}, Split: "dev"},
	}}
	if err := SaveSamples(dir, f); err != nil {
		t.Fatalf("SaveSamples: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "samples.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"anchors": []`) {
		t.Errorf("anchors slot must always serialize, got:\n%s", raw)
	}
	got, err := LoadSamples(dir)
	if err != nil {
		t.Fatalf("LoadSamples: %v", err)
	}
	if len(got.Samples) != 2 || got.Samples[1].ID != "s2" || len(got.Anchors) != 0 {
		t.Errorf("round trip = %+v", got)
	}

	// Invalid sets are rejected on both save and load.
	if err := SaveSamples(dir, SampleFile{Samples: []core.Sample{{ID: "", Input: "x", Expected: "y", Split: "train"}}}); err == nil {
		t.Error("sample without id accepted")
	}
	bad := filepath.Join(dir, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "samples.json"), []byte(`{"samples":[{"id":"s1","input":"x","expected":"y","split":"moon"}],"anchors":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSamples(bad); err == nil {
		t.Error("invalid split accepted on load")
	}
}

func TestSpecAndFilterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	spec := SpecFile{Task: mustSpec(t), Probes: []string{"a {input}", "b {input}"}}
	if err := SaveSpec(dir, spec); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSpec(dir)
	if err != nil || got.Task.Name != spec.Task.Name || len(got.Probes) != 2 {
		t.Errorf("spec round trip = %+v (%v)", got, err)
	}
	report := FilterReport{Variants: 2, Primary: "exact_match", Thresholds: DefaultThresholds(),
		PerSample: []SampleVerdict{{ID: "s1", Scores: []float64{0, 1}, Variance: 0.25, Verdict: VerdictKeep}},
		Kept:      1, DroppedByVerdict: map[Verdict]int{VerdictDeadEasy: 2}}
	if err := SaveFilterReport(dir, report); err != nil {
		t.Fatal(err)
	}
	rgot, err := LoadFilterReport(dir)
	if err != nil || rgot.Kept != 1 || rgot.DroppedByVerdict[VerdictDeadEasy] != 2 {
		t.Errorf("filter round trip = %+v (%v)", rgot, err)
	}
}

// --- helpers -----------------------------------------------------------------

func mustSpec(t *testing.T) core.Task {
	t.Helper()
	var task core.Task
	if err := json.Unmarshal([]byte(specJSON), &task); err != nil {
		t.Fatal(err)
	}
	if err := task.Validate(); err != nil {
		t.Fatal(err)
	}
	return task
}
