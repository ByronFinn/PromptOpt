package miprov2

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

var miproTask = core.Task{
	Name: "zhenghou", Description: "从主诉文本判断证候",
	PromptTemplate: "基础助手。{input}", Metrics: []string{"exact_match"},
}

// miproSamples: three train splits (the demo pool's priority source)
// and three dev splits.
var miproSamples = []core.Sample{
	{ID: "s1", Input: "样例甲", Expected: "甲对", Split: "train"},
	{ID: "s2", Input: "样例乙", Expected: "乙对", Split: "train"},
	{ID: "s3", Input: "样例丙", Expected: "丙对", Split: "train"},
	{ID: "s4", Input: "样例丁", Expected: "丁对", Split: "dev"},
	{ID: "s5", Input: "样例戊", Expected: "戊对", Split: "dev"},
	{ID: "s6", Input: "样例己", Expected: "己对", Split: "dev"},
}

const (
	// miproPropose: four valid variants; the golden run makes every
	// family all-correct so the frontier story is clone-rejection.
	miproPropose = `{"instructions":[
 "一号指令：直接给出正确答案。{input}",
 "二号指令：先分析再作答。{input}",
 "三号指令：逐字精确匹配。{input}",
 "四号指令：按格式规范作答。{input}"
]}`

	// miproProposeMixed: two valid variants (all-correct vs. hopeless)
	// plus two without the {input} placeholder — normalization drops
	// those, so every round draws both survivors and winner selection
	// is exercised head-to-head.
	miproProposeMixed = `{"instructions":[
 "上策指令：直接给出正确答案。{input}",
 "平凡指令：随意作答。{input}",
 "三号没有占位符",
 "四号没有占位符"
]}`
)

// miproEvalFamilies maps instruction markers to the bitmask of samples
// they answer correctly without demos (bit i = s(i+1)); order matters.
var miproEvalFamilies = []struct {
	marker string
	bits   int
}{
	{"一号指令", 0b111111},
	{"二号指令", 0b111111},
	{"三号指令", 0b111111},
	{"四号指令", 0b111111},
	{"上策指令", 0b111111},
	{"平凡指令", 0},
}

// miproEval scripts the executor answer: the instruction family sets
// the base competence and any demo embedded ahead of the injection
// point teaches that sample's answer — few-shot behavior the joint
// search is supposed to exploit.
func miproEval(body string) string {
	mask := 0
	for _, f := range miproEvalFamilies {
		if strings.Contains(body, f.marker) {
			mask = f.bits
			break
		}
	}
	cur := miproCurrent(body)
	prefix := body[:strings.LastIndex(body, miproSamples[cur].Input)]
	for i, s := range miproSamples {
		if strings.Contains(prefix, s.Input) {
			mask |= 1 << i
		}
	}
	if mask&(1<<cur) != 0 {
		return miproSamples[cur].Expected.(string)
	}
	return "答错"
}

// miproCurrent identifies the live sample: its input is injected at
// the {input} position, after the demo block, so the latest occurrence
// wins even when the same sample is also embedded as a demo.
func miproCurrent(body string) int {
	best, at := 0, -1
	for i, s := range miproSamples {
		if p := strings.LastIndex(body, s.Input); p > at {
			best, at = i, p
		}
	}
	return best
}

// startMiproLLM routes by the proposal markers first, then by the
// scripted evaluation answers; usage is role-scripted so the budget
// accounting is exactly predictable.
func startMiproLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		content, usage := func() (string, [2]int) {
			switch {
			case strings.Contains(body, MarkerProposeFix):
				t.Errorf("unexpected repair call: %.200s", body)
				return "{}", [2]int{100, 50}
			case strings.Contains(body, MarkerPropose):
				return miproPropose, [2]int{100, 50}
			default:
				return miproEval(body), [2]int{10, 5}
			}
		}()
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			content, usage[0], usage[1], usage[0]+usage[1])
	}))
}

func miproParams() engine.Params {
	return engine.Params{MaxRounds: 4, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 11}
}

func miproBaselineRecords(samples []core.Sample) []engine.SampleRecord {
	out := make([]engine.SampleRecord, len(samples))
	for i, s := range samples {
		out[i] = engine.SampleRecord{
			Sample: s, Response: "答错",
			Scores: map[string]float64{"exact_match": 0},
		}
	}
	return out
}

func miproRequest(dir string, budget *eval.Budget, onEvent func(eval.Event)) engine.Request {
	return engine.Request{
		Task: miproTask, Params: miproParams(),
		Initial:  core.Candidate{ID: "baseline", Prompt: miproTask.PromptTemplate},
		Samples:  miproSamples,
		Baseline: miproBaselineRecords(miproSamples),
		Provider: nil, // filled by the caller
		Model:    "fake-model", MaxTokens: 1024, OptMaxTokens: 1024, Workers: 2,
		Budget:  budget,
		RunID:   "mipro", RunDir: dir,
		OnEvent: onEvent,
	}
}

// eventLog records engine events; the engine emits from worker
// goroutines, so the slice is guarded (gepa_test.go precedent).
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

// TestMiprov2GoldenLoop drives the full optimizer against the scripted
// LLM: every instruction family answers everything, so round 1's m01
// evicts the baseline and m02–m04 land as score-clone rejections.
// Asserts events, artifacts, usage and same-seed determinism.
func TestMiprov2GoldenLoop(t *testing.T) {
	srv := startMiproLLM(t)

	var log eventLog
	dir := t.TempDir()
	req := miproRequest(dir, eval.NewBudget(0, 0), log.record)
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 4 {
		t.Fatalf("reason = %s rounds = %d, want rounds_done/4", res.Reason, res.Rounds)
	}
	if res.Best.ID != "m01" || res.BestMeans["exact_match"] != 1 {
		t.Fatalf("best = %s mean = %v, want m01/1", res.Best.ID, res.BestMeans["exact_match"])
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "m01" {
		t.Errorf("frontier = %v, want only m01", res.Frontier)
	}
	if !res.ConstraintSatisfied {
		t.Error("no constraint declared —satisfied must hold")
	}

	// One proposal call, four variants.
	pd := log.byType(EventProposeDone)
	if len(pd) != 1 {
		t.Fatalf("propose_done events = %d, want 1", len(pd))
	}
	if got := pd[0].Detail["instructions"]; got != 4 {
		t.Errorf("propose_done instructions = %v, want 4", got)
	}
	if pd[0].Detail["fallback"] != false {
		t.Error("propose_done fallback = true, want false")
	}
	if pd[0].Detail["paradigm"] != "miprov2" || pd[0].Detail["stage"] != "propose" {
		t.Errorf("propose_done detail = %+v, want paradigm=miprov2 stage=propose", pd[0].Detail)
	}

	// Round cadence: start → frontier_updated → joint_done → done.
	r1 := func(typ string) int { return log.indexOf(typ, "round", 1) }
	for _, pair := range [][2]string{
		{engine.EventRoundStart, engine.EventFrontierUpdated},
		{engine.EventFrontierUpdated, EventJointDone},
		{EventJointDone, engine.EventRoundDone},
	} {
		a, b := r1(pair[0]), r1(pair[1])
		if a < 0 || b < 0 || a >= b {
			t.Errorf("round 1 order broken: %s(%d) before %s(%d)", pair[0], a, pair[1], b)
		}
	}
	// Every event type the docs contract requires is present.
	if n := len(log.byType(engine.EventRoundDone)); n != 4 {
		t.Errorf("round_done events = %d, want 4", n)
	}
	if n := len(log.byType(engine.EventFrontierUpdated)); n != 4 {
		t.Errorf("frontier_updated events = %d, want 4", n)
	}
	// The engine never emits run_done/run_start (SSE/replay contract).
	if n := len(log.byType(eval.EventRunDone)); n != 0 {
		t.Errorf("run_done count = %d, want 0", n)
	}
	if n := len(log.byType(eval.EventRunStart)); n != 0 {
		t.Errorf("run_start count = %d, want 0", n)
	}
	// Sample events carry the candidate/round stamp.
	for _, ev := range log.byType(eval.EventSampleDone) {
		if ev.Detail["candidate"] == nil || ev.Detail["round"] == nil {
			t.Fatalf("sample_done missing stamp: %+v", ev.Detail)
		}
	}

	// Joint outcomes: m01 admitted, the rest are score-clone rejects.
	joints := log.byType(EventJointDone)
	if len(joints) != 4 {
		t.Fatalf("joint_done events = %d, want 4", len(joints))
	}
	for i, ev := range joints {
		if ev.Detail["combos"] != 3 {
			t.Errorf("round %d combos = %v, want 3", i+1, ev.Detail["combos"])
		}
		if ev.Detail["demos"] != 2 {
			t.Errorf("round %d demos = %v, want 2 (k=min(2, len(pool)-1))", i+1, ev.Detail["demos"])
		}
		if ev.Detail["paradigm"] != "miprov2" || ev.Detail["stage"] != "joint" {
			t.Errorf("round %d detail = %+v, want paradigm/stage", i+1, ev.Detail)
		}
	}
	if joints[0].Detail["admitted"] != true || joints[0].Detail["candidate"] != "m01" {
		t.Errorf("round 1 joint = %+v, want m01 admitted", joints[0].Detail)
	}
	for _, ev := range joints[1:] {
		if ev.Detail["admitted"] != false {
			t.Errorf("clone round joint = %+v, want admitted=false", ev.Detail)
		}
	}

	// lineage.json: baseline plus four joint children.
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(dir, "lineage.json"), &lineage)
	if len(lineage) != 5 {
		t.Fatalf("lineage records = %d, want 5", len(lineage))
	}
	if lineage[0].ID != "baseline" || lineage[0].Operator != engine.OpBaseline {
		t.Fatalf("baseline record = %+v", lineage[0])
	}
	byID := map[string]engine.LineageRecord{}
	for _, rec := range lineage {
		byID[rec.ID] = rec
	}
	m01 := byID["m01"]
	if m01.Operator != OpJoint || !slices.Equal(m01.Parents, []string{"baseline"}) || !m01.Admitted {
		t.Errorf("m01 = %+v, want joint ← baseline admitted", m01)
	}
	if !slices.Equal(m01.Scores, []float64{1, 1, 1, 1, 1, 1}) || m01.PrimaryMean != 1 {
		t.Errorf("m01 scores/mean = %v/%v", m01.Scores, m01.PrimaryMean)
	}
	if len(m01.Hypotheses) != 1 {
		t.Fatalf("m01 hypotheses = %+v, want one audit entry", m01.Hypotheses)
	}
	if len(m01.Hypotheses[0].SampleIDs) != 2 {
		t.Errorf("m01 hypothesis sample_ids = %v, want the two demo ids", m01.Hypotheses[0].SampleIDs)
	}
	for i := 2; i <= 4; i++ {
		if rec := byID[fmt.Sprintf("m%02d", i)]; rec.Admitted || rec.Incomplete {
			t.Errorf("m%02d admitted/incomplete = %v/%v, want false/false (clone)", i, rec.Admitted, rec.Incomplete)
		}
	}

	// The winner's prompt carries the demo block ahead of exactly one
	// {input} placeholder.
	m01Prompt := memberPrompt(t, dir, "m01")
	if !strings.Contains(m01Prompt, "以下是若干已解决的参考示例") {
		t.Error("m01 prompt missing the demo block")
	}
	if n := strings.Count(m01Prompt, "{input}"); n != 1 {
		t.Errorf("m01 prompt {input} count = %d, want 1", n)
	}

	// frontier.json and report.md carry the deliverable.
	var frontier engine.FrontierFile
	loadJSON(t, filepath.Join(dir, "frontier.json"), &frontier)
	if frontier.Best.ID != "m01" || frontier.Primary != "exact_match" {
		t.Errorf("frontier.json best = %+v", frontier.Best)
	}
	if len(frontier.Members) != 1 || frontier.Members[0].PrimaryMean != 1 {
		t.Errorf("frontier members = %+v, want only m01 at 1", frontier.Members)
	}
	report, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	for _, anchor := range []string{"最优提示词", "参考示例", "m01"} {
		if !strings.Contains(string(report), anchor) {
			t.Errorf("report.md missing anchor %q", anchor)
		}
	}

	// Evaluation units: 4 rounds × (3 probes + 1 winner); optimizer
	// calls: exactly the one proposal.
	units, err := os.ReadDir(filepath.Join(dir, "evals"))
	if err != nil || len(units) != 16 {
		t.Errorf("eval units = %d (err %v), want 16", len(units), err)
	}
	calls, err := os.ReadDir(filepath.Join(dir, "opt-calls"))
	if err != nil || len(calls) != 1 {
		t.Errorf("opt calls = %d (err %v), want 1 (propose)", len(calls), err)
	}

	// Same seed replays: lineage and frontier are structurally
	// identical once the time fields are stripped.
	var log2 eventLog
	dir2 := t.TempDir()
	req2 := miproRequest(dir2, eval.NewBudget(0, 0), log2.record)
	req2.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})
	if _, err := New().Optimize(t.Context(), req2); err != nil {
		t.Fatalf("replay Optimize: %v", err)
	}
	assertDeterminism(t, dir, dir2)
}

// memberPrompt loads one member's prompt from frontier.json (the
// member view carries the candidate prompt for self-contained
// downstream tooling).
func memberPrompt(t *testing.T, dir, id string) string {
	t.Helper()
	var frontier engine.FrontierFile
	loadJSON(t, filepath.Join(dir, "frontier.json"), &frontier)
	for _, m := range frontier.Members {
		if m.ID == id {
			return m.Prompt
		}
	}
	t.Fatalf("member %s not on the frontier", id)
	return ""
}

// TestMiprov2InstructionFilteringAndWinner: variants without {input}
// are dropped (S shrinks accordingly) and the head-to-head winner
// selection is exercised every round — both survivors are drawn and
// only the all-correct family may win, or a losing child's row would
// show demo-taught bits instead of six correct cells.
func TestMiprov2InstructionFilteringAndWinner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		content := miproProposeMixed
		if !strings.Contains(body, MarkerPropose) && !strings.Contains(body, MarkerProposeFix) {
			content = miproEval(body)
		}
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`, content)
	}))
	t.Cleanup(srv.Close)

	var log eventLog
	req := miproRequest(t.TempDir(), eval.NewBudget(0, 0), log.record)
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone {
		t.Fatalf("reason = %s, want rounds_done", res.Reason)
	}
	pd := log.byType(EventProposeDone)
	if len(pd) != 1 || pd[0].Detail["instructions"] != 2 {
		t.Fatalf("propose_done = %+v, want instructions=2 (placeholder-less dropped)", pd)
	}
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
	for _, rec := range lineage[1:] {
		if !slices.Equal(rec.Scores, []float64{1, 1, 1, 1, 1, 1}) {
			t.Errorf("%s row = %v, want all-correct (winner must be the 上策 family)", rec.ID, rec.Scores)
		}
	}
	if res.Best.ID != "m01" {
		t.Errorf("best = %s, want m01", res.Best.ID)
	}
}

// TestMiprov2ProposeRepair: a garbage first proposal is repaired by
// the single Defend call — two opt-call traces land, four variants
// survive.
func TestMiprov2ProposeRepair(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		content := "这完全不是 JSON 格式的回答"
		if strings.Contains(body, MarkerProposeFix) {
			content = miproPropose
		} else if !strings.Contains(body, MarkerPropose) {
			content = miproEval(body)
		}
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`, content)
	}))
	t.Cleanup(srv.Close)

	var log eventLog
	req := miproRequest(t.TempDir(), eval.NewBudget(0, 0), log.record)
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone {
		t.Fatalf("reason = %s, want rounds_done", res.Reason)
	}
	if pd := log.byType(EventProposeDone); len(pd) != 1 || pd[0].Detail["instructions"] != 4 {
		t.Errorf("propose_done = %+v, want 4 repaired variants", pd)
	}
	calls, err := os.ReadDir(filepath.Join(req.RunDir, "opt-calls"))
	if err != nil || len(calls) != 2 {
		t.Fatalf("opt calls = %d (err %v), want 2 (propose + repair)", len(calls), err)
	}
}

// TestMiprov2BudgetRoleAccounting: one proposal meters exactly 100/50
// optimizer tokens; one round meters 3 probes × 4 minibatch samples
// plus the winner's 6 full-row samples of executor usage. Optimizer
// spend never arms the executor soft stop.
func TestMiprov2BudgetRoleAccounting(t *testing.T) {
	srv := startMiproLLM(t)
	budget := eval.NewBudget(0, 0)
	req := miproRequest(t.TempDir(), budget, nil)
	req.Params.MaxRounds = 1
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone {
		t.Fatalf("reason = %s, want rounds_done", res.Reason)
	}
	opt := res.Usage[core.RoleOptimizer]
	if opt.PromptTokens != 100 || opt.CompletionTokens != 50 {
		t.Errorf("optimizer usage = %+v, want 100/50 (1 proposal)", opt)
	}
	exe := res.Usage[core.RoleExecutor]
	if exe.PromptTokens != 180 || exe.CompletionTokens != 90 {
		t.Errorf("executor usage = %+v, want 180/90 (18 calls × 10/5)", exe)
	}
	if budget.SoftStopped() {
		t.Error("no executor token limit was set — soft stop must stay disarmed")
	}
}

// TestMiprov2OptValveBudgetStopped: an optimizer token valve covering
// exactly one proposal stops the loop before round 1 with reason
// budget_stopped and the baseline delivered.
func TestMiprov2OptValveBudgetStopped(t *testing.T) {
	srv := startMiproLLM(t)
	budget := eval.NewBudget(0, 0)
	var log eventLog
	req := miproRequest(t.TempDir(), budget, log.record)
	req.OptBudgetTokens = 150 // exactly one proposal call
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonBudgetStopped || res.Rounds != 0 {
		t.Fatalf("reason/rounds = %s/%d, want budget_stopped/0", res.Reason, res.Rounds)
	}
	if res.Best.ID != "baseline" || res.Best.Prompt == "" {
		t.Fatalf("best = %+v, want non-empty baseline delivery", res.Best)
	}
	if budget.SoftStopped() {
		t.Error("optimizer-only spend must not arm the executor soft stop")
	}
	found := false
	for _, ev := range log.byType(eval.EventBudgetStop) {
		if ev.Detail["role"] == string(core.RoleOptimizer) {
			found = true
		}
	}
	if !found {
		t.Error("no budget_stop event with role=optimizer")
	}
	_, usage := budget.Snapshot()
	if u := usage[core.RoleOptimizer]; u.Total() != 150 {
		t.Errorf("optimizer usage = %d, want exactly one 150-token call", u.Total())
	}
}

// TestMiprov2EvalLimitBudgetStopped: an eval limit that dries up mid
// scoring (probe 1 of 3 truncated, probe 3 empty) still promotes the
// fully scored pair, but the winner's full evaluation gets nothing
// dispatched — it is marked Incomplete and stays off the frontier.
func TestMiprov2EvalLimitBudgetStopped(t *testing.T) {
	srv := startMiproLLM(t)
	budget := eval.NewBudget(0, 5)
	var log eventLog
	req := miproRequest(t.TempDir(), budget, log.record)
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonBudgetStopped || res.Rounds != 1 {
		t.Fatalf("reason/rounds = %s/%d, want budget_stopped/1", res.Reason, res.Rounds)
	}
	if res.Best.ID != "baseline" {
		t.Fatalf("best = %s, want baseline (the incomplete child is not deliverable)", res.Best.ID)
	}
	if evals, _ := budget.Snapshot(); evals != 5 {
		t.Errorf("evals started = %d, want exactly the 5 allowed", evals)
	}
	if len(res.Frontier) != 1 || res.Frontier[0].ID() != "baseline" {
		t.Errorf("frontier = %v, want only baseline", res.Frontier)
	}
	var lineage []engine.LineageRecord
	loadJSON(t, filepath.Join(req.RunDir, "lineage.json"), &lineage)
	if len(lineage) != 2 || lineage[1].ID != "m01" {
		t.Fatalf("lineage = %+v, want baseline + m01", lineage)
	}
	if !lineage[1].Incomplete || lineage[1].Admitted {
		t.Errorf("m01 incomplete/admitted = %v/%v, want true/false", lineage[1].Incomplete, lineage[1].Admitted)
	}
	// The incomplete admission is visible on the event stream.
	incomplete := false
	for _, ev := range log.byType(engine.EventFrontierUpdated) {
		if ev.Detail["candidate"] == "m01" && ev.Detail["incomplete"] == true {
			incomplete = true
		}
	}
	if !incomplete {
		t.Error("no frontier_updated event flagging m01 as incomplete")
	}
}

// TestMiprov2KeptOne: a single retained sample leaves a pool of ≤1 —
// no demo is embedded (pure instruction search) and every minibatch
// falls back to the whole retained set; no empty batch, no division by
// zero, the run completes.
func TestMiprov2KeptOne(t *testing.T) {
	srv := startMiproLLM(t)
	req := miproRequest(t.TempDir(), eval.NewBudget(0, 0), nil)
	req.Samples = miproSamples[:1]
	req.Baseline = miproBaselineRecords(miproSamples[:1])
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

	res, err := New().Optimize(t.Context(), req)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if res.Reason != engine.ReasonRoundsDone || res.Rounds != 4 {
		t.Fatalf("reason/rounds = %s/%d, want rounds_done/4", res.Reason, res.Rounds)
	}
	if res.Best.ID != "m01" || res.BestMeans["exact_match"] != 1 {
		t.Fatalf("best = %s mean = %v, want m01/1", res.Best.ID, res.BestMeans["exact_match"])
	}
	if strings.Contains(res.Best.Prompt, "参考示例") {
		t.Error("kept=1 must degrade to pure instruction search — no demo block")
	}
	if n := strings.Count(res.Best.Prompt, "{input}"); n != 1 {
		t.Errorf("best prompt {input} count = %d, want 1", n)
	}
}

// TestMiprov2AbortedByContext: cancellation maps to reason=aborted
// before any provider call — not even the opt-calls trail exists.
func TestMiprov2AbortedByContext(t *testing.T) {
	srv := startMiproLLM(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := miproRequest(t.TempDir(), eval.NewBudget(0, 0), nil)
	req.Provider = provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1})

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
	if calls, err := os.ReadDir(filepath.Join(req.RunDir, "opt-calls")); err == nil && len(calls) > 0 {
		t.Errorf("opt calls = %d, want none on an aborted-before-start run", len(calls))
	}
}

// --- pure-function units ----------------------------------------------------

func TestDemoPoolOrderAndCap(t *testing.T) {
	samples := []core.Sample{
		{ID: "a", Split: "dev"},
		{ID: "b", Split: "train"},
		{ID: "c", Split: "train"},
		{ID: "d", Split: ""},
		{ID: "e", Split: "train"},
	}
	ids := func(pool []core.Sample) []string {
		out := make([]string, len(pool))
		for i, s := range pool {
			out[i] = s.ID
		}
		return out
	}
	if got := ids(demoPool(samples, 3)); !slices.Equal(got, []string{"b", "c", "e"}) {
		t.Errorf("cap 3 pool = %v, want train-first [b c e]", got)
	}
	if got := ids(demoPool(samples, 4)); !slices.Equal(got, []string{"b", "c", "e", "a"}) {
		t.Errorf("cap 4 pool = %v, want [b c e a]", got)
	}
	if got := ids(demoPool(samples, 8)); !slices.Equal(got, []string{"b", "c", "e", "a", "d"}) {
		t.Errorf("uncapped pool = %v, want [b c e a d]", got)
	}
}

func TestDrawDemos(t *testing.T) {
	pool := miproSamples
	rng := rand.New(rand.NewPCG(1, 1))
	d := drawDemos(rng, pool, 2)
	if len(d) != 2 {
		t.Fatalf("drew %d demos, want 2 (k=min(2, len(pool)-1))", len(d))
	}
	if d[0].ID == d[1].ID {
		t.Error("demos must be distinct")
	}
	// Selected demos keep pool order.
	if slices.Index(pool, d[0]) > slices.Index(pool, d[1]) {
		t.Errorf("demo order %v not pool order", []string{d[0].ID, d[1].ID})
	}
	if got := drawDemos(rng, pool[:2], 2); len(got) != 1 {
		t.Errorf("pool of 2 drew %d demos, want 1", len(got))
	}
	if got := drawDemos(rng, pool[:1], 2); got != nil {
		t.Errorf("pool of 1 drew %v, want nil", got)
	}
	if got := drawDemos(rng, nil, 2); got != nil {
		t.Errorf("empty pool drew %v, want nil", got)
	}
	// Same seed, same draw.
	a := drawDemos(rand.New(rand.NewPCG(7, 7)), pool, 2)
	b := drawDemos(rand.New(rand.NewPCG(7, 7)), pool, 2)
	if !slices.Equal(a, b) {
		t.Errorf("same seed diverged: %v vs %v", a, b)
	}
}

func TestDrawCombosDeterministic(t *testing.T) {
	instructions := []string{"一。{input}", "二。{input}", "三。{input}", "四。{input}"}
	combos := drawCombos(rand.New(rand.NewPCG(3, 3)), instructions, miproSamples)
	if len(combos) != 3 {
		t.Fatalf("combos = %d, want 3 (S=min(3, I))", len(combos))
	}
	seen := map[int]bool{}
	for _, c := range combos {
		if seen[c.instr] {
			t.Errorf("instruction #%d drawn twice", c.instr)
		}
		seen[c.instr] = true
		if c.instruction != instructions[c.instr] {
			t.Errorf("combo instruction mismatch: %q", c.instruction)
		}
	}
	replay := drawCombos(rand.New(rand.NewPCG(3, 3)), instructions, miproSamples)
	for i := range combos {
		if combos[i].instr != replay[i].instr || !slices.Equal(combos[i].demos, replay[i].demos) {
			t.Errorf("combo %d diverged across same-seed draws", i)
		}
	}
}

func TestComposePrompt(t *testing.T) {
	demos := []core.Sample{{
		ID: "d1", Input: "带 {input} 字面量的输入", Expected: map[string]any{"a": 1},
	}}
	got := composePrompt("开头。{input} 结尾", demos)
	// The demo block lands before the first {input}.
	if !strings.HasPrefix(got, "开头。\n以下是若干已解决的参考示例") {
		t.Errorf("demo block not inserted before the first {input}: %q", got)
	}
	if !strings.HasSuffix(got, "{input} 结尾") {
		t.Errorf("instruction tail lost: %q", got)
	}
	// A literal {input} inside demo text is stripped so it cannot
	// capture the injection point; JSON braces in expected are inert.
	if n := strings.Count(got, "{input}"); n != 1 {
		t.Errorf("{input} count = %d, want 1 (demo literal cleaned)", n)
	}
	if !strings.Contains(got, `{"a":1}`) {
		t.Errorf("JSON expected braces mangled: %q", got)
	}
	// Two placeholders: insertion still targets the first.
	two := composePrompt("前 {input} 中 {input} 后", demos)
	if n := strings.Count(two, "{input}"); n != 2 {
		t.Errorf("two-placeholder count = %d, want 2", n)
	}
	if !strings.HasPrefix(two, "前 \n以下是若干已解决的参考示例") {
		t.Errorf("demo block not before the first placeholder: %q", two)
	}
	// No demos: instruction unchanged.
	if got := composePrompt("开头。{input}", nil); got != "开头。{input}" {
		t.Errorf("no-demo compose changed the instruction: %q", got)
	}
}

func TestMinibatchFrom(t *testing.T) {
	kept := miproSamples
	demos := kept[:2] // s1, s2
	rng := rand.New(rand.NewPCG(5, 5))
	batch := minibatchFrom(rng, kept, demos, 4)
	if len(batch) != 4 {
		t.Fatalf("batch = %d, want 4", len(batch))
	}
	for _, s := range batch {
		if s.ID == demos[0].ID || s.ID == demos[1].ID {
			t.Errorf("demo %s leaked into its own scoring minibatch", s.ID)
		}
	}
	// Full exclusion falls back to the whole retained set — the batch
	// is never empty (kept=1 degenerate case).
	if got := minibatchFrom(rng, kept, kept, 4); len(got) != len(kept) {
		t.Errorf("all-excluded fallback = %d samples, want %d", len(got), len(kept))
	}
	// k wider than the remainder returns it unchanged, in order.
	rest := minibatchFrom(rng, kept, demos, 99)
	if len(rest) != 4 || rest[0].ID != "s3" {
		t.Errorf("wide-k batch = %v, want the remainder in kept order", rest)
	}
	// Same seed, same batch.
	a := minibatchFrom(rand.New(rand.NewPCG(9, 9)), kept, demos, 3)
	b := minibatchFrom(rand.New(rand.NewPCG(9, 9)), kept, demos, 3)
	if !slices.Equal(a, b) {
		t.Errorf("same seed diverged: %v vs %v", a, b)
	}
}

func TestNormalizeInstructions(t *testing.T) {
	pool := []string{"", "没有占位符", "甲。{input}", "甲。{input}", "乙。{input}", "丙。{input}", "丁。{input}", "戊。{input}"}
	got := normalizeInstructions(pool, 4)
	want := []string{"甲。{input}", "乙。{input}", "丙。{input}", "丁。{input}"}
	if !slices.Equal(got, want) {
		t.Errorf("normalized = %v, want %v (invalid dropped, deduped, clipped)", got, want)
	}
	if got := normalizeInstructions([]string{"缺占位符"}, 4); len(got) != 0 {
		t.Errorf("all-invalid pool = %v, want empty", got)
	}
}

func TestPickWinner(t *testing.T) {
	scored := []comboScore{
		{pair: combo{instr: 0}, mean: 0.5},
		{pair: combo{instr: 1}, mean: 0.9},
		{pair: combo{instr: 2}, mean: 0.7},
	}
	if got := pickWinner(scored); got.pair.instr != 1 {
		t.Errorf("winner = #%d, want #1 (argmax)", got.pair.instr)
	}
	tied := []comboScore{
		{pair: combo{instr: 0}, mean: 1},
		{pair: combo{instr: 1}, mean: 1},
	}
	if got := pickWinner(tied); got.pair.instr != 0 {
		t.Errorf("tie winner = #%d, want #0 (earlier draw)", got.pair.instr)
	}
}

// --- helpers ---------------------------------------------------------------

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

// assertDeterminism decodes lineage.json/frontier.json from both dirs,
// strips the time fields and compares structurally.
func assertDeterminism(t *testing.T, dirA, dirB string) {
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
