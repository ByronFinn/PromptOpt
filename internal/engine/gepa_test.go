package engine

import (
	"context"
	"encoding/json"
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
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- golden-loop fixtures ---------------------------------------------------

var goldenTask = core.Task{
	Name: "tcm_zhenghou", Description: "从医案文本判断证候",
	PromptTemplate: "基础助手。{input}", Metrics: []string{"exact_match"},
}

var goldenSamples = []core.Sample{
	{ID: "s1", Input: "甲恶寒发热", Expected: "风寒", Split: "train"},
	{ID: "s2", Input: "乙心烦不寐", Expected: "心火", Split: "train"},
	{ID: "s3", Input: "丙潮热盗汗", Expected: "阴虚", Split: "dev"},
	{ID: "s4", Input: "丁脘腹胀满", Expected: "气滞", Split: "dev"},
}

const (
	goldenHypotheses = `{"hypotheses":[
 {"id":"h1","text":"假设一：聚焦心火样本的输出要求","sample_ids":["s2"],"confidence":0.6},
 {"id":"h2","text":"假设二：覆盖阴虚与气滞样本的辨别指引","sample_ids":["s3","s4"],"confidence":0.8}
]}`

	goldenRewrite = `{"id":"rw","name":"改进版","prompt":"改进版助手。{input}"}`
	goldenMerge   = `{"id":"mg","name":"合并版","prompt":"合并版助手。{input}"}`
	goldenFresh   = `{"id":"fr","name":"重启版","prompt":"重启版助手。{input}"}`
)

// goldenEvalMasks maps prompt-family markers to the bitmask of samples
// they answer correctly (bit i = sample s(i+1)). Order matters:
// hypothesis markers win over product markers because temp prompts
// embed both the parent prompt and the guidance.
var goldenEvalMasks = []struct {
	marker string
	bits   int
}{
	{"假设一", 1 << 1},              // s2 → [0,1,0,0]
	{"假设二", (1 << 2) | (1 << 3)}, // s3,s4 → [0,0,1,1]
	{"改进版", (1 << 1) | (1 << 2)}, // s2,s3 → [0,1,1,0]
	{"合并版", 0b1111},              // all → [1,1,1,1]
	{"重启版", (1 << 0) | (1 << 1)}, // s1,s2 → [1,1,0,0]
}

// goldenEval scripts the executor answer for one rendered prompt.
func goldenEval(body string) string {
	mask := 1 // baseline: s1 only → [1,0,0,0]
	for _, m := range goldenEvalMasks {
		if strings.Contains(body, m.marker) {
			mask = m.bits
			break
		}
	}
	for i, s := range goldenSamples {
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

// startGoldenLLM routes by engine markers first, then by the scripted
// evaluation answers; usage is role-scripted per the markers so the
// budget accounting is exactly predictable.
func startGoldenLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		content, usage := func() (string, [2]int) {
			switch {
			case strings.Contains(body, MarkerReflect):
				return goldenHypotheses, [2]int{100, 50}
			case strings.Contains(body, MarkerRewrite):
				return goldenRewrite, [2]int{100, 50}
			case strings.Contains(body, MarkerMerge):
				return goldenMerge, [2]int{100, 50}
			case strings.Contains(body, MarkerFresh):
				return goldenFresh, [2]int{100, 50}
			case strings.Contains(body, MarkerHypRepair), strings.Contains(body, MarkerCandFix):
				t.Errorf("unexpected repair call: %.200s", body)
				return "{}", [2]int{100, 50}
			default:
				return goldenEval(body), [2]int{10, 5}
			}
		}()
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			content, usage[0], usage[1], usage[0]+usage[1])
	}))
}

func goldenParams() Params {
	return Params{MaxRounds: 6, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
}

func goldenBaselineRecords() []SampleRecord {
	out := make([]SampleRecord, len(goldenSamples))
	for i, s := range goldenSamples {
		resp := "答错"
		score := 0.0
		if i == 0 {
			resp, score = "风寒", 1
		}
		out[i] = SampleRecord{Sample: s, Response: resp, Scores: map[string]float64{"exact_match": score}}
	}
	return out
}

func goldenRequest(dir string, budget *eval.Budget, onEvent func(eval.Event)) Request {
	return Request{
		Task: goldenTask, Params: goldenParams(),
		Initial:  core.Candidate{ID: "baseline", Prompt: goldenTask.PromptTemplate},
		Samples:  goldenSamples,
		Baseline: goldenBaselineRecords(),
		Provider: nil, // filled by the caller
		Model:    "fake-model", MaxTokens: 1024, OptMaxTokens: 1024, Workers: 2,
		Budget: budget,
		RunID:  "golden", RunDir: dir,
		OnEvent: onEvent,
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

func (l *eventLog) indexOf(typ string, detailKey string, detailVal any) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, ev := range l.events {
		if ev.Type != typ {
			continue
		}
		if detailKey == "" || ev.Detail[detailKey] == detailVal {
			return i
		}
	}
	return -1
}

// TestGepaGoldenLoop drives the full optimizer against the scripted
// LLM: baseline [1,0,0,0] → g01 rewrite [0,1,1,0] → g02 clone → g03
// merge [1,1,1,1] (evicts all) → g04/g05 dominated → g06 restart
// (vista_restart) dominated. Asserts events, artifacts and determinism.
func TestGepaGoldenLoop(t *testing.T) {
	srv := startGoldenLLM(t)

	var log eventLog
	dir := t.TempDir()
	req := goldenRequest(dir, eval.NewBudget(0, 0), log.record)
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := (&Gepa{}).Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != ReasonRoundsDone || res.Rounds != 6 {
		t.Fatalf("reason = %s rounds = %d, want rounds_done/6", res.Reason, res.Rounds)
	}
	if res.Best.ID != "g03" {
		t.Fatalf("best = %s, want g03 (merge, all-correct)", res.Best.ID)
	}
	if res.BestMeans["exact_match"] != 1 {
		t.Errorf("best primary mean = %v, want 1", res.BestMeans["exact_match"])
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "g03" {
		t.Errorf("frontier = %v, want only g03", res.Frontier)
	}

	// The engine never emits run_done/run_start: inner unit lifecycle
	// stays internal so SSE/replay never close early.
	if n := len(log.byType(eval.EventRunDone)); n != 0 {
		t.Errorf("run_done count = %d, want 0 (engine layer owns none)", n)
	}
	if n := len(log.byType(eval.EventRunStart)); n != 0 {
		t.Errorf("run_start count = %d, want 0 (inner filtered)", n)
	}

	// Event order within round 1: start → reflect → validated → mutate
	// → frontier → done, monotonically increasing indices.
	r1 := func(typ string) int { return log.indexOf(typ, "round", 1) }
	for _, pair := range [][2]string{
		{EventRoundStart, EventReflectDone},
		{EventReflectDone, EventHypoValidated},
		{EventHypoValidated, EventMutateDone},
		{EventMutateDone, EventFrontierUpdated},
		{EventFrontierUpdated, EventRoundDone},
	} {
		a, b := r1(pair[0]), r1(pair[1])
		if a < 0 || b < 0 || a >= b {
			t.Errorf("round 1 order broken: %s(%d) before %s(%d)", pair[0], a, pair[1], b)
		}
	}
	// ε-greedy audit trail: ε=0 exploits the argmax-lift hypothesis.
	hv := log.byType(EventHypoValidated)
	if len(hv) != 6 {
		t.Fatalf("hypotheses_validated events = %d, want 6", len(hv))
	}
	if mode := hv[0].Detail["mode"]; mode != ModeExploit {
		t.Errorf("round 1 mode = %v, want exploit", mode)
	}
	if sel := hv[0].Detail["selected_hypothesis"]; sel != "h2" {
		t.Errorf("round 1 selected = %v, want h2 (lift 0.25)", sel)
	}
	if lift := hv[0].Detail["lift"]; lift != 0.25 {
		t.Errorf("round 1 lift = %v, want 0.25", lift)
	}
	// Sample events carry the candidate/round stamp.
	for _, ev := range log.byType(eval.EventSampleDone) {
		if ev.Detail["candidate"] == nil || ev.Detail["round"] == nil {
			t.Fatalf("sample_done missing stamp: %+v", ev.Detail)
		}
	}

	// vista_restart fired at round 6 (stagnant 2 ≥ limit 2).
	restarts := log.byType(EventVistaRestart)
	if len(restarts) != 1 || restarts[0].Detail["round"] != 6 {
		t.Fatalf("vista_restart events = %+v, want one at round 6", restarts)
	}

	// lineage.json: baseline plus six children with operators,
	// parents, hypothesis references and score rows.
	var lineage []LineageRecord
	loadJSON(t, filepath.Join(dir, "lineage.json"), &lineage)
	if len(lineage) != 7 {
		t.Fatalf("lineage records = %d, want 7", len(lineage))
	}
	if lineage[0].ID != "baseline" || lineage[0].Operator != OpBaseline || len(lineage[0].Parents) != 0 {
		t.Fatalf("baseline record = %+v", lineage[0])
	}
	byID := map[string]LineageRecord{}
	for _, rec := range lineage {
		byID[rec.ID] = rec
	}
	g01 := byID["g01"]
	if g01.Operator != OpRewrite || !slices.Equal(g01.Parents, []string{"baseline"}) {
		t.Errorf("g01 = %+v, want rewrite ← baseline", g01)
	}
	if len(g01.Hypotheses) != 1 || g01.Hypotheses[0].ID != "h2" {
		t.Errorf("g01 hypotheses = %+v, want [h2]", g01.Hypotheses)
	}
	if !slices.Equal(g01.Scores, []float64{0, 1, 1, 0}) || !g01.Admitted {
		t.Errorf("g01 scores/admitted = %v/%v", g01.Scores, g01.Admitted)
	}
	g02 := byID["g02"]
	if g02.Admitted || !slices.Equal(g02.Scores, []float64{0, 1, 1, 0}) {
		t.Errorf("g02 (clone) admitted = %v scores = %v", g02.Admitted, g02.Scores)
	}
	g03 := byID["g03"]
	if g03.Operator != OpMerge || !slices.Contains(g03.Parents, "baseline") || !slices.Contains(g03.Parents, "g01") {
		t.Errorf("g03 = %+v, want merge of baseline+g01", g03)
	}
	if !slices.Equal(g03.Scores, []float64{1, 1, 1, 1}) || !g03.Admitted || g03.PrimaryMean != 1 {
		t.Errorf("g03 = %v admitted=%v mean=%v", g03.Scores, g03.Admitted, g03.PrimaryMean)
	}
	if g06 := byID["g06"]; g06.Operator != OpRestart {
		t.Errorf("g06 operator = %s, want restart", g06.Operator)
	}

	// frontier.json: best is g03; only g03 remains.
	var frontier FrontierFile
	loadJSON(t, filepath.Join(dir, "frontier.json"), &frontier)
	if frontier.Best.ID != "g03" || frontier.Primary != "exact_match" {
		t.Errorf("frontier.json best = %+v", frontier.Best)
	}
	if len(frontier.Members) != 1 || frontier.Members[0].ID != "g03" || frontier.Members[0].PrimaryMean != 1 {
		t.Errorf("frontier members = %+v", frontier.Members)
	}

	// report.md carries the Top-1 prompt in full plus the trade-off
	// and lineage sections.
	report, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	text := string(report)
	for _, anchor := range []string{"最优提示词", "合并版助手。{input}", "取舍说明", "前沿成员", "谱系", "g03"} {
		if !strings.Contains(text, anchor) {
			t.Errorf("report.md missing anchor %q", anchor)
		}
	}

	// Evaluation units: 6 rounds × (2 probes + 1 child) with traces.
	units, err := os.ReadDir(filepath.Join(dir, "evals"))
	if err != nil || len(units) != 18 {
		t.Errorf("eval units = %d (err %v), want 18", len(units), err)
	}
	// Optimizer calls: 6 reflections + 6 mutations.
	calls, err := os.ReadDir(filepath.Join(dir, "opt-calls"))
	if err != nil || len(calls) != 12 {
		t.Errorf("opt calls = %d (err %v), want 12", len(calls), err)
	}

	// Same seed replays: lineage and frontier are structurally
	// identical once the time fields are stripped.
	var log2 eventLog
	dir2 := t.TempDir()
	req2 := goldenRequest(dir2, eval.NewBudget(0, 0), log2.record)
	req2.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})
	if _, err := (&Gepa{}).Optimize(t.Context(), req2); err != nil {
		t.Fatalf("replay Optimize: %v", err)
	}
	assertArtifactDeterminism(t, dir, dir2)
}

// assertArtifactDeterminism decodes lineage.json/frontier.json from
// both dirs, deletes created_at/generated_at and compares structurally.
func assertArtifactDeterminism(t *testing.T, dirA, dirB string) {
	t.Helper()
	var linA, linB []LineageRecord
	loadJSON(t, filepath.Join(dirA, "lineage.json"), &linA)
	loadJSON(t, filepath.Join(dirB, "lineage.json"), &linB)
	for i := range linA {
		linA[i].CreatedAt = time.Time{}
	}
	for i := range linB {
		linB[i].CreatedAt = time.Time{}
	}
	if !reflect.DeepEqual(linA, linB) {
		t.Errorf("lineage.json diverged across same-seed runs:\nA: %s\nB: %s", truncateForLog(linA), truncateForLog(linB))
	}

	var fA, fB FrontierFile
	loadJSON(t, filepath.Join(dirA, "frontier.json"), &fA)
	loadJSON(t, filepath.Join(dirB, "frontier.json"), &fB)
	fA.GeneratedAt, fB.GeneratedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(fA, fB) {
		t.Errorf("frontier.json diverged across same-seed runs:\nA: %s\nB: %s", truncateForLog(fA), truncateForLog(fB))
	}
}

func truncateForLog(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}

func loadJSON(t *testing.T, path string, dst any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// --- budget accounting -------------------------------------------------------

// TestGepaBudgetRoleAccounting: the shared budget meters optimizer
// calls (reflect + mutate, 2 in a 1-round run) and executor calls
// (2 probes × 4 samples + 1 child × 4 = 12) exactly, and pure
// optimizer spend never arms the executor soft stop.
func TestGepaBudgetRoleAccounting(t *testing.T) {
	srv := startGoldenLLM(t)
	budget := eval.NewBudget(0, 0)
	req := goldenRequest(t.TempDir(), budget, nil)
	req.Params = Params{MaxRounds: 1, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := (&Gepa{}).Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != ReasonRoundsDone {
		t.Fatalf("reason = %s, want rounds_done", res.Reason)
	}
	opt := res.Usage[core.RoleOptimizer]
	if opt.PromptTokens != 200 || opt.CompletionTokens != 100 {
		t.Errorf("optimizer usage = %+v, want 200/100 (2 calls × 100/50)", opt)
	}
	exe := res.Usage[core.RoleExecutor]
	if exe.PromptTokens != 120 || exe.CompletionTokens != 60 {
		t.Errorf("executor usage = %+v, want 120/60 (12 calls × 10/5)", exe)
	}
	if budget.SoftStopped() {
		t.Error("no executor token limit was set — soft stop must stay disarmed")
	}
}

// TestGepaOptValveBudgetStopped: --budget-opt-tokens covering exactly
// one optimizer call stops the loop after that call with reason
// budget_stopped and a non-empty best.
func TestGepaOptValveBudgetStopped(t *testing.T) {
	srv := startGoldenLLM(t)
	budget := eval.NewBudget(0, 0)
	var log eventLog
	req := goldenRequest(t.TempDir(), budget, log.record)
	req.Params = Params{MaxRounds: 3, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.OptBudgetTokens = 150 // one optimizer call = exactly 150 tokens
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := (&Gepa{}).Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != ReasonBudgetStopped {
		t.Fatalf("reason = %s, want budget_stopped", res.Reason)
	}
	if res.Best.ID != "baseline" || res.Best.Prompt == "" {
		t.Fatalf("best = %+v, want non-empty baseline delivery", res.Best)
	}
	// Optimizer spend alone never arms the executor soft stop.
	if budget.SoftStopped() {
		t.Error("optimizer-only spend must not arm the executor soft stop")
	}
	// The valve fired visibly: budget_stop with role=optimizer.
	stops := log.byType(eval.EventBudgetStop)
	found := false
	for _, ev := range stops {
		if ev.Detail["role"] == string(core.RoleOptimizer) {
			found = true
		}
	}
	if !found {
		t.Errorf("no budget_stop event with role=optimizer among %+v", stops)
	}
	_, usage := budget.Snapshot()
	if u := usage[core.RoleOptimizer]; u.Total() != 150 {
		t.Errorf("optimizer usage = %d, want exactly one 150-token call", u.Total())
	}
}

// TestGepaExecutorEvalLimitBudgetStopped: an eval limit that runs dry
// mid-validation ends the round with undispatched samples, reason
// budget_stopped and the best still delivered. The partially evaluated
// hypothesis is skipped (its lift is not comparable) and the
// half-evaluated child stays off the frontier.
func TestGepaExecutorEvalLimitBudgetStopped(t *testing.T) {
	srv := startGoldenLLM(t)
	var log eventLog
	budget := eval.NewBudget(0, 5) // 5 evals: probe h1 (4) + 1 of h2
	req := goldenRequest(t.TempDir(), budget, log.record)
	req.Params = Params{MaxRounds: 3, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := (&Gepa{}).Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != ReasonBudgetStopped {
		t.Fatalf("reason = %s, want budget_stopped", res.Reason)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d, want 1 (stopped during round 1)", res.Rounds)
	}
	if res.Best.ID != "baseline" || res.Best.Prompt == "" {
		t.Fatalf("best = %+v, want non-empty baseline delivery", res.Best)
	}
	if evals, _ := budget.Snapshot(); evals != 5 {
		t.Errorf("evals started = %d, want exactly the 5 allowed", evals)
	}
	// Probe h2 ran dry after one sample: skipped, so only h1 was
	// validated and selected.
	hv := log.byType(EventHypoValidated)
	if len(hv) != 1 || hv[0].Detail["validated"] != 1 {
		t.Fatalf("hypotheses_validated = %+v, want one event with validated=1", hv)
	}
	// The half-evaluated child never joined the frontier.
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "baseline" {
		t.Errorf("frontier = %v, want only baseline", res.Frontier)
	}
	var lineage []LineageRecord
	loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
	if len(lineage) != 2 || lineage[1].ID != "g01" {
		t.Fatalf("lineage = %+v, want baseline + g01", lineage)
	}
	if !lineage[1].Incomplete || lineage[1].Admitted {
		t.Errorf("g01 incomplete/admitted = %v/%v, want true/false", lineage[1].Incomplete, lineage[1].Admitted)
	}
}

// TestGepaIncompleteChildStaysOffFrontier: the budget dries up during
// the child's full retained-set evaluation (2 of 4 samples
// dispatched). The zero-filled partial row must not join the frontier
// — it would read as a real [0,1,0,0] row and could evict a fully
// evaluated member scoring 0 on exactly those cells — and the report
// flags the truncation.
func TestGepaIncompleteChildStaysOffFrontier(t *testing.T) {
	srv := startGoldenLLM(t)
	budget := eval.NewBudget(0, 10) // probes h1+h2 (8) + 2 of 4 child evals
	req := goldenRequest(t.TempDir(), budget, nil)
	req.Params = Params{MaxRounds: 3, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := (&Gepa{}).Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != ReasonBudgetStopped {
		t.Fatalf("reason = %s, want budget_stopped", res.Reason)
	}
	if res.Best.ID != "baseline" {
		t.Fatalf("best = %s, want baseline (the incomplete child is not deliverable)", res.Best.ID)
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "baseline" {
		t.Fatalf("frontier = %v, want only baseline", res.Frontier)
	}
	if evals, _ := budget.Snapshot(); evals != 10 {
		t.Errorf("evals started = %d, want exactly the 10 allowed", evals)
	}
	var lineage []LineageRecord
	loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
	if len(lineage) != 2 || lineage[1].ID != "g01" {
		t.Fatalf("lineage = %+v, want baseline + g01", lineage)
	}
	if !lineage[1].Incomplete || lineage[1].Admitted {
		t.Errorf("g01 incomplete/admitted = %v/%v, want true/false", lineage[1].Incomplete, lineage[1].Admitted)
	}
	if !slices.Equal(lineage[1].Scores, []float64{0, 1, 0, 0}) {
		t.Errorf("g01 partial row = %v, want [0,1,0,0] (first two samples dispatched)", lineage[1].Scores)
	}
	// The report flags the truncated evaluation.
	report, err := os.ReadFile(filepath.Join(req.RunDir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	if !strings.Contains(string(report), "预算中断评估不完整") {
		t.Error("report.md does not flag the budget-interrupted child")
	}
}

// TestGepaAllProbesBudgetTruncatedStops: when every hypothesis probe
// runs out of budget, no selection evidence remains — the loop must
// stop immediately instead of burning optimizer calls on doomed
// rounds.
func TestGepaAllProbesBudgetTruncatedStops(t *testing.T) {
	srv := startGoldenLLM(t)
	budget := eval.NewBudget(0, 3) // probe h1 gets 3 of 4 samples, h2 gets none
	req := goldenRequest(t.TempDir(), budget, nil)
	req.Params = Params{MaxRounds: 3, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := (&Gepa{}).Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != ReasonBudgetStopped {
		t.Fatalf("reason = %s, want budget_stopped", res.Reason)
	}
	if res.Rounds != 1 || res.Best.ID != "baseline" {
		t.Fatalf("rounds/best = %d/%s, want 1/baseline", res.Rounds, res.Best.ID)
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "baseline" {
		t.Fatalf("frontier = %v, want only baseline", res.Frontier)
	}
	// Exactly one optimizer call happened (the reflection); no
	// mutation call was burned on a doomed round.
	calls, err := os.ReadDir(filepath.Join(req.RunDir, "opt-calls"))
	if err != nil || len(calls) != 1 {
		t.Errorf("opt calls = %d (err %v), want 1 (reflect only)", len(calls), err)
	}
	var lineage []LineageRecord
	loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
	if len(lineage) != 1 || lineage[0].ID != "baseline" {
		t.Fatalf("lineage = %+v, want only the baseline row", lineage)
	}
}

// TestGepaAbortedByContext: cancellation maps to reason=aborted while
// still delivering the baseline best.
func TestGepaAbortedByContext(t *testing.T) {
	srv := startGoldenLLM(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := goldenRequest(t.TempDir(), eval.NewBudget(0, 0), nil)
	req.Params = Params{MaxRounds: 2, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := (&Gepa{}).Optimize(ctx, req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != ReasonAborted {
		t.Fatalf("reason = %s, want aborted", res.Reason)
	}
	if res.Best.ID != "baseline" {
		t.Fatalf("best = %s, want baseline", res.Best.ID)
	}
}

// TestGepaEmptyHypothesisPoolSkipsRounds: when reflection returns an
// empty hypothesis pool in every round — both shapes, a bare empty
// array and entries wiped by the {input} literal cleaning — each round
// takes the graceful skip branch (vista.Record(false) + round_done
// skipped with 「没有可验证的假设」, the formerly dead branch), and the
// run ends rounds_done with the baseline as best. Exactly one optimizer
// call per round is burned (the reflection) and no mutation call ever
// fires (PRD-0001 D9①).
func TestGepaEmptyHypothesisPoolSkipsRounds(t *testing.T) {
	for _, tc := range []struct{ name, pool string }{
		{"empty array", `{"hypotheses":[]}`},
		{"all {input} literals", `{"hypotheses":[{"id":"h1","text":"{input}","confidence":0.9}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := startOptLLM(t, func(body string) string {
				if strings.Contains(body, MarkerReflect) {
					return tc.pool
				}
				t.Errorf("unexpected optimizer call: %.200s", body)
				return "{}"
			})
			var log eventLog
			req := goldenRequest(t.TempDir(), eval.NewBudget(0, 0), log.record)
			req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

			res, err := (&Gepa{}).Optimize(t.Context(), req)
			if err != nil {
				t.Fatalf("Optimize: %v", err)
			}
			if res.Reason != ReasonRoundsDone || res.Rounds != 6 {
				t.Fatalf("reason = %s rounds = %d, want rounds_done/6", res.Reason, res.Rounds)
			}
			if res.Best.ID != "baseline" {
				t.Fatalf("best = %s, want baseline", res.Best.ID)
			}
			// Each round closes with the graceful skip: no verifiable
			// hypotheses (empty pool reached normalize → skipped round).
			done := log.byType(EventRoundDone)
			if len(done) != 6 {
				t.Fatalf("round_done events = %d, want 6", len(done))
			}
			for i, ev := range done {
				if ev.Detail["round"] != i+1 {
					t.Errorf("round_done[%d] round = %v, want %d", i, ev.Detail["round"], i+1)
				}
				if ev.Detail["skipped"] != true || ev.Detail["error"] != "没有可验证的假设" {
					t.Errorf("round %d round_done = %+v, want skipped with 「没有可验证的假设」", i+1, ev.Detail)
				}
			}
			// Reflection reported the empty pool each round; nothing was
			// ever validated.
			rd := log.byType(EventReflectDone)
			if len(rd) != 6 {
				t.Fatalf("reflect_done events = %d, want 6", len(rd))
			}
			for i, ev := range rd {
				if ev.Detail["hypotheses"] != 0 {
					t.Errorf("round %d reflect_done hypotheses = %v, want 0", i+1, ev.Detail["hypotheses"])
				}
			}
			if n := len(log.byType(EventHypoValidated)); n != 0 {
				t.Errorf("hypotheses_validated events = %d, want 0", n)
			}
			// opt-calls = rounds × 1: the reflections only, no mutation
			// call burned on a skipped round.
			calls, err := os.ReadDir(filepath.Join(req.RunDir, "opt-calls"))
			if err != nil || len(calls) != 6 {
				t.Errorf("opt calls = %d (err %v), want 6 (one reflection per round)", len(calls), err)
			}
			// The trail keeps the baseline row only — nothing was mutated
			// or admitted.
			var lineage []LineageRecord
			loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
			if len(lineage) != 1 || lineage[0].ID != "baseline" {
				t.Fatalf("lineage = %+v, want only the baseline row", lineage)
			}
		})
	}
}
