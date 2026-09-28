package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cmp"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- metric golden cases -------------------------------------------------

func TestJSONValidatorGoldenCases(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   float64
	}{
		{"bare object", `{"entities": []}`, 1},
		{"fenced object", "```json\n{\"a\": 1}\n```", 1},
		{"fenced no language", "```\n{\"a\": 1}\n```", 1},
		{"text before fence", "结果如下：\n```json\n{\"a\": 1}\n```", 0},
		{"text after fence", "```json\n{\"a\": 1}\n```\n以上就是结果", 0},
		{"prose only", "无法抽取实体", 0},
		{"empty", "", 0},
	}
	for _, tc := range cases {
		if got := Evaluate("json_validator", tc.output, "ignored"); got.Score != tc.want {
			t.Errorf("%s: score = %v, want %v (diagnosis %q)", tc.name, got.Score, tc.want, got.Diagnosis)
		}
	}
}

func TestExactMatchGoldenCases(t *testing.T) {
	cases := []struct {
		name     string
		output   string
		expected any
		want     float64
	}{
		{"string after trim", "  风寒束表 \n", "风寒束表", 1},
		{"string case sensitive", "Wind", "wind", 0},
		{"object key order", `{"a": 1, "b": 2}`, map[string]any{"b": 2, "a": 1}, 1},
		{"number format", `{"a": 1.0}`, map[string]any{"a": 1}, 1},
		{"nested array order matters", `{"a": [1, 2]}`, map[string]any{"a": []any{2, 1}}, 0},
		{"fenced object", "```json\n{\"a\": 1}\n```", map[string]any{"a": 1}, 1},
		{"array", `[1, "x"]`, []any{1, "x"}, 1},
		{"model output not json", "oops", map[string]any{"a": 1}, 0},
	}
	for _, tc := range cases {
		if got := Evaluate("exact_match", tc.output, tc.expected); got.Score != tc.want {
			t.Errorf("%s: score = %v, want %v (diagnosis %q)", tc.name, got.Score, tc.want, got.Diagnosis)
		}
	}
}

func TestF1GoldenCases(t *testing.T) {
	// Chinese sentence: partial overlap must land strictly in (0, 1).
	got := Evaluate("f1", "风寒束表", "风寒束表 治以辛温解表")
	if got.Score <= 0 || got.Score >= 1 {
		t.Errorf("chinese f1 = %v, want strictly inside (0,1)", got.Score)
	}
	if want := 2.0 * (0.4) / 1.4; math.Abs(got.Score-want) > 1e-9 {
		t.Errorf("chinese f1 = %v, want %v", got.Score, want)
	}

	// Object leaf multiset: one extra entity drops F1 to 0.8.
	expected := map[string]any{"entities": []any{
		map[string]any{"type": "证候", "text": "风寒束表"},
		map[string]any{"type": "治法", "text": "辛温解表"},
	}}
	output := `{"entities": [
		{"type": "证候", "text": "风寒束表"},
		{"type": "治法", "text": "辛温解表"},
		{"type": "中药", "text": "石膏"}]}`
	got = Evaluate("f1", output, expected)
	if math.Abs(got.Score-0.8) > 1e-9 {
		t.Errorf("object leaf f1 = %v, want 0.8 (diagnosis %q)", got.Score, got.Diagnosis)
	}

	// Array elements as multiset: [a,b,b] vs [a,b,c] -> 2/3.
	got = Evaluate("f1", `["a","b","b"]`, []any{"a", "b", "c"})
	if math.Abs(got.Score-2.0/3.0) > 1e-9 {
		t.Errorf("array f1 = %v, want 2/3", got.Score)
	}

	// Identical strings score 1; empty vs empty too.
	if got := Evaluate("f1", "同 一 段", "同 一 段"); got.Score != 1 {
		t.Errorf("identical f1 = %v, want 1", got.Score)
	}
	if got := Evaluate("f1", "", ""); got.Score != 1 {
		t.Errorf("empty f1 = %v, want 1", got.Score)
	}

	// Model side parse failure scores 0 with a diagnosis.
	got = Evaluate("f1", "not json at all", expected)
	if got.Score != 0 || got.Diagnosis == "" {
		t.Errorf("parse failure f1 = %+v, want 0 with diagnosis", got)
	}
}

func TestTokenizeSplitsCJK(t *testing.T) {
	toks := tokenize("风寒束表 fever 治法")
	want := []string{"风", "寒", "束", "表", "fever", "治", "法"}
	if strings.Join(toks, "|") != strings.Join(want, "|") {
		t.Errorf("tokenize = %v, want %v", toks, want)
	}
}

// --- budget ---------------------------------------------------------------

func TestBudgetEvalLimit(t *testing.T) {
	b := NewBudget(0, 2)
	if !b.TryAcquireEval() || !b.TryAcquireEval() {
		t.Fatal("first two acquires must succeed")
	}
	if b.TryAcquireEval() {
		t.Fatal("third acquire must be denied")
	}
}

func TestBudgetTokenSoftStop(t *testing.T) {
	b := NewBudget(100, 0)
	b.RecordUsage(core.RoleExecutor, core.Usage{PromptTokens: 60})
	if b.SoftStopped() {
		t.Fatal("60/100 must not stop")
	}
	b.RecordUsage(core.RoleExecutor, core.Usage{PromptTokens: 40})
	if b.SoftStopped() {
		t.Fatal("used == limit must not stop (100/100)")
	}
	b.RecordUsage(core.RoleExecutor, core.Usage{PromptTokens: 1})
	if !b.SoftStopped() {
		t.Fatal("101/100 must arm the soft stop")
	}
	if b.TryAcquireEval() {
		t.Fatal("soft stop must deny further dispatch")
	}
}

func TestBudgetIgnoresOptimizerRoleForTokenLimit(t *testing.T) {
	b := NewBudget(10, 0)
	b.RecordUsage(core.RoleOptimizer, core.Usage{PromptTokens: 1000})
	if b.SoftStopped() {
		t.Fatal("optimizer usage must not count against the executor budget")
	}
	_, usage := b.Snapshot()
	if usage[core.RoleOptimizer].PromptTokens != 1000 {
		t.Errorf("optimizer usage = %+v", usage[core.RoleOptimizer])
	}
}

func TestBudgetConcurrentAccounting(t *testing.T) {
	b := NewBudget(0, 50)
	var wg sync.WaitGroup
	acquired := make(chan struct{}, 200)
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.TryAcquireEval() {
				acquired <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(acquired)
	n := len(acquired)
	if n != 50 {
		t.Errorf("acquired = %d, want exactly 50", n)
	}

	var wgr sync.WaitGroup
	for range 100 {
		wgr.Add(1)
		go func() {
			defer wgr.Done()
			b.RecordUsage(core.RoleExecutor, core.Usage{PromptTokens: 1, CompletionTokens: 2})
		}()
	}
	wgr.Wait()
	_, usage := b.Snapshot()
	if usage[core.RoleExecutor].PromptTokens != 100 || usage[core.RoleExecutor].CompletionTokens != 200 {
		t.Errorf("usage = %+v, want 100/200 (no lost updates)", usage[core.RoleExecutor])
	}
}

// --- engine (httptest-backed end to end) ----------------------------------

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

func completionJSON(content string, u core.Usage) string {
	return completionFull("", content, "stop", u)
}

// completionFull builds a response with explicit reasoning content and
// finish reason.
func completionFull(reasoning, content, finishReason string, u core.Usage) string {
	return fmt.Sprintf(`{"choices":[{"finish_reason":%q,"message":{"role":"assistant","reasoning_content":%q,"content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		finishReason, reasoning, content, u.PromptTokens, u.CompletionTokens, u.Total())
}

func testSamples(n int) []core.Sample {
	out := make([]core.Sample, n)
	for i := range n {
		out[i] = core.Sample{
			ID:       fmt.Sprintf("s-%02d", i),
			Input:    fmt.Sprintf("中医输入文本编号%d", i),
			Expected: map[string]any{"ok": true},
			Split:    "test",
		}
	}
	return out
}

type engineOpts struct {
	workers      int
	budgetTokens int64
	budgetEvals  int64
	usage        core.Usage
	respond      func(call int, body string) (int, string)
	ctx          context.Context // defaults to context.Background
}

// runEngine evaluates n samples against a fake LLM and returns the
// result, recorded request bodies, emitted events and the run dir.
func runEngine(t *testing.T, o engineOpts, samples []core.Sample) (core.RunResult, []string, []Event, string) {
	t.Helper()
	ctx := o.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	respond := o.respond
	if respond == nil {
		respond = func(int, string) (int, string) {
			return http.StatusOK, completionJSON(`{"ok": true}`, o.usage)
		}
	}
	srv, bodies := startLLM(t, respond)
	client := provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})
	dir := t.TempDir()

	var mu sync.Mutex
	var events []Event
	e := &Engine{
		RunID: "test-run", RunDir: dir, Model: "fake-model", MaxTokens: 64,
		Workers: cmp.Or(o.workers, 1), Metrics: []string{"json_validator", "exact_match"},
		Budget: NewBudget(o.budgetTokens, o.budgetEvals), Provider: client,
		TaskName: "task", CandidateID: "cand", DatasetName: "ds",
		OnEvent: func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
	}
	res, err := e.Run(ctx, core.Candidate{ID: "cand", Prompt: "抽取实体：{input}"}, samples)
	if err != nil {
		t.Fatalf("Engine.Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return res, *bodies, events, dir
}

func TestEngineEvaluatesEachSampleExactlyOnce(t *testing.T) {
	res, bodies, events, _ := runEngine(t, engineOpts{workers: 4, usage: core.Usage{PromptTokens: 10, CompletionTokens: 5}}, testSamples(3))

	if res.ExitCode != 0 || res.Status != core.StatusCompleted {
		t.Errorf("result = %s/exit %d, want completed/0", res.Status, res.ExitCode)
	}
	if len(bodies) != 3 {
		t.Fatalf("llm calls = %d, want 3", len(bodies))
	}
	for _, s := range testSamples(3) {
		count := 0
		for _, b := range bodies {
			if strings.Contains(b, s.Input) {
				count++
			}
		}
		if count != 1 {
			t.Errorf("sample %s evaluated %d times, want exactly 1", s.ID, count)
		}
	}
	if res.Evaluated != 3 || res.Undispatched != 0 || len(res.FailedSamples) != 0 {
		t.Errorf("result = %+v", res)
	}
	if res.MetricMeans["json_validator"] != 1 || res.MetricMeans["exact_match"] != 1 {
		t.Errorf("means = %+v, want all 1", res.MetricMeans)
	}
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 30 || u.CompletionTokens != 15 {
		t.Errorf("usage = %+v, want 30/15", u)
	}
	// Event sequence: run_start first, run_done last, 3 sample_done.
	if len(events) < 5 || events[0].Type != EventRunStart || events[len(events)-1].Type != EventRunDone {
		t.Fatalf("event sequence = %v", eventTypes(events))
	}
	done := 0
	for _, ev := range events {
		if ev.Type == EventSampleDone {
			done++
			if ev.Usage == nil || ev.Usage.PromptTokens != 10 {
				t.Errorf("sample_done without usage: %+v", ev)
			}
		}
	}
	if done != 3 {
		t.Errorf("sample_done count = %d, want 3", done)
	}
}

func TestEngineBudgetEvalsStopsDispatch(t *testing.T) {
	res, bodies, _, _ := runEngine(t, engineOpts{budgetEvals: 1}, testSamples(3))

	if res.ExitCode != 2 || res.Status != core.StatusBudgetExhausted {
		t.Errorf("result = %s/exit %d, want budget_exhausted/2", res.Status, res.ExitCode)
	}
	if res.Undispatched != 2 || res.Evaluated != 1 {
		t.Errorf("undispatched = %d, evaluated = %d, want 2/1", res.Undispatched, res.Evaluated)
	}
	if len(bodies) != 1 {
		t.Errorf("llm calls = %d, want 1 (limit never exceeded)", len(bodies))
	}
}

func TestEngineBudgetEvalsExactFitStillSucceeds(t *testing.T) {
	// 3 samples, --budget-evals 3: everything dispatches, used == limit
	// must NOT read as budget exhaustion.
	res, bodies, _, _ := runEngine(t, engineOpts{budgetEvals: 3}, testSamples(3))

	if res.ExitCode != 0 || res.Undispatched != 0 {
		t.Errorf("result = %+v, want exit 0 with no undispatched", res)
	}
	if len(bodies) != 3 {
		t.Errorf("llm calls = %d, want 3", len(bodies))
	}
}

func TestEngineTokenSoftStopCompletesInFlight(t *testing.T) {
	// One worker, four samples of 100 tokens each, limit 150: sample 0
	// fits, sample 1 crosses the limit and arms the soft stop, sample 2
	// was already dispatched and completes, sample 3 stays undispatched.
	// (Dispatch of job k+1 can race with job k's usage recording, so
	// the crossing must happen before the last sample to be
	// deterministic — that is the spec's post-hoc soft-stop semantics.)
	res, bodies, events, dir := runEngine(t, engineOpts{
		workers:      1,
		budgetTokens: 150,
		usage:        core.Usage{PromptTokens: 100},
	}, testSamples(4))

	if res.ExitCode != 2 || res.Status != core.StatusBudgetExhausted {
		t.Errorf("result = %s/exit %d, want budget_exhausted/2", res.Status, res.ExitCode)
	}
	if res.Undispatched != 1 || res.Evaluated != 3 {
		t.Errorf("undispatched = %d, evaluated = %d, want 1/3", res.Undispatched, res.Evaluated)
	}
	if len(bodies) != 3 {
		t.Errorf("llm calls = %d, want 3", len(bodies))
	}
	// All dispatched samples completed and scored (soft stop lets
	// in-flight work finish).
	entries, err := os.ReadDir(filepath.Join(dir, "samples"))
	if err != nil || len(entries) != 3 {
		t.Errorf("sample traces = %v (%v), want 3", entries, err)
	}
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 300 {
		t.Errorf("executor usage = %d, want 300", u.PromptTokens)
	}
	sawBudgetStop := false
	for _, ev := range events {
		if ev.Type == EventBudgetStop {
			sawBudgetStop = true
		}
	}
	if !sawBudgetStop {
		t.Error("budget_stop event missing")
	}
}

func TestEngineProviderFailureFailsRun(t *testing.T) {
	res, _, _, dir := runEngine(t, engineOpts{
		respond: func(int, string) (int, string) {
			return http.StatusInternalServerError, `{"error": "upstream down"}`
		},
	}, testSamples(3))

	if res.ExitCode != 1 || res.Status != core.StatusFailed {
		t.Errorf("result = %s/exit %d, want failed/1", res.Status, res.ExitCode)
	}
	if len(res.FailedSamples) != 3 {
		t.Errorf("failed samples = %v", res.FailedSamples)
	}
	if len(res.MetricMeans) != 0 {
		t.Errorf("means = %+v, want none (no scored samples)", res.MetricMeans)
	}
	// Failed calls still leave traces.
	calls, err := os.ReadDir(filepath.Join(dir, "calls"))
	if err != nil || len(calls) != 3 {
		t.Errorf("call traces = %v (%v), want 3", calls, err)
	}
}

func TestEngineBudgetExhaustionBeatsFailure(t *testing.T) {
	// Two samples fail permanently, the third is never dispatched:
	// exit 2 wins over exit 1.
	res, bodies, _, _ := runEngine(t, engineOpts{
		budgetEvals: 2,
		respond: func(int, string) (int, string) {
			return http.StatusInternalServerError, `{}`
		},
	}, testSamples(3))

	if res.ExitCode != 2 || res.Status != core.StatusBudgetExhausted {
		t.Errorf("result = %s/exit %d, want budget_exhausted/2", res.Status, res.ExitCode)
	}
	if res.Undispatched != 1 || len(res.FailedSamples) != 2 {
		t.Errorf("undispatched = %d, failed = %v", res.Undispatched, res.FailedSamples)
	}
	if len(bodies) != 2 {
		t.Errorf("llm calls = %d, want 2", len(bodies))
	}
}

func TestEngineWritesTracesPerCallAndSample(t *testing.T) {
	samples := testSamples(2)
	_, _, _, dir := runEngine(t, engineOpts{usage: core.Usage{PromptTokens: 7, CompletionTokens: 3}}, samples)
	for i, s := range samples {
		var ct CallTrace
		callPath := filepath.Join(dir, "calls", fmt.Sprintf("%03d-%s.json", i+1, s.ID))
		b, err := os.ReadFile(callPath)
		if err != nil {
			t.Fatalf("read %s: %v", callPath, err)
		}
		if err := json.Unmarshal(b, &ct); err != nil {
			t.Fatalf("decode %s: %v", callPath, err)
		}
		if ct.SampleID != s.ID || ct.Role != core.RoleExecutor || ct.Seq != i+1 {
			t.Errorf("call trace header = %+v", ct)
		}
		if len(ct.Request.Messages) != 1 || !strings.Contains(ct.Request.Messages[0].Content, s.Input) {
			t.Errorf("call trace request = %+v", ct.Request.Messages)
		}
		if ct.Request.Model != "fake-model" || ct.Request.MaxTokens != 64 {
			t.Errorf("call trace request meta = %+v", ct.Request)
		}
		if ct.Response.Content == "" || ct.Response.Usage.PromptTokens != 7 {
			t.Errorf("call trace response = %+v", ct.Response)
		}
		if ct.LatencyMS < 0 {
			t.Errorf("call trace latency = %d", ct.LatencyMS)
		}

		var st core.SampleTrace
		samplePath := filepath.Join(dir, "samples", fmt.Sprintf("%03d-%s.json", i+1, s.ID))
		b, err = os.ReadFile(samplePath)
		if err != nil {
			t.Fatalf("read %s: %v", samplePath, err)
		}
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatalf("decode %s: %v", samplePath, err)
		}
		if st.SampleID != s.ID || st.Scores["exact_match"] != 1 || st.Usage.PromptTokens != 7 || st.DurationMS < 0 {
			t.Errorf("sample trace = %+v", st)
		}
		if !strings.Contains(st.Prompt, s.Input) || st.Response != `{"ok": true}` {
			t.Errorf("sample trace prompt/response = %q / %q", st.Prompt, st.Response)
		}
	}
}

func eventTypes(events []Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// --- review-fix regressions ----------------------------------------------

func TestEngineSampleTracesUniqueOnSanitizedCollision(t *testing.T) {
	// "train/001" and "train:001" sanitize to the same filename; the
	// seq prefix must keep both sample traces on disk instead of
	// letting parallel workers overwrite each other.
	samples := []core.Sample{
		{ID: "train/001", Input: "输入甲", Expected: map[string]any{"ok": true}, Split: "test"},
		{ID: "train:001", Input: "输入乙", Expected: map[string]any{"ok": true}, Split: "test"},
	}
	res, _, _, dir := runEngine(t, engineOpts{workers: 2}, samples)
	if res.ExitCode != 0 || res.Evaluated != 2 {
		t.Fatalf("result = %+v", res)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "samples"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("sample traces = %v (%v), want 2 distinct files", entries, err)
	}
	byID := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, "samples", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var st core.SampleTrace
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatalf("decode %s: %v", e.Name(), err)
		}
		byID[st.SampleID] = st.Prompt
	}
	if len(byID) != 2 {
		t.Fatalf("distinct sample ids on disk = %d, want 2: %v", len(byID), byID)
	}
	if !strings.Contains(byID["train/001"], "输入甲") || !strings.Contains(byID["train:001"], "输入乙") {
		t.Errorf("traces crossed inputs: %v", byID)
	}
}

func TestEngineEmptyContentWithLengthFailsExplicitly(t *testing.T) {
	// finish_reason=length with the whole budget spent on reasoning:
	// the sample must fail with explicit attribution (max_tokens
	// problem), keep budget accounting and stay out of metric means.
	res, _, events, dir := runEngine(t, engineOpts{
		respond: func(int, string) (int, string) {
			return http.StatusOK, completionFull("很长的思考过程……", "", "length",
				core.Usage{PromptTokens: 100, CompletionTokens: 2048})
		},
	}, testSamples(1))

	if res.ExitCode != 1 || res.Status != core.StatusFailed {
		t.Errorf("result = %s/exit %d, want failed/1", res.Status, res.ExitCode)
	}
	if len(res.FailedSamples) != 1 || len(res.MetricMeans) != 0 {
		t.Errorf("summary = %+v, want 1 failed sample and no metric means", res)
	}
	if u := res.UsageByRole[core.RoleExecutor]; u.CompletionTokens != 2048 {
		t.Errorf("usage = %+v, want completion 2048 (tokens were spent)", u)
	}
	b, err := os.ReadFile(filepath.Join(dir, "samples", "001-s-00.json"))
	if err != nil {
		t.Fatalf("read sample trace: %v", err)
	}
	var st core.SampleTrace
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Error, "empty content, finish_reason=length") {
		t.Errorf("trace error = %q, want explicit length attribution", st.Error)
	}
	if st.Reasoning == "" {
		t.Error("reasoning content missing from trace")
	}
	for _, ev := range events {
		if ev.Type == EventSampleDone && !strings.Contains(ev.Error, "finish_reason=length") {
			t.Errorf("sample_done event error = %q", ev.Error)
		}
	}
}

func TestEngineCanceledContextAbortsBeforeDispatch(t *testing.T) {
	// A canceled context must stop dispatch: nothing is sent, no
	// budget is consumed, no junk traces are written, and the run
	// reports aborted (exit 1), not failed or budget-exhausted.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, bodies, _, dir := runEngine(t, engineOpts{ctx: ctx, workers: 2}, testSamples(3))

	if res.Status != core.StatusAborted || res.ExitCode != 1 {
		t.Errorf("result = %s/exit %d, want aborted/1", res.Status, res.ExitCode)
	}
	if res.Undispatched != 3 || res.Evaluated != 0 {
		t.Errorf("undispatched = %d, evaluated = %d, want 3/0", res.Undispatched, res.Evaluated)
	}
	if len(bodies) != 0 {
		t.Errorf("llm calls = %d, want 0 after cancellation", len(bodies))
	}
	for _, sub := range []string{"calls", "samples"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil || len(entries) != 0 {
			t.Errorf("%s traces = %v (%v), want none", sub, entries, err)
		}
	}
}
