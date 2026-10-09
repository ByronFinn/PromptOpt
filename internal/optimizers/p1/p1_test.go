package p1

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/harness"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- fixtures ----------------------------------------------------------------

var p1Samples = []core.Sample{
	{ID: "s1", Input: "样本一恶寒发热", Expected: "证候一", Split: "train"},
	{ID: "s2", Input: "样本二心烦不寐", Expected: "证候二", Split: "train"},
	{ID: "s3", Input: "样本三潮热盗汗", Expected: "证候三", Split: "dev"},
	{ID: "s4", Input: "样本四脘腹胀满", Expected: "证候四", Split: "dev"},
	{ID: "s5", Input: "样本五咳嗽痰多", Expected: "证候五", Split: "train"},
	{ID: "s6", Input: "样本六腰膝酸软", Expected: "证候六", Split: "train"},
}

// p1BaselineRecords: the baseline prompt solves only the easy tail
// (s4..s6) over the FULL retained set — the final evaluation's
// comparison anchor.
func p1BaselineRecords() []engine.SampleRecord {
	out := make([]engine.SampleRecord, 0, len(p1Samples))
	for i, s := range p1Samples {
		score := 0.0
		if i >= 3 {
			score = 1
		}
		out = append(out, engine.SampleRecord{
			Sample: s, Scores: map[string]float64{"exact_match": score},
		})
	}
	return out
}

const (
	p1Hypotheses = `{"hypotheses":[{"id":"h1","text":"假设一：聚焦难样本的输出要求","confidence":0.9}]}`
	// Two hypotheses double the per-round probe cost — the truncation
	// fixture relies on it.
	p1Hypotheses2 = `{"hypotheses":[
 {"id":"h1","text":"假设一：聚焦难样本的输出要求","confidence":0.9},
 {"id":"h2","text":"假设二：覆盖易样本的核对指引","confidence":0.8}]}`
	p1Rewrite = `{"id":"rw","name":"改进版","prompt":"改进版助手。{input}"}`
	// Rewriting back to the baseline prompt keeps every child a clone:
	// the search never beats the baseline, so no final evaluation is
	// earned.
	p1BaselineRewrite = `{"id":"same","name":"原版","prompt":"基础助手。{input}"}`
)

// p1EvalMasks maps prompt-family markers to the bitmask of samples
// they answer correctly (bit i = sample i+1). Order matters: hypothesis
// markers win over product markers because temp prompts embed both the
// parent prompt and the guidance.
var p1EvalMasks = []struct {
	marker string
	bits   int
}{
	{"假设一", 1 << 1},              // s2
	{"假设二", (1 << 2) | (1 << 3)}, // s3, s4
	{"改进版", 0b111111},            // every sample
}

// p1Eval scripts the executor answer for one rendered prompt; the
// unmarked baseline prompt solves only s4..s6.
func p1Eval(body string) string {
	mask := 0b111000
	for _, m := range p1EvalMasks {
		if strings.Contains(body, m.marker) {
			mask = m.bits
			break
		}
	}
	for i, s := range p1Samples {
		if !strings.Contains(body, s.Input) {
			continue
		}
		if mask&(1<<i) != 0 {
			return s.Expected.(string)
		}
		return "答错"
	}
	return "答错"
}

// startP1LLM routes by engine markers first, then by the scripted
// evaluation answers.
func startP1LLM(t *testing.T, hypotheses, rewrite string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		content := p1Eval(body)
		switch {
		case strings.Contains(body, engine.MarkerReflect):
			content = hypotheses
		case strings.Contains(body, engine.MarkerRewrite),
			strings.Contains(body, engine.MarkerMerge),
			strings.Contains(body, engine.MarkerFresh):
			content = rewrite
		case strings.Contains(body, engine.MarkerHypRepair),
			strings.Contains(body, engine.MarkerCandFix):
			t.Errorf("unexpected repair call: %.200s", body)
			content = "{}"
		}
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeFilterReport persists a filter.json whose per-sample variances
// order the S* selection.
func writeFilterReport(t *testing.T, dir string, variance map[string]float64) {
	t.Helper()
	report := harness.FilterReport{
		Variants: 2, Primary: "exact_match", Thresholds: harness.DefaultThresholds(),
	}
	for _, s := range p1Samples {
		report.PerSample = append(report.PerSample, harness.SampleVerdict{
			ID: s.ID, Scores: []float64{1, 0}, Variance: variance[s.ID], Verdict: harness.VerdictKeep,
		})
		report.Kept++
	}
	if err := harness.SaveFilterReport(dir, report); err != nil {
		t.Fatal(err)
	}
}

func p1Request(runDir, synthDir string, srv *httptest.Server, budget *eval.Budget, onEvent func(eval.Event)) engine.Request {
	return engine.Request{
		Task: core.Task{
			Name: "tcm_p1", Description: "从医案文本判断证候",
			PromptTemplate: "基础助手。{input}", Metrics: []string{"exact_match"},
		},
		Params:   engine.Params{MaxRounds: 2, Minibatch: 1, StagnationLimit: 3, Epsilon: 0, Seed: 7},
		Initial:  core.Candidate{ID: "baseline", Prompt: "基础助手。{input}"},
		Samples:  p1Samples,
		Baseline: p1BaselineRecords(),
		Provider: provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Model:    "fake-model", MaxTokens: 128, OptMaxTokens: 128, Workers: 2,
		Budget: budget,
		RunID:  "p1-test", RunDir: runDir,
		SynthDir: synthDir,
		OnEvent:  onEvent,
	}
}

// eventLog records engine events; the engine emits from worker
// goroutines, so the slice is guarded.
type eventLog struct {
	mu     sync.Mutex
	events []eval.Event
}

func (l *eventLog) record(ev eval.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

func (l *eventLog) byType(typ string) []eval.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []eval.Event
	for _, ev := range l.events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// starIDs is the expected S* for the shared fixtures: top-3 retained
// samples by probe variance (s1 > s2 > s3 > … > s6).
var starIDs = []string{"s1", "s2", "s3"}

func newFixture(t *testing.T, hypotheses, rewrite string, evalLimit int64) (engine.Request, *eval.Budget, *eventLog) {
	t.Helper()
	srv := startP1LLM(t, hypotheses, rewrite)
	base := t.TempDir()
	runDir, synthDir := filepath.Join(base, "runs", "r1"), filepath.Join(base, "synth", "r1")
	if err := os.MkdirAll(synthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFilterReport(t, synthDir, map[string]float64{
		"s1": 0.5, "s2": 0.25, "s3": 0.125, "s4": 0.0625, "s5": 0.03, "s6": 0,
	})
	budget := eval.NewBudget(0, evalLimit)
	log := &eventLog{}
	return p1Request(runDir, synthDir, srv, budget, log.record), budget, log
}

// --- ① the budget-allocated search + full-set final evaluation ---------------

func TestP1SearchesStarAndFinalizesFullSet(t *testing.T) {
	req, budget, log := newFixture(t, p1Hypotheses, p1Rewrite, 14)
	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}

	// Terminal state: the S* search improved, the final evaluation
	// delivered a full-coverage row that dominates the baseline.
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 2 {
		t.Errorf("reason/rounds = %s/%d, want rounds_done/2", res.Reason, res.Rounds)
	}
	if res.Best.ID != finalID {
		t.Errorf("Best.ID = %s, want %s", res.Best.ID, finalID)
	}
	if res.BestMeans["exact_match"] != 1 {
		t.Errorf("BestMeans = %+v, want exact_match=1 over the full kept set", res.BestMeans)
	}
	frontierIDs := make([]string, 0, len(res.Frontier))
	for _, m := range res.Frontier {
		frontierIDs = append(frontierIDs, m.ID())
	}
	// The final row [1,1,1,1,1,1] dominates the baseline [0,0,0,1,1,1]:
	// the evicted baseline stays in the lineage, not on the frontier.
	if !reflect.DeepEqual(frontierIDs, []string{"p1-final"}) {
		t.Errorf("run-root frontier = %v, want p1-final alone (baseline dominated)", frontierIDs)
	}

	// Budget accounting: planStar(kept=6, rounds=2, mb=1, limit=14) →
	// m=3; two rounds cost (1 probe + 3 child) each and the final
	// evaluation spends the remaining 6 — exactly the eval limit.
	if used, _ := budget.Snapshot(); used != 14 {
		t.Errorf("budget evals = %d, want exactly 14", used)
	}

	// S* selection event: three samples by variance, in retained order.
	sel := log.byType(EventSelectDone)
	if len(sel) != 1 {
		t.Fatalf("select_done events = %d, want 1", len(sel))
	}
	if sel[0].Detail["s_star"] != 3 || sel[0].Detail["kept"] != 6 {
		t.Errorf("select_done detail = %+v, want s_star=3 kept=6", sel[0].Detail)
	}
	if !reflect.DeepEqual(sel[0].Detail["samples"], starIDs) {
		t.Errorf("select_done samples = %v, want %v", sel[0].Detail["samples"], starIDs)
	}
	fin := log.byType(EventFinalDone)
	if len(fin) != 1 || fin[0].Detail["admitted"] != true {
		t.Errorf("final_done events = %+v, want one admitted final", fin)
	}

	// Phase separation: every sample_done outside the final evaluation
	// must come from an S* sample — the search rounds never touch
	// kept∖S* — and the final evaluation must cover all six.
	innerDone, finalDone := 0, 0
	finalSeen := map[string]bool{}
	for _, ev := range log.byType(eval.EventSampleDone) {
		cand, _ := ev.Detail["candidate"].(string)
		if cand == finalID {
			finalDone++
			finalSeen[ev.SampleID] = true
			continue
		}
		innerDone++
		if !slices.Contains(starIDs, ev.SampleID) {
			t.Errorf("search-phase eval touched %s outside S* %v", ev.SampleID, starIDs)
		}
	}
	if innerDone != 8 || finalDone != 6 {
		t.Errorf("sample_done inner/final = %d/%d, want 8/6", innerDone, finalDone)
	}
	if len(finalSeen) != len(p1Samples) {
		t.Errorf("final evaluation covered %v, want all %d kept samples", finalSeen, len(p1Samples))
	}

	// Artifacts: the search's audit trail parks under p1-search/, the
	// run root keeps the authoritative baseline+final picture.
	for _, p := range []string{
		filepath.Join(req.RunDir, searchDirName, "lineage.json"),
		filepath.Join(req.RunDir, searchDirName, "report.md"),
		filepath.Join(req.RunDir, "lineage.json"),
		filepath.Join(req.RunDir, "report.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing artifact %s: %v", p, err)
		}
	}
}

func TestP1BaselineBestSkipsFinalEval(t *testing.T) {
	// The rewrite echoes the baseline prompt: every child is a clone,
	// the search's Best stays the baseline, and no candidate earns the
	// final evaluation — the rounds still burn their budget.
	req, budget, log := newFixture(t, p1Hypotheses, p1BaselineRewrite, 14)
	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Best.ID != "baseline" {
		t.Errorf("Best.ID = %s, want baseline", res.Best.ID)
	}
	if res.Reason != engine.ReasonRoundsDone {
		t.Errorf("reason = %s, want rounds_done", res.Reason)
	}
	// Two rounds × (1 probe + 3 child) evaluations, no final spend.
	if used, _ := budget.Snapshot(); used != 8 {
		t.Errorf("budget evals = %d, want 8 (rounds only)", used)
	}
	fin := log.byType(EventFinalDone)
	if len(fin) != 1 || fin[0].Detail["skipped"] != true {
		t.Errorf("final_done = %+v, want one skipped final", fin)
	}
	if n := len(log.byType(eval.EventSampleDone)); n != 8 {
		t.Errorf("sample_done count = %d, want 8 (S* rounds only)", n)
	}
}

func TestP1TruncatedFinalDeliversBaseline(t *testing.T) {
	// planStar(kept=6, rounds=2, mb=1, limit=12) → m=2; two rounds of
	// (2 probes + 2 children) spend 8 and the 6-sample final
	// evaluation runs two slots short: its partial row must not decide
	// delivery, and the baseline is what gets delivered.
	req, budget, log := newFixture(t, p1Hypotheses2, p1Rewrite, 12)
	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonBudgetStopped {
		t.Errorf("reason = %s, want budget_stopped", res.Reason)
	}
	if res.Best.ID != "baseline" {
		t.Errorf("Best.ID = %s, want the undominated baseline", res.Best.ID)
	}
	if used, _ := budget.Snapshot(); used != 12 {
		t.Errorf("budget evals = %d, want exactly the 12-slot limit", used)
	}
	fin := log.byType(EventFinalDone)
	if len(fin) != 1 || fin[0].Detail["undispatched"] != 2 {
		t.Errorf("final_done = %+v, want undispatched=2", fin)
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "baseline" {
		t.Errorf("frontier = %+v, want the baseline alone", res.Frontier)
	}
}

// --- ② explicit error, never a silent fallback -------------------------------

func TestP1MissingReportIsExplicitError(t *testing.T) {
	srv := startP1LLM(t, p1Hypotheses, p1Rewrite)
	base := t.TempDir()

	// No synthesis root at all.
	req := p1Request(filepath.Join(base, "runs", "r1"), "", srv, eval.NewBudget(0, 14), nil)
	res, err := New().Optimize(t.Context(), req)
	if err == nil {
		t.Fatal("empty SynthDir: Optimize succeeded, want an explicit error")
	}
	if !strings.Contains(err.Error(), "SynthDir") || !strings.Contains(err.Error(), "filter.json") {
		t.Errorf("error = %v, want it to name SynthDir and filter.json", err)
	}
	if res.Best.ID != "" || res.Frontier != nil || res.Reason != "" {
		t.Errorf("result = %+v, want the zero value on failure", res)
	}

	// A synthesis root whose filter.json never landed.
	synthDir := filepath.Join(base, "synth", "r2")
	if err := os.MkdirAll(synthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	req = p1Request(filepath.Join(base, "runs", "r2"), synthDir, srv, eval.NewBudget(0, 14), nil)
	if _, err := New().Optimize(t.Context(), req); err == nil || !strings.Contains(err.Error(), "filter.json") {
		t.Errorf("missing filter.json: err = %v, want a load failure", err)
	}
}

// --- planning helpers ---------------------------------------------------------

func TestPlanStar(t *testing.T) {
	cases := []struct {
		name              string
		kept, rounds, mb  int
		limit, used, want int
	}{
		{"unlimited budget takes all kept", 6, 2, 1, 0, 0, 6},
		{"main fixture reserves probe slots", 6, 2, 1, 14, 0, 3},
		{"post-pipeline starvation clamps to one", 4, 5, 4, 14, 12, 1},
		{"used beyond limit clamps to one", 4, 2, 1, 10, 20, 1},
		{"large budget caps at kept", 6, 2, 1, 100, 0, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, remaining := planStar(tc.kept, tc.rounds, tc.mb, int64(tc.limit), int64(tc.used))
			if m != tc.want {
				t.Errorf("planStar m = %d, want %d", m, tc.want)
			}
			if wantRem := int64(tc.limit) - int64(tc.used); tc.limit > 0 && remaining != wantRem {
				t.Errorf("remaining = %d, want %d", remaining, wantRem)
			}
		})
	}
}

func TestSelectStar(t *testing.T) {
	kept := []core.Sample{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}
	report := harness.FilterReport{PerSample: []harness.SampleVerdict{
		{ID: "a", Variance: 0.1},
		{ID: "b", Variance: 0.5},
		// c absent from the report (checkpoint-added): ranks last.
		{ID: "d", Variance: 0.1},
	}}
	// Top-2 by variance: b first, a over d by retained order on the
	// tie; returned in retained order.
	if got := selectStar(kept, report, 2); !reflect.DeepEqual(sampleIDs(got), []string{"a", "b"}) {
		t.Errorf("selectStar(m=2) = %v, want [a b]", sampleIDs(got))
	}
	// Report-absent samples rank last but still fill a wide cut.
	if got := selectStar(kept, report, 4); !reflect.DeepEqual(sampleIDs(got), []string{"a", "b", "c", "d"}) {
		t.Errorf("selectStar(m=4) = %v, want the full retained order", sampleIDs(got))
	}
	// m clamps to the retained set.
	if got := selectStar(kept, report, 10); len(got) != 4 {
		t.Errorf("selectStar(m=10) length = %d, want 4", len(got))
	}
}
