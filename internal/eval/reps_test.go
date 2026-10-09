package eval

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// --- budget: multi-slot acquisition (P2 派发环点名) -------------------------

func TestBudgetTryAcquireEvalsAllOrNothing(t *testing.T) {
	b := NewBudget(0, 5)
	if !b.TryAcquireEvals(3) {
		t.Fatal("3 of 5 must succeed")
	}
	if b.TryAcquireEvals(3) {
		t.Fatal("3 more over the limit must fail")
	}
	if evals, _ := b.Snapshot(); evals != 3 {
		t.Fatalf("evals = %d, want 3 (failed acquire must not partially occupy)", evals)
	}
	if !b.TryAcquireEvals(2) {
		t.Fatal("the remaining 2 must fit exactly")
	}
	if evals, _ := b.Snapshot(); evals != 5 {
		t.Fatalf("evals = %d, want 5", evals)
	}
	if b.TryAcquireEval() {
		t.Fatal("single acquire over the exhausted limit must fail")
	}
	// k < 1 reads as one slot, never a free pass.
	b2 := NewBudget(0, 1)
	if !b2.TryAcquireEvals(0) {
		t.Fatal("k=0 must clamp to a single-slot acquire")
	}
	if b2.TryAcquireEvals(1) {
		t.Fatal("limit 1 must be exhausted after the clamped acquire")
	}
	// A soft stop denies multi-slot acquisition like a single one.
	b3 := NewBudget(10, 0)
	b3.RecordUsage(core.RoleExecutor, core.Usage{PromptTokens: 11})
	if b3.TryAcquireEvals(2) {
		t.Fatal("soft stop must deny multi-slot acquisition")
	}
}

func TestBudgetEvalLimitAccessor(t *testing.T) {
	if got := NewBudget(7, 9).EvalLimit(); got != 9 {
		t.Errorf("EvalLimit = %d, want 9", got)
	}
	if got := NewBudget(7, 0).EvalLimit(); got != 0 {
		t.Errorf("EvalLimit = %d, want 0 (unlimited)", got)
	}
}

// --- engine: reps folding (用例 ①) ------------------------------------------

// TestEngineRepsFoldsMeanSDAndEvent drives the full reps fold: the mock
// alternates valid/invalid JSON per call, so with reps=2 the sample's
// exact_match score is the mean 0.5, the in-sample sd is 0.5, usage is
// the two-rep accumulation, the sample_done event carries scores_sd and
// the sample trace records reps=2.
func TestEngineRepsFoldsMeanSDAndEvent(t *testing.T) {
	res, bodies, events, dir := runEngine(t, engineOpts{
		workers: 1, // call order (and therefore the alternation) stays deterministic
		reps:    2,
		usage:   core.Usage{PromptTokens: 10, CompletionTokens: 5},
		respond: func(call int, _ string) (int, string) {
			if call%2 == 1 {
				return http.StatusOK, completionJSON(`{"ok": true}`, core.Usage{PromptTokens: 10, CompletionTokens: 5})
			}
			return http.StatusOK, completionJSON("这不是 JSON", core.Usage{PromptTokens: 10, CompletionTokens: 5})
		},
	}, testSamples(1))

	if res.ExitCode != 0 || res.Evaluated != 1 {
		t.Fatalf("result = %+v, want completed with 1 evaluated", res)
	}
	if len(bodies) != 2 {
		t.Fatalf("llm calls = %d, want 2 (one per rep)", len(bodies))
	}
	if got := res.MetricMeans["exact_match"]; got != 0.5 {
		t.Errorf("exact_match mean = %v, want 0.5 (k-rep fold)", got)
	}
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 20 || u.CompletionTokens != 10 {
		t.Errorf("usage = %+v, want 20/10 (k-rep accumulation)", u)
	}

	var done *Event
	for i := range events {
		if events[i].Type == EventSampleDone {
			done = &events[i]
		}
	}
	if done == nil {
		t.Fatal("sample_done event missing")
	}
	if done.Scores["exact_match"] != 0.5 {
		t.Errorf("event scores = %v, want the 0.5 mean", done.Scores)
	}
	if got := done.ScoresSD["exact_match"]; got != 0.5 {
		t.Errorf("event scores_sd = %v, want 0.5 (population sd of [1,0])", got)
	}

	b, err := os.ReadFile(filepath.Join(dir, "samples", "001-s-00.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st core.SampleTrace
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.Reps != 2 || st.Scores["exact_match"] != 0.5 || st.ScoresSD["exact_match"] != 0.5 {
		t.Errorf("sample trace reps/scores/sd = %d/%v/%v, want 2/0.5/0.5", st.Reps, st.Scores, st.ScoresSD)
	}
}

// TestEngineSingleRepStaysSDFree pins the zero-value contract: without
// --reps the wire stays byte-identical to the historical shape — one
// call per sample and no scores_sd anywhere.
func TestEngineSingleRepStaysSDFree(t *testing.T) {
	res, bodies, events, _ := runEngine(t, engineOpts{usage: core.Usage{PromptTokens: 10, CompletionTokens: 5}}, testSamples(2))
	if res.ExitCode != 0 || len(bodies) != 2 {
		t.Fatalf("result = %+v bodies = %d, want 0/2", res, len(bodies))
	}
	for _, ev := range events {
		if ev.Type == EventSampleDone && ev.ScoresSD != nil {
			t.Errorf("sample_done carries scores_sd on the single-shot path: %v", ev.ScoresSD)
		}
	}
}

// TestEngineTemperatureReachesTheWire pins the A-layer knob: a set
// temperature rides every executor request; the zero default keeps the
// field off the wire (gateway default takes over — the 现状).
func TestEngineTemperatureReachesTheWire(t *testing.T) {
	hot, hotBodies, _, _ := runEngine(t, engineOpts{temperature: 0.7}, testSamples(1))
	if hot.ExitCode != 0 {
		t.Fatalf("hot run = %+v", hot)
	}
	for _, b := range hotBodies {
		if !strings.Contains(b, `"temperature":0.7`) {
			t.Errorf("executor request missing the temperature field: %.200s", b)
		}
	}

	_, coldBodies, _, _ := runEngine(t, engineOpts{}, testSamples(1))
	for _, b := range coldBodies {
		if strings.Contains(b, "temperature") {
			t.Errorf("temperature 0 must stay off the wire: %.200s", b)
		}
	}
}

// --- engine: dispatch budget per rep (用例 ②，TryAcquireEvals 点名) ---------

// TestEngineRepsBudgetDispatchPerSample pins the dispatch-time cost
// semantics: reps=3 with --budget-evals 5 lets the first sample occupy
// 3 slots whole and denies the second (needs 3, only 2 left) — the
// sample stays undispatched and no rep of it ever dials.
func TestEngineRepsBudgetDispatchPerSample(t *testing.T) {
	res, bodies, _, _ := runEngine(t, engineOpts{workers: 1, budgetEvals: 5, reps: 3}, testSamples(2))

	if res.ExitCode != 2 || res.Status != core.StatusBudgetExhausted {
		t.Errorf("result = %s/exit %d, want budget_exhausted/2", res.Status, res.ExitCode)
	}
	if res.Evaluated != 1 || res.Undispatched != 1 {
		t.Errorf("evaluated/undispatched = %d/%d, want 1/1", res.Evaluated, res.Undispatched)
	}
	if len(bodies) != 3 {
		t.Errorf("llm calls = %d, want 3 (the denied sample's reps never dial)", len(bodies))
	}
}

// TestEngineRepsBudgetExactFitStillSucceeds: 2 samples × reps 2 need
// exactly 4 slots — used == limit must not read as exhaustion.
func TestEngineRepsBudgetExactFitStillSucceeds(t *testing.T) {
	res, bodies, _, _ := runEngine(t, engineOpts{workers: 1, budgetEvals: 4, reps: 2}, testSamples(2))
	if res.ExitCode != 0 || res.Undispatched != 0 {
		t.Errorf("result = %+v, want exit 0 with everything dispatched", res)
	}
	if len(bodies) != 4 {
		t.Errorf("llm calls = %d, want 4", len(bodies))
	}
}

// TestEngineAnyRepFailureFailsSample: the second rep of the only sample
// fails after retries — the whole sample fails with no folded scores,
// while the first rep's tokens stay metered (spent either way).
func TestEngineAnyRepFailureFailsSample(t *testing.T) {
	res, bodies, _, _ := runEngine(t, engineOpts{
		workers: 1, reps: 3,
		respond: func(call int, _ string) (int, string) {
			if call == 2 {
				return http.StatusInternalServerError, `{"error": "upstream down"}`
			}
			return http.StatusOK, completionJSON(`{"ok": true}`, core.Usage{PromptTokens: 10, CompletionTokens: 5})
		},
	}, testSamples(1))

	if res.ExitCode != 1 || res.Status != core.StatusFailed {
		t.Errorf("result = %s/exit %d, want failed/1", res.Status, res.ExitCode)
	}
	if len(res.FailedSamples) != 1 || len(res.MetricMeans) != 0 {
		t.Errorf("summary = %+v, want 1 failed sample and no means (任一 rep 失败即样本失败)", res)
	}
	if len(bodies) != 2 {
		t.Errorf("llm calls = %d, want 2 (the run stops at the failing rep)", len(bodies))
	}
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 10 {
		t.Errorf("executor usage = %+v, want the first rep's 10 tokens metered", u)
	}
}

// TestEngineRepScoresLandInTrace pins the P10 rep-strip data source:
// with reps=3 the sample trace records the per-rep per-metric scores in
// rep order (rep_scores), alongside the mean/sd fold; the single-shot
// path stays rep_scores-free ( ScoresSD 的 omitempty 口径一致).
func TestEngineRepScoresLandInTrace(t *testing.T) {
	res, _, _, dir := runEngine(t, engineOpts{
		workers: 1, // 调用次序（即逐 rep 内容）保持确定
		reps:    3,
		usage:   core.Usage{PromptTokens: 10, CompletionTokens: 5},
		respond: func(call int, _ string) (int, string) {
			if call%2 == 1 {
				return http.StatusOK, completionJSON(`{"ok": true}`, core.Usage{PromptTokens: 10, CompletionTokens: 5})
			}
			return http.StatusOK, completionJSON("这不是 JSON", core.Usage{PromptTokens: 10, CompletionTokens: 5})
		},
	}, testSamples(1))
	if res.ExitCode != 0 {
		t.Fatalf("result = %+v, want completed", res)
	}
	b, err := os.ReadFile(filepath.Join(dir, "samples", "001-s-00.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st core.SampleTrace
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	want := []map[string]float64{
		{"exact_match": 1, "json_validator": 1},
		{"exact_match": 0, "json_validator": 0},
		{"exact_match": 1, "json_validator": 1},
	}
	if len(st.RepScores) != len(want) {
		t.Fatalf("rep_scores = %v, want %d reps", st.RepScores, len(want))
	}
	for i, rep := range want {
		for m, v := range rep {
			if got := st.RepScores[i][m]; got != v {
				t.Errorf("rep_scores[%d][%s] = %v, want %v", i, m, got, v)
			}
		}
	}
	// 均值/SD 逻辑不动：scores 仍是 k-rep 均值，scores_sd 仍为总体 SD。
	if st.Scores["exact_match"] != 2.0/3.0 {
		t.Errorf("scores = %v, want the 2/3 mean", st.Scores)
	}
	if sd := st.ScoresSD["exact_match"]; sd*sd < 2.0/9.0-1e-9 || sd*sd > 2.0/9.0+1e-9 {
		t.Errorf("scores_sd = %v, want the population sd of [1,0,1] (var 2/9)", sd)
	}

	// 单次采样路径：trace 不带 rep_scores（同 ScoresSD 的 reps>1 门）。
	_, _, _, singleDir := runEngine(t, engineOpts{usage: core.Usage{PromptTokens: 10, CompletionTokens: 5}}, testSamples(1))
	sb, err := os.ReadFile(filepath.Join(singleDir, "samples", "001-s-00.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sb), "rep_scores") {
		t.Errorf("single-shot trace carries rep_scores:\n%s", sb)
	}
}
