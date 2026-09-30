package evoprompt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
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
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- fixtures ----------------------------------------------------------------

var evoTask = core.Task{
	Name: "tcm_zhenghou", Description: "从医案文本判断证候",
	PromptTemplate: "基础助手。{input}", Metrics: []string{"exact_match"},
}

var evoSamples = []core.Sample{
	{ID: "s1", Input: "甲恶寒发热", Expected: "风寒", Split: "train"},
	{ID: "s2", Input: "乙心烦不寐", Expected: "心火", Split: "train"},
	{ID: "s3", Input: "丙潮热盗汗", Expected: "阴虚", Split: "dev"},
	{ID: "s4", Input: "丁脘腹胀满", Expected: "气滞", Split: "dev"},
}

// Seed variants and generation children, as candidate JSON.
var evoInitVariants = []string{
	`{"id":"v1","name":"变体一","prompt":"变体一助手。{input}"}`,
	`{"id":"v2","name":"变体二","prompt":"变体二助手。{input}"}`,
	`{"id":"v3","name":"变体三","prompt":"变体三助手。{input}"}`,
	`{"id":"v4","name":"变体四","prompt":"变体四助手。{input}"}`,
	`{"id":"v5","name":"变体五","prompt":"变体五助手。{input}"}`,
}

const (
	evoCrossover = `{"id":"xo","name":"交叉版","prompt":"交叉版助手。{input}"}`
	evoMutation  = `{"id":"mu","name":"变异版","prompt":"变异版助手。{input}"}`
	evoDE        = `{"id":"dv","name":"差分版","prompt":"差分版助手。{input}"}`

	// A seed proposal failing the quality gate (no {input}), used to
	// drive the repair path and the degenerate single-parent case.
	evoBadVariant = `{"id":"bad","name":"坏变体","prompt":"没有占位符的提示词"}`
)

// evoEvalMasks maps prompt-family markers to the bitmask of samples
// they answer correctly (bit i = sample s(i+1)); the default mask is
// the baseline's s1-only row. Evaluation prompts never carry the
// operator markers, so families are the only signal needed.
var evoEvalMasks = []struct {
	marker string
	bits   int
}{
	{"变体一", 0b0011}, // s1,s2
	{"变体二", 0b0101}, // s1,s3
	{"变体三", 0b0111}, // s1,s2,s3
	{"变体四", 0b1011}, // s1,s2,s4
	{"变体五", 0b1101}, // s1,s3,s4
	{"交叉版", 0b1111}, // all
	{"变异版", 0b0010}, // s2
	{"差分版", 0b1110}, // s2,s3,s4
}

// evoEval scripts the executor answer for one rendered prompt.
func evoEval(body string) string {
	mask := 1 // baseline: s1 only
	for _, m := range evoEvalMasks {
		if strings.Contains(body, m.marker) {
			mask = m.bits
			break
		}
	}
	for i, s := range evoSamples {
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

// evoLLM is the scripted LLM: operator markers route to candidate
// JSON, everything else to the scripted evaluation answers. Usage is
// scripted per the markers so budget accounting is exactly
// predictable. initResponses overrides the seed variants; repair
// overrides the MarkerCandFix answer ("" marks repairs unexpected).
type evoLLM struct {
	t             *testing.T
	initResponses []string
	repair        string

	mu             sync.Mutex
	initCalls      int
	crossoverCalls int
	mutationCalls  int
	deCalls        int
	repairCalls    int
}

func (s *evoLLM) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		content, usage := func() (string, [2]int) {
			switch {
			case strings.Contains(body, engine.MarkerCandFix):
				s.mu.Lock()
				s.repairCalls++
				s.mu.Unlock()
				if s.repair != "" {
					return s.repair, [2]int{100, 50}
				}
				s.t.Errorf("unexpected repair call: %.200s", body)
				return "{}", [2]int{100, 50}
			case strings.Contains(body, MarkerInitPop):
				s.mu.Lock()
				s.initCalls++
				n := s.initCalls
				s.mu.Unlock()
				variants := evoInitVariants
				if s.initResponses != nil {
					variants = s.initResponses
				}
				return variants[(n-1)%len(variants)], [2]int{100, 50}
			case strings.Contains(body, MarkerCrossover):
				s.mu.Lock()
				s.crossoverCalls++
				s.mu.Unlock()
				return evoCrossover, [2]int{100, 50}
			case strings.Contains(body, MarkerDE):
				s.mu.Lock()
				s.deCalls++
				s.mu.Unlock()
				return evoDE, [2]int{100, 50}
			case strings.Contains(body, MarkerMutation):
				s.mu.Lock()
				s.mutationCalls++
				s.mu.Unlock()
				return evoMutation, [2]int{100, 50}
			default:
				return evoEval(body), [2]int{10, 5}
			}
		}()
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			content, usage[0], usage[1], usage[0]+usage[1])
	})
}

// startEvoLLM serves the default scripted behavior (five good seed
// variants, repairs unexpected).
func startEvoLLM(t *testing.T) *httptest.Server {
	t.Helper()
	s := &evoLLM{t: t}
	return httptest.NewServer(s.handler())
}

func evoParams() engine.Params {
	return engine.Params{MaxRounds: 2, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
}

func evoBaselineRecords() []engine.SampleRecord {
	out := make([]engine.SampleRecord, len(evoSamples))
	for i, s := range evoSamples {
		resp, score := "答错", 0.0
		if i == 0 {
			resp, score = "风寒", 1
		}
		out[i] = engine.SampleRecord{Sample: s, Response: resp, Scores: map[string]float64{"exact_match": score}}
	}
	return out
}

func evoRequest(dir string, budget *eval.Budget, onEvent func(eval.Event)) engine.Request {
	return engine.Request{
		Task: evoTask, Params: evoParams(),
		Initial:  core.Candidate{ID: "baseline", Prompt: evoTask.PromptTemplate},
		Samples:  evoSamples,
		Baseline: evoBaselineRecords(),
		Provider: nil, // filled by the caller
		Model:    "fake-model", MaxTokens: 1024, OptMaxTokens: 1024, Workers: 2,
		Budget:  budget,
		RunID:   "evo",
		RunDir:  dir,
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

// --- golden loop --------------------------------------------------------------

// TestEvoPromptGoldenLoop drives the full GA loop against the scripted
// LLM: seeding evaluates five variants (best mean 0.75), generation
// 1's crossover child 交叉版 answers everything (admitted, evicts the
// whole frontier) while its mutation child 变异版 is dominated,
// generation 2 breeds clones of both (clone-rejected). It asserts
// events, population bookkeeping, artifacts, budget accounting and
// same-seed determinism.
func TestEvoPromptGoldenLoop(t *testing.T) {
	srv := startEvoLLM(t)

	var log eventLog
	dir := t.TempDir()
	budget := eval.NewBudget(0, 0)
	req := evoRequest(dir, budget, log.record)
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 2 {
		t.Fatalf("reason = %s rounds = %d, want rounds_done/2", res.Reason, res.Rounds)
	}
	// e06 = generation 1's crossover child (e01..e05 are the seeds).
	if res.Best.ID != "e06" {
		t.Fatalf("best = %s, want e06 (交叉版, all-correct)", res.Best.ID)
	}
	if res.BestMeans["exact_match"] != 1 {
		t.Errorf("best primary mean = %v, want 1", res.BestMeans["exact_match"])
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "e06" {
		t.Errorf("frontier = %v, want only e06", res.Frontier)
	}
	if !res.ConstraintSatisfied {
		t.Errorf("constraint satisfied = false, want true (no constraint declared)")
	}

	// population_updated: one after seeding, one per generation, each
	// stamped with paradigm/stage and consistent with the lineage means.
	pops := log.byType(EventPopulationUpdated)
	if len(pops) != 3 {
		t.Fatalf("population_updated events = %d, want 3 (init + 2 generations)", len(pops))
	}
	if best := pops[0].Detail["best"]; best != "e03" {
		t.Errorf("init population best = %v, want e03 (seed mean 0.75, id-ordered)", best)
	}
	if fitness := pops[0].Detail["fitness"]; fitness != 0.75 {
		t.Errorf("init population fitness = %v, want 0.75", fitness)
	}
	if size := pops[0].Detail["size"]; size != 6 {
		t.Errorf("init population size = %v, want 6", size)
	}
	for i, ev := range pops {
		if ev.Detail["paradigm"] != Paradigm || ev.Detail["stage"] == "" {
			t.Errorf("population_updated %d missing paradigm/stage: %+v", i, ev.Detail)
		}
	}
	if best := pops[1].Detail["best"]; best != "e06" {
		t.Errorf("generation 1 population best = %v, want e06", best)
	}
	// Round events carry the paradigm stamp too.
	for _, typ := range []string{engine.EventRoundStart, engine.EventRoundDone} {
		for _, ev := range log.byType(typ) {
			if ev.Detail["paradigm"] != Paradigm || ev.Detail["stage"] == "" {
				t.Errorf("%s missing paradigm/stage: %+v", typ, ev.Detail)
			}
		}
	}
	// Event order within generation 1: start → population snapshot →
	// done.
	if a, b := log.indexOf(engine.EventRoundStart, "round", 1), log.indexOf(EventPopulationUpdated, "round", 1); a < 0 || b < 0 || a >= b {
		t.Errorf("generation 1 order broken: round_start(%d) before population_updated(%d)", a, b)
	}
	if a, b := log.indexOf(EventPopulationUpdated, "round", 1), log.indexOf(engine.EventRoundDone, "round", 1); a < 0 || b < 0 || a >= b {
		t.Errorf("generation 1 order broken: population_updated(%d) before round_done(%d)", a, b)
	}
	// Sample events carry the candidate/round stamp.
	for _, ev := range log.byType(eval.EventSampleDone) {
		if ev.Detail["candidate"] == nil || ev.Detail["round"] == nil {
			t.Fatalf("sample_done missing stamp: %+v", ev.Detail)
		}
	}

	// lineage.json: baseline + 5 seeds + 4 children with operators and
	// parents; the population's fitness and the lineage's primary mean
	// stay consistent (double bookkeeping).
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(dir, "lineage.json"), &lineage)
	if len(lineage) != 10 {
		t.Fatalf("lineage records = %d, want 10", len(lineage))
	}
	if lineage[0].ID != "baseline" || lineage[0].Operator != engine.OpBaseline {
		t.Fatalf("baseline record = %+v", lineage[0])
	}
	byID := map[string]engine.LineageRecord{}
	for _, rec := range lineage {
		byID[rec.ID] = rec
	}
	for i, want := range []float64{0.5, 0.5, 0.75, 0.75, 0.75} {
		id := fmt.Sprintf("e%02d", i+1)
		rec := byID[id]
		if rec.Operator != OpInit || !slices.Equal(rec.Parents, []string{"baseline"}) {
			t.Errorf("%s = %+v, want init ← baseline", id, rec)
		}
		if rec.PrimaryMean != want || !rec.Admitted {
			t.Errorf("%s mean/admitted = %v/%v, want %v/true", id, rec.PrimaryMean, rec.Admitted, want)
		}
	}
	e06 := byID["e06"]
	if e06.Operator != OpCrossover || len(e06.Parents) != 2 {
		t.Errorf("e06 = %+v, want crossover of two parents", e06)
	}
	if !slices.Equal(e06.Scores, []float64{1, 1, 1, 1}) || e06.PrimaryMean != 1 {
		t.Errorf("e06 scores/mean = %v/%v, want all-ones/1", e06.Scores, e06.PrimaryMean)
	}
	e07 := byID["e07"]
	if e07.Operator != OpMutation || len(e07.Parents) != 1 {
		t.Errorf("e07 = %+v, want single-parent mutation", e07)
	}
	// Clone children stay in the lineage but never join the frontier.
	for _, id := range []string{"e08", "e09"} {
		if rec := byID[id]; rec.Admitted {
			t.Errorf("%s (clone) admitted = true, want false", id)
		}
	}

	// frontier.json: e06 dominates everyone.
	var frontierFile engine.FrontierFile
	loadJSON(t, filepath.Join(dir, "frontier.json"), &frontierFile)
	if frontierFile.Best.ID != "e06" || frontierFile.Primary != "exact_match" {
		t.Errorf("frontier.json best = %+v", frontierFile.Best)
	}
	if len(frontierFile.Members) != 1 || frontierFile.Members[0].ID != "e06" {
		t.Errorf("frontier members = %+v, want only e06", frontierFile.Members)
	}
	// report.md carries the winning prompt in full.
	report, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	for _, anchor := range []string{"最优提示词", "交叉版助手。{input}"} {
		if !strings.Contains(string(report), anchor) {
			t.Errorf("report.md missing anchor %q", anchor)
		}
	}

	// Evaluation units: 5 seeds + 2 children × 2 generations.
	units, err := os.ReadDir(filepath.Join(dir, "evals"))
	if err != nil || len(units) != 9 {
		t.Errorf("eval units = %d (err %v), want 9", len(units), err)
	}
	// Optimizer calls: 5 seeds + 2 children × 2 generations, no repairs.
	calls, err := os.ReadDir(filepath.Join(dir, "opt-calls"))
	if err != nil || len(calls) != 9 {
		t.Errorf("opt calls = %d (err %v), want 9", len(calls), err)
	}

	// Budget accounting: optimizer 9 × (100, 50); executor 36 sample
	// calls × (10, 5).
	opt := res.Usage[core.RoleOptimizer]
	if opt.PromptTokens != 900 || opt.CompletionTokens != 450 {
		t.Errorf("optimizer usage = %+v, want 900/450", opt)
	}
	exe := res.Usage[core.RoleExecutor]
	if exe.PromptTokens != 360 || exe.CompletionTokens != 180 {
		t.Errorf("executor usage = %+v, want 360/180", exe)
	}
	if budget.SoftStopped() {
		t.Error("no executor token limit was set — soft stop must stay disarmed")
	}

	// Same seed replays: lineage and frontier are structurally
	// identical once the time fields are stripped.
	var log2 eventLog
	dir2 := t.TempDir()
	req2 := evoRequest(dir2, eval.NewBudget(0, 0), log2.record)
	req2.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})
	if _, err := New().Optimize(t.Context(), req2); err != nil {
		t.Fatalf("replay Optimize: %v", err)
	}
	assertEvoDeterminism(t, dir, dir2)
}

func assertEvoDeterminism(t *testing.T, dirA, dirB string) {
	t.Helper()
	var linA, linB []engine.LineageRecord
	loadJSON(t, filepath.Join(dirA, "lineage.json"), &linA)
	loadJSON(t, filepath.Join(dirB, "lineage.json"), &linB)
	for i := range linA {
		linA[i].CreatedAt = time.Time{}
	}
	for i := range linB {
		linB[i].CreatedAt = time.Time{}
	}
	if !reflect.DeepEqual(linA, linB) {
		t.Errorf("lineage.json diverged across same-seed runs:\nA: %v\nB: %v", linA, linB)
	}
	var fA, fB engine.FrontierFile
	loadJSON(t, filepath.Join(dirA, "frontier.json"), &fA)
	loadJSON(t, filepath.Join(dirB, "frontier.json"), &fB)
	fA.GeneratedAt, fB.GeneratedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(fA, fB) {
		t.Errorf("frontier.json diverged across same-seed runs:\nA: %v\nB: %v", fA, fB)
	}
}

// --- variant branches ----------------------------------------------------------

// TestEvoPromptDEBranch pins the differential-evolution variant: the
// crossover operator is never dialed, both children carry operator de
// with two or three parents, and the run finishes normally.
func TestEvoPromptDEBranch(t *testing.T) {
	s := &evoLLM{t: t}
	deSrv := httptest.NewServer(s.handler())
	defer deSrv.Close()

	dir := t.TempDir()
	req := evoRequest(dir, eval.NewBudget(0, 0), nil)
	req.Params = engine.Params{MaxRounds: 1, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.Opts = map[string]string{OptVariant: VariantDE}
	req.Provider = provider.NewOpenAI(deSrv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 1 {
		t.Fatalf("reason = %s rounds = %d, want rounds_done/1", res.Reason, res.Rounds)
	}
	s.mu.Lock()
	crossovers, de, mutations := s.crossoverCalls, s.deCalls, s.mutationCalls
	s.mu.Unlock()
	if crossovers != 0 {
		t.Errorf("crossover calls = %d, want 0 under the de variant", crossovers)
	}
	if de != 2 {
		t.Errorf("de calls = %d, want 2 (one per child)", de)
	}
	if mutations != 0 {
		t.Errorf("meta mutation calls = %d, want 0 (population >= 2)", mutations)
	}
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(dir, "lineage.json"), &lineage)
	children := 0
	for _, rec := range lineage {
		if rec.Operator != OpDE || rec.Round != 1 {
			continue
		}
		children++
		if len(rec.Parents) < 2 || len(rec.Parents) > 3 {
			t.Errorf("de child %s parents = %v, want 2-3", rec.ID, rec.Parents)
		}
	}
	if children != 2 {
		t.Fatalf("de children in lineage = %d, want 2", children)
	}
	// 差分版 scores [0,1,1,1] (mean 0.75) ties the seeds; Best breaks
	// by id asc → e03.
	if res.Best.ID != "e03" {
		t.Errorf("best = %s, want e03 (tie broken by id)", res.Best.ID)
	}
}

// TestEvoPromptUnknownVariant rejects an Opts value outside {ga, de}.
func TestEvoPromptUnknownVariant(t *testing.T) {
	req := evoRequest(t.TempDir(), eval.NewBudget(0, 0), nil)
	req.Opts = map[string]string{OptVariant: "bogus"}
	req.Provider = provider.NewOpenAI("http://127.0.0.1:1", "1", provider.OpenAIConfig{}) // never dialed
	if _, err := New().Optimize(t.Context(), req); err == nil || !strings.Contains(err.Error(), "unknown variant") {
		t.Fatalf("err = %v, want unknown variant error", err)
	}
}

// --- budget stops ---------------------------------------------------------------

// TestEvoPromptOptValveBudgetStopped: an optimizer token budget
// covering exactly two calls stops seeding with reason budget_stopped
// while still delivering the best evaluated seed.
func TestEvoPromptOptValveBudgetStopped(t *testing.T) {
	s := &evoLLM{t: t}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	var log eventLog
	req := evoRequest(t.TempDir(), eval.NewBudget(0, 0), log.record)
	req.Params = engine.Params{MaxRounds: 3, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.OptBudgetTokens = 300 // two optimizer calls = exactly 300 tokens
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonBudgetStopped {
		t.Fatalf("reason = %s, want budget_stopped", res.Reason)
	}
	if res.Rounds != 0 {
		t.Errorf("rounds = %d, want 0 (stopped while seeding)", res.Rounds)
	}
	// e01 and e02 were produced and evaluated before the valve armed at
	// exactly 300 tokens; e01 (mean 0.5) beats the baseline (0.25).
	if res.Best.ID != "e01" || res.Best.Prompt == "" {
		t.Fatalf("best = %+v, want the evaluated seed e01", res.Best)
	}
	// The valve fired visibly: budget_stop with role=optimizer.
	found := false
	for _, ev := range log.byType(eval.EventBudgetStop) {
		if ev.Detail["role"] == string(core.RoleOptimizer) {
			found = true
		}
	}
	if !found {
		t.Error("no optimizer-role budget_stop event for the tripped valve")
	}
}

// TestEvoPromptEvalLimitBudgetStopped: a dispatch-limit budget runs
// dry mid-evaluation — the half-evaluated seed joins the lineage
// flagged incomplete, never the frontier, and the current best is
// still delivered.
func TestEvoPromptEvalLimitBudgetStopped(t *testing.T) {
	srv := startEvoLLM(t)

	dir := t.TempDir()
	// 5 dispatch slots: seed e01 (4 samples) fits, e02 dispatches one
	// sample and leaves three undispatched.
	req := evoRequest(dir, eval.NewBudget(0, 5), nil)
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonBudgetStopped {
		t.Fatalf("reason = %s, want budget_stopped", res.Reason)
	}
	if res.Best.ID != "e01" {
		t.Fatalf("best = %s, want e01 (mean 0.5 > baseline 0.25)", res.Best.ID)
	}
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(dir, "lineage.json"), &lineage)
	byID := map[string]engine.LineageRecord{}
	for _, rec := range lineage {
		byID[rec.ID] = rec
	}
	e02 := byID["e02"]
	if !e02.Incomplete || e02.Admitted {
		t.Errorf("e02 incomplete/admitted = %v/%v, want true/false", e02.Incomplete, e02.Admitted)
	}
	// The frontier never saw e02.
	for _, m := range res.Frontier {
		if m.ID() == "e02" {
			t.Error("incomplete seed e02 joined the frontier")
		}
	}
}

// --- small-population and small-sample edges -----------------------------------

// TestEvoPromptKeptOne drives the whole loop over a one-sample retained
// set: rows stay length-1, no division breaks, Best is delivered.
func TestEvoPromptKeptOne(t *testing.T) {
	srv := startEvoLLM(t)

	req := evoRequest(t.TempDir(), eval.NewBudget(0, 0), nil)
	req.Samples = evoSamples[:1]
	req.Baseline = evoBaselineRecords()[:1]
	req.Params = engine.Params{MaxRounds: 1, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 1 {
		t.Fatalf("reason = %s rounds = %d, want rounds_done/1", res.Reason, res.Rounds)
	}
	if res.Best.ID == "" || res.Best.Prompt == "" {
		t.Fatalf("best = %+v, want a non-empty delivery", res.Best)
	}
	if res.BestMeans["exact_match"] != 1 {
		t.Errorf("best primary mean = %v, want 1 (every scripted prompt answers s1)", res.BestMeans["exact_match"])
	}
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
	for _, rec := range lineage {
		if len(rec.Scores) != 1 {
			t.Errorf("%s row length = %d, want 1", rec.ID, len(rec.Scores))
		}
	}
}

// TestEvoPromptDegenerateSingleParent: when every seed proposal fails
// the quality gate the population holds only the baseline, and both
// per-generation children degrade to single-parent mutation — the
// crossover operator is never dialed.
func TestEvoPromptDegenerateSingleParent(t *testing.T) {
	s := &evoLLM{t: t, initResponses: []string{evoBadVariant}, repair: evoBadVariant}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	var log eventLog
	req := evoRequest(t.TempDir(), eval.NewBudget(0, 0), log.record)
	req.Params = engine.Params{MaxRounds: 1, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 1 {
		t.Fatalf("reason = %s rounds = %d, want rounds_done/1", res.Reason, res.Rounds)
	}
	s.mu.Lock()
	crossovers, mutations, repairs := s.crossoverCalls, s.mutationCalls, s.repairCalls
	s.mu.Unlock()
	if crossovers != 0 {
		t.Errorf("crossover calls = %d, want 0 (population of one)", crossovers)
	}
	if mutations != 2 {
		t.Errorf("mutation calls = %d, want 2 (degenerate single-parent children)", mutations)
	}
	if repairs != 5 {
		t.Errorf("repair calls = %d, want 5 (every bad seed proposal repaired once)", repairs)
	}
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
	if len(lineage) != 3 { // baseline + two mutation children
		t.Fatalf("lineage records = %d, want 3", len(lineage))
	}
	for _, rec := range lineage[1:] {
		if rec.Operator != OpMutation || !slices.Equal(rec.Parents, []string{"baseline"}) {
			t.Errorf("child %s = %+v, want mutation ← baseline", rec.ID, rec)
		}
	}
	// The population events show the degenerate seeding: one member.
	pops := log.byType(EventPopulationUpdated)
	if len(pops) != 2 {
		t.Fatalf("population_updated events = %d, want 2", len(pops))
	}
	if size := pops[0].Detail["size"]; size != 1 {
		t.Errorf("init population size = %v, want 1", size)
	}
	if size := pops[1].Detail["size"]; size != 3 {
		t.Errorf("generation 1 population size = %v, want 3 (baseline + 2 children)", size)
	}
}

// TestEvoPromptAborted: a canceled context ends the run with reason
// aborted before any LLM dial.
func TestEvoPromptAborted(t *testing.T) {
	req := evoRequest(t.TempDir(), eval.NewBudget(0, 0), nil)
	req.Provider = provider.NewOpenAI("http://127.0.0.1:1", "1", provider.OpenAIConfig{}) // never dialed
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res, err := New().Optimize(ctx, req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonAborted || res.Rounds != 0 {
		t.Fatalf("reason = %s rounds = %d, want aborted/0", res.Reason, res.Rounds)
	}
	if res.Best.ID != "baseline" {
		t.Fatalf("best = %s, want baseline", res.Best.ID)
	}
}

// --- unit tests ------------------------------------------------------------------

// TestTournamentDeterministicAndExcluding: identical seeds replay
// identical winner sequences, the excluded index never wins, and the
// weakest member never wins a k=2 tournament it cannot dominate.
func TestTournamentDeterministicAndExcluding(t *testing.T) {
	pop := []individual{
		{cand: core.Candidate{ID: "a"}, fitness: 0.1}, // weakest
		{cand: core.Candidate{ID: "b"}, fitness: 0.9},
		{cand: core.Candidate{ID: "c"}, fitness: 0.5},
		{cand: core.Candidate{ID: "d"}, fitness: 0.9},
	}
	runA := &evoRun{pop: pop, rng: newTestRNG(42)}
	runB := &evoRun{pop: pop, rng: newTestRNG(42)}
	for range 100 {
		a, b := runA.tournament(-1), runB.tournament(-1)
		if a != b {
			t.Fatalf("tournament diverged across same-seed runs: %d vs %d", a, b)
		}
		if a == 0 {
			t.Fatal("the weakest member won a k=2 tournament alone")
		}
	}
	for range 100 {
		if w := runA.tournament(0); w == 0 {
			t.Fatal("excluded index won the tournament")
		}
	}
}

// newTestRNG mirrors the Optimize seeding so unit tests match run
// behavior.
func newTestRNG(seed int64) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(seed), uint64(seed)))
}

// TestTruncateElitism: truncation keeps the fittest members, breaking
// fitness ties by id asc, and never grows the population.
func TestTruncateElitism(t *testing.T) {
	pop := []individual{
		{cand: core.Candidate{ID: "low"}, fitness: 0.1},
		{cand: core.Candidate{ID: "b"}, fitness: 0.9},
		{cand: core.Candidate{ID: "a"}, fitness: 0.9},
		{cand: core.Candidate{ID: "mid"}, fitness: 0.5},
		{cand: core.Candidate{ID: "z"}, fitness: 0.5},
		{cand: core.Candidate{ID: "y"}, fitness: 0.5},
		{cand: core.Candidate{ID: "x"}, fitness: 0.2},
	}
	got := truncate(pop, 6)
	if len(got) != 6 {
		t.Fatalf("truncated size = %d, want 6", len(got))
	}
	// 0.9 pair id-asc, 0.5 trio id-asc, then 0.2 — the 0.1 elite-out.
	wantIDs := []string{"a", "b", "mid", "y", "z", "x"}
	for i, id := range wantIDs {
		if got[i].cand.ID != id {
			t.Fatalf("truncated order = %v, want %v at %d", got[i].cand.ID, id, i)
		}
	}
	// Elitism: a smaller cap keeps the incumbent elites on top.
	if top := truncate(pop, 3); top[0].cand.ID != "a" || top[1].cand.ID != "b" || top[2].cand.ID != "mid" {
		t.Errorf("top-3 = %v, want a,b,mid", top)
	}
	// A cap beyond the population is a no-op.
	if got := truncate(pop[:2], 6); len(got) != 2 {
		t.Errorf("oversized truncate = %d members, want 2", len(got))
	}
}

// TestPrimaryMeanProjectsFixedOrder: the fitness projection follows the
// fixed sample order and counts missing cells as zeros.
func TestPrimaryMeanProjectsFixedOrder(t *testing.T) {
	records := []engine.SampleRecord{
		{Sample: evoSamples[0], Scores: map[string]float64{"exact_match": 1}},
		{Sample: evoSamples[2], Scores: map[string]float64{"exact_match": 1}},
	}
	if got := primaryMean(records, evoSamples, "exact_match"); got != 0.5 {
		t.Errorf("primaryMean = %v, want 0.5 (2 of 4 cells, gaps as 0)", got)
	}
	if got := primaryMean(nil, evoSamples, "exact_match"); got != 0 {
		t.Errorf("primaryMean(nil) = %v, want 0", got)
	}
}
