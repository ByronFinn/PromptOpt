package eval

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- parseJudge golden cases ----------------------------------------------

func TestParseJudgeGoldenCases(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantScore float64
		wantDiag  string
		wantErr   bool
	}{
		{"legal verdict", `{"score": 0.75, "diagnosis": "部分正确：少抽一个实体"}`, 0.75, "部分正确：少抽一个实体", false},
		{"fenced verdict", "```json\n{\"score\": 0.5, \"diagnosis\": \"一半正确\"}\n```", 0.5, "一半正确", false},
		{"high clamp to 1", `{"score": 1.5, "diagnosis": "过界"}`, 1, "过界", false},
		{"low clamp to 0", `{"score": -0.2, "diagnosis": "负分"}`, 0, "负分", false},
		{"missing diagnosis", `{"score": 1}`, 1, "", false},
		{"missing score reads as zero", `{"diagnosis": "裁判忘给分"}`, 0, "裁判忘给分", false},
		{"extra fields ignored", `{"score": 0.5, "diagnosis": "d", "reason": "x"}`, 0.5, "d", false},
		{"prose is not a verdict", "这个输出看起来还行", 0, "", true},
		{"empty verdict", "", 0, "", true},
	}
	for _, tc := range cases {
		mr, err := parseJudge(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: parseJudge = %+v, want error", tc.name, mr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: parseJudge: %v", tc.name, err)
			continue
		}
		if mr.Score != tc.wantScore || mr.Diagnosis != tc.wantDiag {
			t.Errorf("%s: parseJudge = %+v, want score %v diag %q", tc.name, mr, tc.wantScore, tc.wantDiag)
		}
	}
}

func TestJudgePromptCarriesSampleEvidence(t *testing.T) {
	s := core.Sample{ID: "s-1", Input: "患者恶寒发热无汗", Expected: map[string]any{"ok": true}, Split: "test"}
	p := judgePrompt(s, `{"ok": false}`)
	for _, want := range []string{"评估裁判", s.Input, `{"ok":true}`, `{"ok": false}`, "diagnosis"} {
		if !strings.Contains(p, want) {
			t.Errorf("judge prompt missing %q:\n%s", want, p)
		}
	}
}

// --- engine integration (httptest-backed, all LLM calls mocked) ------------

// Distinct per-role spends so assertions can prove the merge: executor
// 10/5, judge 7/3 → sample total 17/8.
var (
	execSpend  = core.Usage{PromptTokens: 10, CompletionTokens: 5}
	judgeSpend = core.Usage{PromptTokens: 7, CompletionTokens: 3}
)

// judgeRespond builds a body-keyed fake LLM: requests carrying the
// judge rubric get the judge answer, everything else the executor
// answer. The sleeps keep executor and judge latencies apart and
// nonzero so DurationMS/JudgeMS semantics are observable.
func judgeRespond(judge, exec func() (int, string)) func(int, string) (int, string) {
	return func(_ int, body string) (int, string) {
		if strings.Contains(body, "评估裁判") {
			time.Sleep(3 * time.Millisecond)
			return judge()
		}
		time.Sleep(2 * time.Millisecond)
		return exec()
	}
}

func goodExec() (int, string) {
	return http.StatusOK, completionJSON(`{"ok": true}`, execSpend)
}

type judgeEngineOpts struct {
	workers      int
	metrics      []string // defaults to [llm_judge]
	budgetTokens int64
	temperature  float64 // executor-side temperature; the judge must stay at 0 regardless
	respond      func(int, string) (int, string) // defaults to a 0.75 verdict
}

// runJudgeEngine evaluates samples with a custom metric list against a
// body-keyed fake LLM and returns the result, recorded request bodies,
// emitted events and the run dir.
func runJudgeEngine(t *testing.T, o judgeEngineOpts, samples []core.Sample) (core.RunResult, []string, []Event, string) {
	t.Helper()
	respond := o.respond
	if respond == nil {
		respond = judgeRespond(
			func() (int, string) {
				return http.StatusOK, completionJSON(`{"score": 0.75, "diagnosis": "部分正确：少抽一个实体"}`, judgeSpend)
			},
			goodExec,
		)
	}
	metrics := o.metrics
	if metrics == nil {
		metrics = []string{MetricLLMJudge}
	}
	srv, bodies := startLLM(t, respond)
	client := provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})
	dir := t.TempDir()

	var mu sync.Mutex
	var events []Event
	e := &Engine{
		RunID: "judge-run", RunDir: dir, Model: "fake-model", MaxTokens: 64,
		Workers: max(o.workers, 1), Metrics: metrics,
		Budget: NewBudget(o.budgetTokens, 0), Provider: client,
		Temperature: o.temperature,
		TaskName:    "task", CandidateID: "cand", DatasetName: "ds",
		OnEvent: func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
	}
	res, err := e.Run(context.Background(), core.Candidate{ID: "cand", Prompt: "抽取实体：{input}"}, samples)
	if err != nil {
		t.Fatalf("Engine.Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return res, *bodies, events, dir
}

// callTraces decodes every trace under calls/.
func callTraces(t *testing.T, dir string) []CallTrace {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatalf("read calls dir: %v", err)
	}
	out := make([]CallTrace, 0, len(entries))
	for _, en := range entries {
		b, err := os.ReadFile(filepath.Join(dir, "calls", en.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var ct CallTrace
		if err := json.Unmarshal(b, &ct); err != nil {
			t.Fatalf("decode %s: %v", en.Name(), err)
		}
		out = append(out, ct)
	}
	return out
}

func TestEngineLLMJudgeScoresWithChineseDiagnosis(t *testing.T) {
	const diagnosis = "部分正确：少抽一个实体"
	res, _, events, dir := runJudgeEngine(t, judgeEngineOpts{}, testSamples(1))

	if res.ExitCode != 0 || res.Status != core.StatusCompleted || res.Evaluated != 1 {
		t.Fatalf("result = %s/exit %d, want completed/0 with 1 evaluated", res.Status, res.ExitCode)
	}
	if res.MetricMeans[MetricLLMJudge] != 0.75 {
		t.Errorf("llm_judge mean = %v, want 0.75", res.MetricMeans[MetricLLMJudge])
	}
	// Budget snapshot: the judge's usage is metered under its own
	// RoleJudge key (visible separately), while the sample-level totals
	// below keep merging executor+judge.
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 10 || u.CompletionTokens != 5 {
		t.Errorf("executor usage = %+v, want 10/5", u)
	}
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 7 || u.CompletionTokens != 3 {
		t.Errorf("judge usage = %+v, want 7/3 under RoleJudge", u)
	}

	// ASI chain: the Chinese diagnosis rides the sample_done event.
	var done *Event
	for i := range events {
		if events[i].Type == EventSampleDone {
			done = &events[i]
		}
	}
	if done == nil {
		t.Fatal("sample_done event missing")
	}
	if done.Diagnosis[MetricLLMJudge] != diagnosis {
		t.Errorf("event diagnosis = %v, want %q", done.Diagnosis, diagnosis)
	}
	if done.Scores[MetricLLMJudge] != 0.75 {
		t.Errorf("event score = %v", done.Scores)
	}
	if done.Usage == nil || done.Usage.PromptTokens != 17 || done.Usage.CompletionTokens != 8 {
		t.Errorf("event usage = %+v, want 17/8 including judge tokens", done.Usage)
	}

	// Call traces: one executor, one judge with Stage set; the judge
	// request carries the rubric plus the candidate output and the
	// reference answer.
	traces := callTraces(t, dir)
	if len(traces) != 2 {
		t.Fatalf("call traces = %d, want 2 (executor + judge)", len(traces))
	}
	var execTrace, judgeTrace *CallTrace
	for i := range traces {
		if traces[i].Stage == "judge" {
			judgeTrace = &traces[i]
		} else {
			execTrace = &traces[i]
		}
	}
	if judgeTrace == nil || execTrace == nil {
		t.Fatalf("trace split failed: %+v", traces)
	}
	if execTrace.Stage != "" {
		t.Errorf("executor trace stage = %q, want unset", execTrace.Stage)
	}
	if judgeTrace.Role != core.RoleJudge || judgeTrace.SampleID != "s-00" {
		t.Errorf("judge trace header = %+v", judgeTrace)
	}
	judgePromptSent := judgeTrace.Request.Messages[0].Content
	for _, want := range []string{`{"ok": true}`, `{"ok":true}`, "中医输入文本编号0", "评估裁判"} {
		if !strings.Contains(judgePromptSent, want) {
			t.Errorf("judge prompt missing %q", want)
		}
	}
	if judgeTrace.Request.Model != "fake-model" || judgeTrace.Request.MaxTokens != 64 {
		t.Errorf("judge request meta = %+v", judgeTrace.Request)
	}

	// Sample trace: usage merged, DurationMS keeps the executor latency
	// and JudgeMS mirrors the judge trace's latency exactly.
	b, err := os.ReadFile(filepath.Join(dir, "samples", "001-s-00.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st core.SampleTrace
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.Diagnosis[MetricLLMJudge] != diagnosis || st.Scores[MetricLLMJudge] != 0.75 {
		t.Errorf("sample trace scores/diagnosis = %v / %v", st.Scores, st.Diagnosis)
	}
	if st.Usage.PromptTokens != 17 || st.Usage.CompletionTokens != 8 {
		t.Errorf("sample trace usage = %+v, want 17/8", st.Usage)
	}
	if st.JudgeMS <= 0 || st.JudgeMS != judgeTrace.LatencyMS {
		t.Errorf("sample trace judge_ms = %d, want > 0 and == judge trace latency %d", st.JudgeMS, judgeTrace.LatencyMS)
	}
	if st.DurationMS <= 0 || st.DurationMS != execTrace.LatencyMS {
		t.Errorf("sample trace duration_ms = %d, want > 0 and == executor trace latency %d", st.DurationMS, execTrace.LatencyMS)
	}
	if done.JudgeMS != st.JudgeMS {
		t.Errorf("event judge_ms = %d, want sample trace value %d", done.JudgeMS, st.JudgeMS)
	}
}

func TestEngineMixedMetricsWithJudge(t *testing.T) {
	res, bodies, _, _ := runJudgeEngine(t, judgeEngineOpts{
		metrics: []string{"json_validator", MetricLLMJudge},
	}, testSamples(2))

	if res.ExitCode != 0 {
		t.Fatalf("result = %+v, want exit 0", res)
	}
	if res.MetricMeans["json_validator"] != 1 || res.MetricMeans[MetricLLMJudge] != 0.75 {
		t.Errorf("means = %+v, want json_validator 1 and llm_judge 0.75", res.MetricMeans)
	}
	if len(bodies) != 4 {
		t.Errorf("llm calls = %d, want 4 (2 executor + 2 judge)", len(bodies))
	}
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 20 || u.CompletionTokens != 10 {
		t.Errorf("executor usage = %+v, want 20/10 (2×10/5)", u)
	}
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 14 || u.CompletionTokens != 6 {
		t.Errorf("judge usage = %+v, want 14/6 (2×7/3) under RoleJudge", u)
	}
}

func TestEngineLLMJudgeBadVerdictFailsSample(t *testing.T) {
	// A judge answer that is not JSON degrades loudly: the sample fails
	// (incomplete evidence) while its tokens stay metered and traced.
	res, _, events, dir := runJudgeEngine(t, judgeEngineOpts{
		respond: judgeRespond(
			func() (int, string) {
				return http.StatusOK, completionJSON("这个输出看起来还行，但我不想给分", judgeSpend)
			},
			goodExec,
		),
	}, testSamples(1))

	if res.ExitCode != 1 || res.Status != core.StatusFailed {
		t.Fatalf("result = %s/exit %d, want failed/1", res.Status, res.ExitCode)
	}
	if len(res.FailedSamples) != 1 || res.FailedSamples[0] != "s-00" {
		t.Errorf("failed samples = %v", res.FailedSamples)
	}
	if len(res.MetricMeans) != 0 {
		t.Errorf("means = %+v, want none (failed samples never score)", res.MetricMeans)
	}
	// Tokens were spent either way: judge usage still counts, under its
	// own role key.
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 10 || u.CompletionTokens != 5 {
		t.Errorf("executor usage = %+v, want 10/5", u)
	}
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 7 || u.CompletionTokens != 3 {
		t.Errorf("judge usage = %+v, want 7/3 including the unusable judge call", u)
	}
	for _, ev := range events {
		if ev.Type == EventSampleDone {
			if !strings.Contains(ev.Error, "llm_judge") {
				t.Errorf("event error = %q, want llm_judge attribution", ev.Error)
			}
			if ev.JudgeMS <= 0 {
				t.Errorf("event judge_ms = %d, want the spent judge latency", ev.JudgeMS)
			}
		}
	}
	var judgeTrace *CallTrace
	for _, ct := range callTraces(t, dir) {
		if ct.Stage == "judge" {
			judgeTrace = &ct
		}
	}
	if judgeTrace == nil || judgeTrace.Response.Content == "" {
		t.Fatalf("judge trace missing or empty: %+v", judgeTrace)
	}
	var st core.SampleTrace
	b, err := os.ReadFile(filepath.Join(dir, "samples", "001-s-00.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Error, "llm_judge") {
		t.Errorf("sample trace error = %q, want llm_judge attribution", st.Error)
	}
	if st.Usage.PromptTokens != 17 || st.Usage.CompletionTokens != 8 {
		t.Errorf("sample trace usage = %+v, want 17/8", st.Usage)
	}
}

func TestEngineLLMJudgeProviderFailureFailsSample(t *testing.T) {
	res, _, _, _ := runJudgeEngine(t, judgeEngineOpts{
		respond: judgeRespond(
			func() (int, string) { return http.StatusInternalServerError, `{"error": "judge down"}` },
			goodExec,
		),
	}, testSamples(1))

	if res.ExitCode != 1 || len(res.FailedSamples) != 1 {
		t.Fatalf("result = %+v, want failed/1", res)
	}
	// The judge call never answered, so only the executor spend exists.
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 10 || u.CompletionTokens != 5 {
		t.Errorf("executor usage = %+v, want 10/5 (executor only)", u)
	}
}

// TestEngineLLMJudgeUsageArmsExecutorSoftStop pins the budget decision
// of the judge isolation (roadmap-v7 §2.1): judge usage is metered
// under its own RoleJudge key, and that key arms the same token soft
// stop the executor historically shared — total evaluation cost stays
// one valve. Sample 0 spends 60(exec)+60(judge)=120 over the 100-token
// limit, sample 1 was already handed off (the post-hoc soft-stop
// semantics of the dispatch loop), sample 2 stays undispatched: exit
// code 2 semantics unchanged.
func TestEngineLLMJudgeUsageArmsExecutorSoftStop(t *testing.T) {
	// Judge usage is metered under its own role key...
	res, bodies, events, _ := runJudgeEngine(t, judgeEngineOpts{
		workers:      1,
		budgetTokens: 100,
		respond: judgeRespond(
			func() (int, string) {
				return http.StatusOK, completionJSON(`{"score": 1, "diagnosis": "完全正确"}`, core.Usage{PromptTokens: 60})
			},
			func() (int, string) {
				return http.StatusOK, completionJSON(`{"ok": true}`, core.Usage{PromptTokens: 60})
			},
		),
	}, testSamples(3))

	if res.ExitCode != 2 || res.Status != core.StatusBudgetExhausted {
		t.Errorf("result = %s/exit %d, want budget_exhausted/2", res.Status, res.ExitCode)
	}
	if res.Undispatched != 1 || res.Evaluated != 2 {
		t.Errorf("undispatched = %d, evaluated = %d, want 1/2", res.Undispatched, res.Evaluated)
	}
	if len(bodies) != 4 {
		t.Errorf("llm calls = %d, want 4 (2 samples × executor+judge)", len(bodies))
	}
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 120 {
		t.Errorf("executor usage = %+v, want 120 (2×60)", u)
	}
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 120 {
		t.Errorf("judge usage = %+v, want 120 (2×60) under RoleJudge", u)
	}
	sawBudgetStop := false
	for _, ev := range events {
		if ev.Type == EventBudgetStop {
			sawBudgetStop = true
		}
	}
	if !sawBudgetStop {
		t.Error("budget_stop event missing (judge spend must arm the soft stop)")
	}
}

// TestJudgeRequestTemperatureStaysZero（用例 ⑦）pins judge.go:88: the
// judge request keeps its hardcoded deterministic 0 even when the
// executor samples hot — the executor's temperature never leaks into
// the judge call, so the judge's request body carries no temperature
// field at all (0 is omitempty-ed off the wire).
func TestJudgeRequestTemperatureStaysZero(t *testing.T) {
	_, bodies, _, _ := runJudgeEngine(t, judgeEngineOpts{temperature: 0.9}, testSamples(1))
	judgeCalls, execCalls := 0, 0
	for _, b := range bodies {
		if strings.Contains(b, "评估裁判") {
			judgeCalls++
			if strings.Contains(b, "temperature") {
				t.Errorf("judge request carries a temperature field: %.200s", b)
			}
			continue
		}
		execCalls++
		if !strings.Contains(b, `"temperature":0.9`) {
			t.Errorf("executor request lost the hot temperature: %.200s", b)
		}
	}
	if judgeCalls != 1 || execCalls != 1 {
		t.Fatalf("calls = %d judge / %d executor, want 1/1", judgeCalls, execCalls)
	}
}

func TestLoadTaskAcceptsLLMJudgeMetric(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.yaml")
	doc := `name: judged-task
prompt_template: "抽取实体：{input}"
metrics: [llm_judge, exact_match]
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := core.LoadTask(path)
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}
	if task.Primary() != MetricLLMJudge {
		t.Errorf("primary = %q, want llm_judge (first declared)", task.Primary())
	}
	if len(task.Metrics) != 2 || task.Metrics[0] != MetricLLMJudge || task.Metrics[1] != "exact_match" {
		t.Errorf("metrics = %v", task.Metrics)
	}
}
