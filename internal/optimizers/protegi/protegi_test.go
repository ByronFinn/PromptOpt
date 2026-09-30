package protegi

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
	goldenGradient = "文本梯度：模型把主诉罗列成症状，没有映射到标准证候名。改法：先识别主诉，再输出且仅输出标准证候名。"

	goldenApplyV1 = `{"id":"tg1","name":"梯度版一","description":"沿梯度改写","prompt":"梯度版一助手。先识别主诉，输出标准证候名。{input}"}`
	goldenApplyV2 = `{"id":"tg2","name":"梯度版二","description":"沿梯度再改写","prompt":"梯度版二助手。主诉映射证候名对照后仅输出证候名。{input}"}`
)

// goldenEvalMasks maps child-prompt markers to the bitmask of samples
// they answer correctly (bit i = sample s(i+1)).
var goldenEvalMasks = []struct {
	marker string
	bits   int
}{
	{"梯度版二助手", 0b1111},              // v2 → [1,1,1,1]
	{"梯度版一助手", (1 << 0) | (1 << 1)}, // v1 → [1,1,0,0]
}

// goldenEval scripts the executor answer for one rendered prompt; the
// baseline prompt answers s1 only.
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

// newFakeLLM serves one scripted route over the OpenAI wire format;
// usage is role-scripted so budget accounting is exactly predictable.
func newFakeLLM(t *testing.T, route func(body string) (string, [2]int)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		content, usage := route(string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			content, usage[0], usage[1], usage[0]+usage[1])
	}))
}

// startGoldenLLM routes by stage markers first (gradient before apply
// before the scripted evaluation answers); v1-parent applies produce
// v2 unless clone is set, in which case every apply returns v1.
func startGoldenLLM(t *testing.T, clone bool, gradientText string) *httptest.Server {
	t.Helper()
	return newFakeLLM(t, func(body string) (string, [2]int) {
		switch {
		case strings.Contains(body, MarkerGradient):
			return gradientText, [2]int{100, 50}
		case strings.Contains(body, MarkerApply):
			if !clone && strings.Contains(body, "梯度版一助手") {
				return goldenApplyV2, [2]int{100, 50}
			}
			return goldenApplyV1, [2]int{100, 50}
		case strings.Contains(body, engine.MarkerCandFix), strings.Contains(body, engine.MarkerHypRepair):
			t.Errorf("unexpected repair call: %.200s", body)
			return "{}", [2]int{100, 50}
		default:
			return goldenEval(body), [2]int{10, 5}
		}
	})
}

func goldenParams() engine.Params {
	return engine.Params{MaxRounds: 4, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
}

func goldenBaselineRecords() []engine.SampleRecord {
	out := make([]engine.SampleRecord, len(goldenSamples))
	for i, s := range goldenSamples {
		resp, score := "答错", 0.0
		if i == 0 {
			resp, score = "风寒", 1
		}
		out[i] = engine.SampleRecord{Sample: s, Response: resp, Scores: map[string]float64{"exact_match": score}}
	}
	return out
}

func goldenRequest(dir string, budget *eval.Budget, onEvent func(eval.Event)) engine.Request {
	return engine.Request{
		Task: goldenTask, Params: goldenParams(),
		Initial:  core.Candidate{ID: "baseline", Prompt: goldenTask.PromptTemplate},
		Samples:  goldenSamples,
		Baseline: goldenBaselineRecords(),
		Model:    "fake-model", MaxTokens: 1024, OptMaxTokens: 1024, Workers: 2,
		Budget: budget,
		RunID:  "golden", RunDir: dir,
		OnEvent: onEvent,
	}
}

func goldenProvider(t *testing.T, srv *httptest.Server) provider.Provider {
	t.Helper()
	return provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})
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

// TestProTeGiGoldenLoop drives the full optimizer against the scripted
// LLM. Whatever fold split seed 7 produces, the trace converges the
// same way: round 1 admits p01 (v1 prompt, [1,1,0,0], evicting the
// [1,0,0,0] baseline), a later round admits the v2 child
// ([1,1,1,1], evicting everything) and the remaining rounds skip —
// the all-pass parent leaves the chosen fold without failure
// evidence. Asserts events, artifacts, budget accounting and
// determinism.
func TestProTeGiGoldenLoop(t *testing.T) {
	srv := startGoldenLLM(t, false, goldenGradient)

	var log eventLog
	dir := t.TempDir()
	req := goldenRequest(dir, eval.NewBudget(0, 0), log.record)
	req.Provider = goldenProvider(t, srv)

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 4 {
		t.Fatalf("reason = %s rounds = %d, want rounds_done/4", res.Reason, res.Rounds)
	}
	// Best is the v2 child: full marks, prompt carries the placeholder.
	if res.Best.Prompt == "" || !strings.Contains(res.Best.Prompt, "{input}") {
		t.Fatalf("best prompt = %q, want non-empty with {input}", res.Best.Prompt)
	}
	if !strings.Contains(res.Best.Prompt, "梯度版二助手") {
		t.Fatalf("best prompt = %q, want the v2 rewrite", res.Best.Prompt)
	}
	if res.BestMeans["exact_match"] != 1 {
		t.Errorf("best primary mean = %v, want 1", res.BestMeans["exact_match"])
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != res.Best.ID {
		t.Errorf("frontier = %v, want only the best %s", res.Frontier, res.Best.ID)
	}

	// The engine never emits run_done/run_start: inner unit lifecycle
	// stays internal so SSE/replay never close early.
	if n := len(log.byType(eval.EventRunDone)); n != 0 {
		t.Errorf("run_done count = %d, want 0 (engine layer owns none)", n)
	}
	if n := len(log.byType(eval.EventRunStart)); n != 0 {
		t.Errorf("run_start count = %d, want 0 (inner filtered)", n)
	}

	// Event order within round 1: start → gradient → apply(mutate) →
	// frontier → done, monotonically increasing indices.
	r1 := func(typ string) int { return log.indexOf(typ, "round", 1) }
	for _, pair := range [][2]string{
		{engine.EventRoundStart, EventGradientDone},
		{EventGradientDone, engine.EventMutateDone},
		{engine.EventMutateDone, engine.EventFrontierUpdated},
		{engine.EventFrontierUpdated, engine.EventRoundDone},
	} {
		a, b := r1(pair[0]), r1(pair[1])
		if a < 0 || b < 0 || a >= b {
			t.Errorf("round 1 order broken: %s(%d) before %s(%d)", pair[0], a, pair[1], b)
		}
	}

	// Two productive rounds: gradient → apply twice; the other two
	// close skipped (no failure evidence on the fold).
	grads := log.byType(EventGradientDone)
	if len(grads) != 2 {
		t.Fatalf("gradient_done events = %d, want 2", len(grads))
	}
	for _, ev := range grads {
		if ev.Detail["paradigm"] != "protegi" || ev.Detail["stage"] != "gradient" {
			t.Errorf("gradient_done contract detail = %+v, want paradigm/stage", ev.Detail)
		}
		if f, _ := ev.Detail["failures"].(int); f < 1 {
			t.Errorf("gradient_done failures = %v, want >= 1 (only failing samples feed the gradient)", ev.Detail["failures"])
		}
	}
	muts := log.byType(engine.EventMutateDone)
	if len(muts) != 2 {
		t.Fatalf("mutate_done events = %d, want 2", len(muts))
	}
	for _, ev := range muts {
		if ev.Detail["operator"] != OpGradient {
			t.Errorf("mutate_done operator = %v, want %s", ev.Detail["operator"], OpGradient)
		}
	}
	rounds := log.byType(engine.EventRoundDone)
	if len(rounds) != 4 {
		t.Fatalf("round_done events = %d, want 4", len(rounds))
	}
	skipped := 0
	for _, ev := range rounds {
		if s, _ := ev.Detail["skipped"].(bool); s {
			skipped++
		}
	}
	if skipped != 2 {
		t.Errorf("skipped rounds = %d, want 2 (all-pass folds carry no failure evidence)", skipped)
	}
	if ev := log.byType(engine.EventRoundStart)[0]; ev.Detail["paradigm"] != "protegi" {
		t.Errorf("round_start paradigm = %v, want protegi", ev.Detail["paradigm"])
	}

	// Sample events carry the candidate/round stamp.
	for _, ev := range log.byType(eval.EventSampleDone) {
		if ev.Detail["candidate"] == nil || ev.Detail["round"] == nil {
			t.Fatalf("sample_done missing stamp: %+v", ev.Detail)
		}
	}

	// lineage.json: baseline plus two gradient children. Round 1's
	// child is p01 ([1,1,0,0], admitted, evicting the baseline); the
	// v2 child lands [1,1,1,1] and is the delivered best.
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(dir, "lineage.json"), &lineage)
	if len(lineage) != 3 {
		t.Fatalf("lineage records = %d, want 3", len(lineage))
	}
	if lineage[0].ID != "baseline" || lineage[0].Operator != engine.OpBaseline {
		t.Fatalf("baseline record = %+v", lineage[0])
	}
	p01 := lineage[1]
	if p01.ID != "p01" || p01.Operator != OpGradient || !slices.Equal(p01.Parents, []string{"baseline"}) {
		t.Errorf("p01 = %+v, want gradient ← baseline", p01)
	}
	if !slices.Equal(p01.Scores, []float64{1, 1, 0, 0}) || !p01.Admitted || p01.PrimaryMean != 0.5 {
		t.Errorf("p01 scores/admitted/mean = %v/%v/%v", p01.Scores, p01.Admitted, p01.PrimaryMean)
	}
	v2 := lineage[2]
	if v2.Operator != OpGradient || !slices.Equal(v2.Parents, []string{p01.ID}) {
		t.Errorf("v2 child = %+v, want gradient ← %s", v2, p01.ID)
	}
	if !slices.Equal(v2.Scores, []float64{1, 1, 1, 1}) || !v2.Admitted || v2.PrimaryMean != 1 {
		t.Errorf("v2 scores/admitted/mean = %v/%v/%v", v2.Scores, v2.Admitted, v2.PrimaryMean)
	}
	if v2.ID != res.Best.ID {
		t.Errorf("lineage v2 id = %s, want the delivered best %s", v2.ID, res.Best.ID)
	}

	// frontier.json: best is the v2 child; only it remains.
	var frontier engine.FrontierFile
	loadJSON(t, filepath.Join(dir, "frontier.json"), &frontier)
	if frontier.Best.ID != res.Best.ID || frontier.Primary != "exact_match" {
		t.Errorf("frontier.json best = %+v", frontier.Best)
	}
	if len(frontier.Members) != 1 || frontier.Members[0].ID != res.Best.ID || frontier.Members[0].PrimaryMean != 1 {
		t.Errorf("frontier members = %+v", frontier.Members)
	}

	// report.md carries the Top-1 prompt in full plus the trade-off
	// and lineage sections.
	report, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	text := string(report)
	for _, anchor := range []string{"最优提示词", "梯度版二助手", "取舍说明", "前沿成员", "谱系", res.Best.ID} {
		if !strings.Contains(text, anchor) {
			t.Errorf("report.md missing anchor %q", anchor)
		}
	}

	// Evaluation units: two full retained-set children.
	units, err := os.ReadDir(filepath.Join(dir, "evals"))
	if err != nil || len(units) != 2 {
		t.Errorf("eval units = %d (err %v), want 2", len(units), err)
	}
	// Optimizer calls: 2 rounds × (gradient + apply).
	calls, err := os.ReadDir(filepath.Join(dir, "opt-calls"))
	if err != nil || len(calls) != 4 {
		t.Errorf("opt calls = %d (err %v), want 4", len(calls), err)
	}

	// Budget roles: optimizer 4 × 100/50, executor 8 × 10/5.
	opt := res.Usage[core.RoleOptimizer]
	if opt.PromptTokens != 400 || opt.CompletionTokens != 200 {
		t.Errorf("optimizer usage = %+v, want 400/200 (4 calls × 100/50)", opt)
	}
	exe := res.Usage[core.RoleExecutor]
	if exe.PromptTokens != 80 || exe.CompletionTokens != 40 {
		t.Errorf("executor usage = %+v, want 80/40 (8 calls × 10/5)", exe)
	}

	// Same seed replays: lineage and frontier are structurally
	// identical once the time fields are stripped.
	var log2 eventLog
	dir2 := t.TempDir()
	req2 := goldenRequest(dir2, eval.NewBudget(0, 0), log2.record)
	req2.Provider = goldenProvider(t, srv)
	if _, err := New().Optimize(t.Context(), req2); err != nil {
		t.Fatalf("replay Optimize: %v", err)
	}
	assertArtifactDeterminism(t, dir, dir2)
}

// assertArtifactDeterminism decodes lineage.json/frontier.json from
// both dirs, deletes created_at/generated_at and compares structurally.
func assertArtifactDeterminism(t *testing.T, dirA, dirB string) {
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
		t.Errorf("lineage.json diverged across same-seed runs:\nA: %s\nB: %s", truncateForLog(linA), truncateForLog(linB))
	}

	var fA, fB engine.FrontierFile
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

// TestProTeGiCloneChildRejected: when every apply returns the same
// prompt, the second child repeats p01's score row and the frontier
// must reject it as a score clone while the lineage keeps the trail.
func TestProTeGiCloneChildRejected(t *testing.T) {
	srv := startGoldenLLM(t, true, goldenGradient)
	req := goldenRequest(t.TempDir(), eval.NewBudget(0, 0), nil)
	req.Provider = goldenProvider(t, srv)

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 4 {
		t.Fatalf("reason = %s rounds = %d, want rounds_done/4", res.Reason, res.Rounds)
	}
	if res.Best.ID != "p01" || res.BestMeans["exact_match"] != 0.5 {
		t.Fatalf("best = %s (%v), want p01 (0.5)", res.Best.ID, res.BestMeans)
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "p01" {
		t.Fatalf("frontier = %v, want only p01", res.Frontier)
	}
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
	if len(lineage) < 3 {
		t.Fatalf("lineage records = %d, want >= 3 (baseline, p01 and at least one clone)", len(lineage))
	}
	clones := 0
	for _, rec := range lineage[2:] {
		if rec.Admitted || rec.Incomplete {
			t.Errorf("clone %s admitted = %v incomplete = %v, want rejected", rec.ID, rec.Admitted, rec.Incomplete)
		}
		if slices.Equal(rec.Scores, []float64{1, 1, 0, 0}) {
			clones++
		}
	}
	if clones == 0 {
		t.Errorf("no rejected clone with p01's score row among %+v", lineage[2:])
	}
}

// TestProTeGiEmptyGradientSkipsRounds: a whitespace-only gradient is
// not a usable gradient — the round closes skipped with the error,
// later rounds still run and the baseline stays deliverable.
func TestProTeGiEmptyGradientSkipsRounds(t *testing.T) {
	srv := startGoldenLLM(t, false, " \n\t ")
	var log eventLog
	req := goldenRequest(t.TempDir(), eval.NewBudget(0, 0), log.record)
	req.Provider = goldenProvider(t, srv)

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 4 {
		t.Fatalf("reason = %s rounds = %d, want rounds_done/4", res.Reason, res.Rounds)
	}
	if res.Best.ID != "baseline" || res.Best.Prompt == "" {
		t.Fatalf("best = %+v, want non-empty baseline delivery", res.Best)
	}
	if n := len(log.byType(EventGradientDone)); n != 0 {
		t.Errorf("gradient_done events = %d, want 0", n)
	}
	for _, ev := range log.byType(engine.EventRoundDone) {
		if s, _ := ev.Detail["skipped"].(bool); !s || !strings.Contains(fmt.Sprint(ev.Detail["error"]), "文本梯度为空") {
			t.Errorf("round_done = %+v, want skipped with the empty-gradient error", ev.Detail)
		}
	}
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
	if len(lineage) != 1 || lineage[0].ID != "baseline" {
		t.Fatalf("lineage = %+v, want only the baseline row", lineage)
	}
}

// --- budget accounting -------------------------------------------------------

// TestProTeGiBudgetRoleAccounting: the shared budget meters optimizer
// calls (gradient + apply in a 1-round run) and executor calls (1
// child × 4 samples) exactly, and optimizer spend never arms the
// executor soft stop.
func TestProTeGiBudgetRoleAccounting(t *testing.T) {
	srv := startGoldenLLM(t, false, goldenGradient)
	budget := eval.NewBudget(0, 0)
	req := goldenRequest(t.TempDir(), budget, nil)
	req.Params = engine.Params{MaxRounds: 1, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.Provider = goldenProvider(t, srv)

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone {
		t.Fatalf("reason = %s, want rounds_done", res.Reason)
	}
	opt := res.Usage[core.RoleOptimizer]
	if opt.PromptTokens != 200 || opt.CompletionTokens != 100 {
		t.Errorf("optimizer usage = %+v, want 200/100 (2 calls × 100/50)", opt)
	}
	exe := res.Usage[core.RoleExecutor]
	if exe.PromptTokens != 40 || exe.CompletionTokens != 20 {
		t.Errorf("executor usage = %+v, want 40/20 (4 calls × 10/5)", exe)
	}
	if budget.SoftStopped() {
		t.Error("no executor token limit was set — soft stop must stay disarmed")
	}
}

// TestProTeGiOptValveBudgetStopped: --budget-opt-tokens covering
// exactly one optimizer call stops the loop at the apply stage with
// reason budget_stopped and a non-empty best.
func TestProTeGiOptValveBudgetStopped(t *testing.T) {
	srv := startGoldenLLM(t, false, goldenGradient)
	budget := eval.NewBudget(0, 0)
	var log eventLog
	req := goldenRequest(t.TempDir(), budget, log.record)
	req.Params = engine.Params{MaxRounds: 3, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.OptBudgetTokens = 150 // one optimizer call = exactly 150 tokens
	req.Provider = goldenProvider(t, srv)

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonBudgetStopped {
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
	found := false
	for _, ev := range log.byType(eval.EventBudgetStop) {
		if ev.Detail["role"] == string(core.RoleOptimizer) {
			found = true
		}
	}
	if !found {
		t.Errorf("no budget_stop event with role=optimizer among %+v", log.byType(eval.EventBudgetStop))
	}
	// The gradient landed; the apply never dialed.
	if n := len(log.byType(EventGradientDone)); n != 1 {
		t.Errorf("gradient_done events = %d, want 1", n)
	}
	if _, usage := budget.Snapshot(); usage[core.RoleOptimizer].Total() != 150 {
		t.Errorf("optimizer usage = %d, want exactly one 150-token call", usage[core.RoleOptimizer].Total())
	}
	calls, err := os.ReadDir(filepath.Join(req.RunDir, "opt-calls"))
	if err != nil || len(calls) != 1 {
		t.Errorf("opt calls = %d (err %v), want 1 (gradient only)", len(calls), err)
	}
}

// TestProTeGiExecutorEvalLimitBudgetStopped: an eval limit that runs
// dry mid-child ends the run with reason budget_stopped; the
// half-evaluated child never joins the frontier and the lineage flags
// it incomplete.
func TestProTeGiExecutorEvalLimitBudgetStopped(t *testing.T) {
	srv := startGoldenLLM(t, false, goldenGradient)
	budget := eval.NewBudget(0, 3) // 3 of the child's 4 samples
	req := goldenRequest(t.TempDir(), budget, nil)
	req.Params = engine.Params{MaxRounds: 2, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	req.Provider = goldenProvider(t, srv)

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonBudgetStopped {
		t.Fatalf("reason = %s, want budget_stopped", res.Reason)
	}
	if res.Rounds != 1 {
		t.Errorf("rounds = %d, want 1 (stopped during round 1)", res.Rounds)
	}
	if res.Best.ID != "baseline" || res.Best.Prompt == "" {
		t.Fatalf("best = %+v, want non-empty baseline delivery", res.Best)
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "baseline" {
		t.Fatalf("frontier = %v, want only baseline", res.Frontier)
	}
	if evals, _ := budget.Snapshot(); evals != 3 {
		t.Errorf("evals started = %d, want exactly the 3 allowed", evals)
	}
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
	if len(lineage) != 2 || lineage[1].ID != "p01" {
		t.Fatalf("lineage = %+v, want baseline + p01", lineage)
	}
	if !lineage[1].Incomplete || lineage[1].Admitted {
		t.Errorf("p01 incomplete/admitted = %v/%v, want true/false", lineage[1].Incomplete, lineage[1].Admitted)
	}
}

// TestProTeGiKeptSingleSample: a single-sample retained set degenerates
// to one fold and still runs the full gradient loop — one productive
// round (the failing baseline yields failure evidence) then an
// all-pass skip.
func TestProTeGiKeptSingleSample(t *testing.T) {
	srv := startGoldenLLM(t, false, goldenGradient)
	var log eventLog
	single := goldenSamples[:1]
	req := goldenRequest(t.TempDir(), eval.NewBudget(0, 0), log.record)
	req.Samples = single
	req.Baseline = []engine.SampleRecord{{Sample: single[0], Response: "答错", Scores: map[string]float64{"exact_match": 0}}}
	req.Provider = goldenProvider(t, srv)

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 4 {
		t.Fatalf("reason = %s rounds = %d, want rounds_done/4", res.Reason, res.Rounds)
	}
	// Round 1: failing baseline → gradient → v1 child answers s1 → [1]
	// evicts the [0] baseline; later rounds skip on the all-pass fold.
	if res.Best.ID != "p01" || res.BestMeans["exact_match"] != 1 {
		t.Fatalf("best = %s (%v), want p01 (1)", res.Best.ID, res.BestMeans)
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "p01" {
		t.Fatalf("frontier = %v, want only p01", res.Frontier)
	}
	if n := len(log.byType(EventGradientDone)); n != 1 {
		t.Errorf("gradient_done events = %d, want 1", n)
	}
	skipped := 0
	for _, ev := range log.byType(engine.EventRoundDone) {
		if s, _ := ev.Detail["skipped"].(bool); s {
			skipped++
		}
	}
	if skipped != 3 {
		t.Errorf("skipped rounds = %d, want 3", skipped)
	}
	calls, err := os.ReadDir(filepath.Join(req.RunDir, "opt-calls"))
	if err != nil || len(calls) != 2 {
		t.Errorf("opt calls = %d (err %v), want 2 (one gradient + one apply)", len(calls), err)
	}
}

// TestProTeGiAbortedByContext: cancellation maps to reason=aborted
// while still delivering the baseline best.
func TestProTeGiAbortedByContext(t *testing.T) {
	srv := startGoldenLLM(t, false, goldenGradient)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := goldenRequest(t.TempDir(), eval.NewBudget(0, 0), nil)
	req.Provider = goldenProvider(t, srv)

	res, err := New().Optimize(ctx, req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonAborted {
		t.Fatalf("reason = %s, want aborted", res.Reason)
	}
	if res.Best.ID != "baseline" {
		t.Fatalf("best = %s, want baseline", res.Best.ID)
	}
}

// --- unit tests: bandit, folds, prompts --------------------------------------

// TestBanditUCB1Steps walks the bandit by hand: unvisited arms go
// first in index order, exploitation follows the mean once every arm
// was played, and the exploration bonus can outvote a higher mean.
func TestBanditUCB1Steps(t *testing.T) {
	b := newBandit(3)
	// Unvisited priority, index order.
	if got := b.Select(); got != 0 {
		t.Fatalf("first select = %d, want 0", got)
	}
	b.Update(0, 1)
	if got := b.Select(); got != 1 {
		t.Fatalf("second select = %d, want 1", got)
	}
	b.Update(1, 0)
	if got := b.Select(); got != 2 {
		t.Fatalf("third select = %d, want 2", got)
	}
	b.Update(2, 0.5)
	// All arms played once: equal bonuses, the highest mean wins.
	if got := b.Select(); got != 0 {
		t.Errorf("exploit select = %d, want 0 (mean 1 beats 0 and 0.5)", got)
	}
	// Reward is the fold lift and may be negative.
	b.Update(0, -1)
	if got := b.Select(); got != 2 {
		t.Errorf("after negative update select = %d, want 2 (mean 0.5)", got)
	}
}

// TestBanditUCB1ExplorationFlip: with arm0 at 3 pulls (mean 0.6) and
// arm1 at 1 pull (mean 0.5), T=4 gives arm1 the larger exploration
// bonus — score0 = 0.6+√(2ln4/3) ≈ 1.561 < score1 = 0.5+√(2ln4/1) ≈
// 2.165 — so the bandit must pick arm1 despite its lower mean.
func TestBanditUCB1ExplorationFlip(t *testing.T) {
	b := newBandit(2)
	for range 3 {
		b.Update(0, 0.6)
	}
	b.Update(1, 0.5)
	if got := b.Select(); got != 1 {
		t.Errorf("select = %d, want 1 (exploration bonus flips the argmax)", got)
	}
}

// TestBanditSingleArm: the kept=1 degeneration — one arm always wins.
func TestBanditSingleArm(t *testing.T) {
	b := newBandit(1)
	for range 3 {
		if got := b.Select(); got != 0 {
			t.Fatalf("select = %d, want 0", got)
		}
		b.Update(0, -0.25)
	}
}

// TestMakeFoldsPartition: the folds cover every sample exactly once,
// sizes stay within one of each other, and k clamps to the set size
// (kept=1 → a single fold).
func TestMakeFoldsPartition(t *testing.T) {
	samples := make([]core.Sample, 5)
	for i := range samples {
		samples[i] = core.Sample{ID: fmt.Sprintf("s%d", i+1)}
	}
	folds := makeFolds(samples, 2, rand.New(rand.NewPCG(1, 1)))
	if len(folds) != 2 {
		t.Fatalf("folds = %d, want 2", len(folds))
	}
	var got []string
	for i, fold := range folds {
		if len(fold) == 0 {
			t.Fatalf("fold %d is empty", i)
		}
		for _, s := range fold {
			got = append(got, s.ID)
		}
	}
	if len(got) != 5 {
		t.Fatalf("fold sizes = %d total, want 5", len(got))
	}
	slices.Sort(got)
	want := []string{"s1", "s2", "s3", "s4", "s5"}
	if !slices.Equal(got, want) {
		t.Errorf("fold union = %v, want %v", got, want)
	}

	// k above the set size clamps to singleton folds.
	f3 := makeFolds(samples[:3], 4, rand.New(rand.NewPCG(1, 1)))
	if len(f3) != 3 {
		t.Fatalf("folds = %d, want 3 (k clamped to len)", len(f3))
	}
	for i, fold := range f3 {
		if len(fold) != 1 {
			t.Errorf("fold %d size = %d, want 1", i, len(fold))
		}
	}

	// kept=1: one fold holding the single sample.
	f1 := makeFolds(samples[:1], 4, rand.New(rand.NewPCG(1, 1)))
	if len(f1) != 1 || len(f1[0]) != 1 || f1[0][0].ID != "s1" {
		t.Fatalf("single-sample folds = %+v, want one fold [s1]", f1)
	}
}

// TestMakeFoldsSeedReproducible: the same seed yields the same fold
// order; mutating the input afterwards never bleeds into the folds
// (they alias one shuffled copy, not the caller's slice).
func TestMakeFoldsSeedReproducible(t *testing.T) {
	samples := make([]core.Sample, 6)
	for i := range samples {
		samples[i] = core.Sample{ID: fmt.Sprintf("s%d", i+1)}
	}
	a := makeFolds(samples, 3, rand.New(rand.NewPCG(7, 7)))
	b := makeFolds(samples, 3, rand.New(rand.NewPCG(7, 7)))
	if !reflect.DeepEqual(a, b) {
		t.Errorf("same-seed folds diverged:\nA: %v\nB: %v", a, b)
	}

	// Input isolation: folds hold their own copy.
	samples[0].ID = "mutated"
	if slices.Contains(foldIDs(a), "mutated") {
		t.Errorf("mutating the input leaked into folds: %v", foldIDs(a))
	}
}

func foldIDs(folds [][]core.Sample) []string {
	var ids []string
	for _, fold := range folds {
		for _, s := range fold {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// TestFoldArms: small retained sets collapse to few arms, large ones
// cap at maxFoldArms.
func TestFoldArms(t *testing.T) {
	for n, want := range map[int]int{1: 1, 2: 1, 3: 1, 4: 2, 5: 2, 7: 3, 8: 4, 100: 4} {
		if got := foldArms(n); got != want {
			t.Errorf("foldArms(%d) = %d, want %d", n, got, want)
		}
	}
}

// TestCleanGradient: stray {input} literals are stripped and long
// gradients truncate at 600 runes (plus the ellipsis).
func TestCleanGradient(t *testing.T) {
	if got := cleanGradient("  改进方向 {input} 保留  "); got != "改进方向  保留" {
		t.Errorf("cleanGradient = %q, want %q", got, "改进方向  保留")
	}
	long := strings.Repeat("梯", 700)
	got := cleanGradient(long)
	if n := len([]rune(got)); n != maxGradientRunes+1 {
		t.Errorf("truncated gradient runes = %d, want %d (cap + ellipsis)", n, maxGradientRunes+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("truncated gradient must end with the ellipsis")
	}
	if got := cleanGradient("   "); got != "" {
		t.Errorf("whitespace-only gradient = %q, want empty", got)
	}
}

// TestBuildGradientPromptFeedsFailuresOnly: the gradient prompt
// carries the marker, the task spec, the parent prompt and the failing
// sample's evidence.
func TestBuildGradientPromptFeedsFailuresOnly(t *testing.T) {
	failures := []engine.SampleRecord{{
		Sample: goldenSamples[1], Response: "答错",
		Scores: map[string]float64{"exact_match": 0},
	}}
	prompt := buildGradientPrompt(goldenTask, core.Candidate{ID: "baseline", Prompt: "基础助手。{input}"}, failures)
	for _, want := range []string{
		MarkerGradient, "tcm_zhenghou", "基础助手", goldenSamples[1].ID,
		goldenSamples[1].Input, "心火", "答错", "exact_match=0.0000",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("gradient prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestBuildApplyPromptEmbedsGradient: the apply prompt carries the
// marker, the parent, the gradient text and the candidate JSON
// contract (with the {input} requirement).
func TestBuildApplyPromptEmbedsGradient(t *testing.T) {
	grad := "批评：输出太啰嗦。改法：只输出证候名。"
	prompt := buildApplyPrompt(goldenTask, core.Candidate{ID: "baseline", Prompt: "基础助手。{input}"}, grad)
	for _, want := range []string{
		MarkerApply, "基础助手", grad, `{input}`, `"prompt"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("apply prompt missing %q:\n%s", want, prompt)
		}
	}
}
