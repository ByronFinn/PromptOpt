package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- decision cascade (P7, httptest-backed, all calls mocked) --------------

// startSystemOne runs a fake /v1/systemone service recording raw request
// bodies; respond decides status and body per call.
func startSystemOne(t *testing.T, respond func(call int, body []byte) (int, string)) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(b))
		call := len(*bodies)
		mu.Unlock()
		status, respBody := respond(call, b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

// systemOneScore builds a score-type answer body: n levels, fixed score
// and confidence, probabilities spreading over the level indexes.
func systemOneScore(score, confidence float64, levels int) string {
	probs := make(map[string]float64, levels)
	legend := make(map[string]string, levels)
	for i := range levels {
		probs[fmt.Sprintf("%d", i)] = 1.0 / float64(levels)
		legend[fmt.Sprintf("%d", i)] = fmt.Sprintf("level-%d", i)
	}
	b, _ := json.Marshal(map[string]any{
		"model": "tev1:0.8b",
		"answers": map[string]any{
			decisionQuestion: map[string]any{
				"type":          "score",
				"probabilities": probs,
				"confidence":    confidence,
				"score":         score,
				"legend":        legend,
				"selected":      levels - 1,
			},
		},
		"usage": map[string]any{"input_tokens": 123, "output_tokens": 1},
	})
	return string(b)
}

// fixtureLevels are the golden fixture's own 3 criteria (systemone-score.json).
var fixtureLevels = []string{"完全错误", "部分正确", "完全正确"}

// decisionEngineOpts tunes the decision-cascade engine harness: separate
// stubs for the executor, the generative judge (the cascade's fallback
// target, wired through JudgeProvider — the P2 isolation chain) and the
// decision service, so routing assertions are per-server.
type decisionEngineOpts struct {
	decision    func(call int, body []byte) (int, string) // decision service answers
	judge       func() (int, string)                      // generative judge answers
	confidence  float64                                   // 0 = default 0.5
	diagBelow   float64                                   // 0 = default 0.6
	levels      []string                                  // nil = default 4 levels
	samples     int
	backend     string // defaults to decision
	zeroBackend bool   // force JudgeBackend "" (the pre-P7 zero value)
	noDecClient bool   // force DecisionClient nil (misconfiguration case)
}

// runDecisionEngine runs the cascade harness and returns the result,
// the decision bodies, the generative-judge call count, events and dir.
func runDecisionEngine(t *testing.T, o decisionEngineOpts) (core.RunResult, []string, int, []Event, string) {
	t.Helper()
	backend := o.backend
	if backend == "" && !o.zeroBackend {
		backend = JudgeBackendDecision
	}
	decSrv, decBodies := startSystemOne(t, o.decision)
	genCalls := 0
	var mu sync.Mutex
	genSrv, _ := startLLM(t, func(_ int, body string) (int, string) {
		mu.Lock()
		genCalls++
		mu.Unlock()
		if o.judge != nil {
			return o.judge()
		}
		return http.StatusOK, completionJSON(`{"score": 0.75, "diagnosis": "部分正确：少抽一个实体"}`, judgeSpend)
	})
	execSrv, _ := startLLM(t, func(_ int, _ string) (int, string) {
		return http.StatusOK, completionJSON(`{"ok": true}`, execSpend)
	})

	var events []Event
	e := &Engine{
		RunID: "decision-run", RunDir: t.TempDir(), Model: "fake-model", MaxTokens: 64,
		Workers: 1, Metrics: []string{MetricLLMJudge},
		Budget:   NewBudget(0, 0),
		Provider: provider.NewOpenAI(execSrv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		// The fallback rides the P2 isolation chain: its own provider
		// instance, so generative-judge calls are countable per server.
		JudgeProvider: provider.NewOpenAI(genSrv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		JudgeModel:    "judge-model",

		JudgeBackend:            backend,
		JudgeDecisionConfidence: o.confidence,
		JudgeDecisionDiagBelow:  o.diagBelow,
		DecisionLevels:          o.levels,
		TaskName:                "task", CandidateID: "cand", DatasetName: "ds",
		OnEvent: func(ev Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	}
	if !o.noDecClient {
		e.DecisionClient = provider.NewSystemOne(decSrv.URL, "tev1:0.8b", provider.SystemOneConfig{})
	}
	res, err := e.Run(t.Context(), core.Candidate{ID: "cand", Prompt: "抽取实体：{input}"}, testSamples(max(o.samples, 1)))
	if err != nil {
		t.Fatalf("Engine.Run: %v", err)
	}
	return res, *decBodies, genCalls, events, e.RunDir
}

// TestDecisionJudgeGoldenFixtureNormalization（用例 ①）: the measured
// tcmsp-30 response (3-level criteria, score 1.386988025801118,
// confidence 0.111453) normalizes by the question's own level count
// minus one — divisor 2 → 0.693494012900559, on the generative judge's
// 0~1 scale. The confidence threshold sits below the fixture's
// 0.111453 and the diag line below the norm so the answer is trusted
// end to end (no fallback call anywhere).
func TestDecisionJudgeGoldenFixtureNormalization(t *testing.T) {
	raw, err := os.ReadFile("../provider/testdata/systemone-score.json")
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	fixture := string(raw)
	res, decBodies, genCalls, _, dir := runDecisionEngine(t, decisionEngineOpts{
		decision: func(_ int, _ []byte) (int, string) {
			return http.StatusOK, fixture
		},
		confidence: 0.1, // fixture confidence 0.111453 ≥ 0.1 → trusted
		diagBelow:  0.6, // norm 0.6935 ≥ 0.6 → trusted (explicit: 0 would read as the default anyway)
		levels:     fixtureLevels,
	})

	if res.ExitCode != 0 {
		t.Fatalf("result = %s/exit %d, want completed/0", res.Status, res.ExitCode)
	}
	want := 1.386988025801118 / 2
	if res.MetricMeans[MetricLLMJudge] != want {
		t.Errorf("llm_judge mean = %.18f, want %.18f (fixture score / (3-1))",
			res.MetricMeans[MetricLLMJudge], want)
	}
	if len(decBodies) != 1 {
		t.Fatalf("decision calls = %d, want 1", len(decBodies))
	}
	if genCalls != 0 {
		t.Errorf("generative judge calls = %d, want 0 (trusted decision answer)", genCalls)
	}
	// Metering: the decision call lands under RoleJudge (123/1) and
	// arms the same valve; the sample folds it into its total.
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 123 || u.CompletionTokens != 1 {
		t.Errorf("judge usage = %+v, want 123/1 (decision call, fixture usage)", u)
	}
	// Trusted answer: deterministic auditable placeholder carrying the
	// confidence value.
	var st core.SampleTrace
	readSampleTrace(t, dir, "001-s-00.json", &st)
	if !strings.Contains(st.Diagnosis[MetricLLMJudge], "决策裁判采信") || !strings.Contains(st.Diagnosis[MetricLLMJudge], "0.1115") {
		t.Errorf("trusted diagnosis = %q, want the confidence-bearing placeholder", st.Diagnosis[MetricLLMJudge])
	}
}

// TestDecisionJudgeFourLevelDivisor（用例 ②）: the default 4-level rubric
// divides by 3 — score 1.5 → 0.5. The diag line is set below 0.5 so the
// trusted path stays observable (the default 0.6 would cascade this
// sample to the generative judge and mask the divisor).
func TestDecisionJudgeFourLevelDivisor(t *testing.T) {
	res, decBodies, genCalls, _, _ := runDecisionEngine(t, decisionEngineOpts{
		decision: func(_ int, _ []byte) (int, string) {
			return http.StatusOK, systemOneScore(1.5, 0.9, 4)
		},
		diagBelow: 0.4, // norm 0.5 ≥ 0.4 → trusted
	})
	if res.ExitCode != 0 {
		t.Fatalf("result = %s/exit %d", res.Status, res.ExitCode)
	}
	if res.MetricMeans[MetricLLMJudge] != 0.5 {
		t.Errorf("llm_judge mean = %v, want 0.5 (score 1.5 / (4-1))", res.MetricMeans[MetricLLMJudge])
	}
	if len(decBodies) != 1 || genCalls != 0 {
		t.Errorf("calls = decision %d / generative %d, want 1/0", len(decBodies), genCalls)
	}
}

// TestDecisionJudgeRequestShape（用例 ③）: the wire request pins the
// re-review contract — model bound to the client, state carrying the
// rubric principle sentence, a name-indexed questions object with one
// score question whose criteria is the non-empty ascending level array.
func TestDecisionJudgeRequestShape(t *testing.T) {
	var reqBody map[string]any
	_, decBodies, genCalls, _, _ := runDecisionEngine(t, decisionEngineOpts{
		decision: func(_ int, _ []byte) (int, string) {
			return http.StatusOK, systemOneScore(3.0, 0.9, 4)
		},
	})
	if len(decBodies) != 1 || genCalls != 0 {
		t.Fatalf("calls = decision %d / generative %d, want 1/0", len(decBodies), genCalls)
	}
	if err := json.Unmarshal([]byte(decBodies[0]), &reqBody); err != nil {
		t.Fatalf("decode request: %v\n%s", err, decBodies[0])
	}
	if reqBody["model"] != "tev1:0.8b" {
		t.Errorf("request model = %v, want the bound decision model", reqBody["model"])
	}
	if reqBody["state"] != decisionState || !strings.Contains(decisionState, "评估裁判") {
		t.Errorf("request state = %v, want the rubric principle sentence", reqBody["state"])
	}
	questions, ok := reqBody["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions is not a name-indexed object: %T", reqBody["questions"])
	}
	q, ok := questions[decisionQuestion].(map[string]any)
	if !ok {
		t.Fatalf("questions[%q] missing: %v", decisionQuestion, questions)
	}
	if q["type"] != "score" {
		t.Errorf("question type = %v, want score", q["type"])
	}
	ins, _ := q["instructions"].(string)
	for _, want := range []string{"【样本输入】", "【参考答案】", "【候选输出】", "中医输入文本编号0"} {
		if !strings.Contains(ins, want) {
			t.Errorf("instructions missing %q: %s", want, ins)
		}
	}
	criteria, ok := q["criteria"].([]any)
	if !ok || len(criteria) == 0 {
		t.Fatalf("criteria not a non-empty array: %v", q["criteria"])
	}
	if len(criteria) != len(decisionLevels) {
		t.Fatalf("criteria len = %d, want %d", len(criteria), len(decisionLevels))
	}
	for i, c := range criteria {
		if c != decisionLevels[i] {
			t.Errorf("criteria[%d] = %v, want %v (ascending)", i, c, decisionLevels[i])
		}
	}
}

// TestDecisionJudgeLowConfidenceFallsBack（用例 ④）: confidence below the
// threshold hands the verdict to the generative judge — each stub
// server sees exactly its own call, the sample score is the generative
// verdict, and both calls' usage folds into the sample under the
// documented roles.
func TestDecisionJudgeLowConfidenceFallsBack(t *testing.T) {
	res, decBodies, genCalls, _, dir := runDecisionEngine(t, decisionEngineOpts{
		decision: func(_ int, _ []byte) (int, string) {
			return http.StatusOK, systemOneScore(3.0, 0.2, 4) // 0.2 < 0.5 default
		},
		// judge default: score 0.75 verdict with judgeSpend 7/3
	})
	if res.ExitCode != 0 {
		t.Fatalf("result = %s/exit %d", res.Status, res.ExitCode)
	}
	if len(decBodies) != 1 || genCalls != 1 {
		t.Fatalf("calls = decision %d / generative %d, want 1/1", len(decBodies), genCalls)
	}
	if res.MetricMeans[MetricLLMJudge] != 0.75 {
		t.Errorf("llm_judge mean = %v, want 0.75 (generative verdict replaces the indecisive answer)", res.MetricMeans[MetricLLMJudge])
	}
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 130 || u.CompletionTokens != 4 {
		t.Errorf("judge usage = %+v, want 130/4 (decision 123/1 + generative 7/3)", u)
	}
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 10 || u.CompletionTokens != 5 {
		t.Errorf("executor usage = %+v, want 10/5", u)
	}
	// Trace pair: the decision probe and the generative verdict are both
	// on the audit timeline, in seq order.
	var sawDecision, sawJudge bool
	for _, ct := range callTraces(t, dir) {
		switch ct.Stage {
		case "judge-decision":
			sawDecision = true
			if ct.Role != core.RoleJudge {
				t.Errorf("decision trace role = %q, want judge", ct.Role)
			}
			if !strings.Contains(ct.Response.Content, `"confidence":0.2`) {
				t.Errorf("decision trace response missing the answer JSON: %.200s", ct.Response.Content)
			}
		case "judge":
			sawJudge = true
		}
	}
	if !sawDecision || !sawJudge {
		t.Errorf("trace pair incomplete: sawDecision=%v sawJudge=%v", sawDecision, sawJudge)
	}
}

// TestDecisionJudgeDiagLine（用例 ⑤）: a low normalized score falls back
// for the Chinese diagnosis (the score half the decision model covers
// is replaced by the coherent generative pair); the inverse case trusts
// the decision answer and its diagnosis is the placeholder.
func TestDecisionJudgeDiagLine(t *testing.T) {
	t.Run("score below the line falls back for the diagnosis", func(t *testing.T) {
		res, decBodies, genCalls, _, dir := runDecisionEngine(t, decisionEngineOpts{
			decision: func(_ int, _ []byte) (int, string) {
				return http.StatusOK, systemOneScore(0.9, 0.9, 4) // norm 0.3 < 0.6
			},
			judge: func() (int, string) {
				return http.StatusOK, completionJSON(`{"score": 0.25, "diagnosis": "明显错误：答非所问"}`, judgeSpend)
			},
		})
		if res.ExitCode != 0 {
			t.Fatalf("result = %s/exit %d", res.Status, res.ExitCode)
		}
		if len(decBodies) != 1 || genCalls != 1 {
			t.Fatalf("calls = decision %d / generative %d, want 1/1", len(decBodies), genCalls)
		}
		if res.MetricMeans[MetricLLMJudge] != 0.25 {
			t.Errorf("llm_judge mean = %v, want 0.25 (generative pair replaces the low score)", res.MetricMeans[MetricLLMJudge])
		}
		var st core.SampleTrace
		readSampleTrace(t, dir, "001-s-00.json", &st)
		if st.Diagnosis[MetricLLMJudge] != "明显错误：答非所问" {
			t.Errorf("diagnosis = %q, want the generative diagnosis", st.Diagnosis[MetricLLMJudge])
		}
	})
	t.Run("score above the line keeps the decision answer and placeholder", func(t *testing.T) {
		res, decBodies, genCalls, _, dir := runDecisionEngine(t, decisionEngineOpts{
			decision: func(_ int, _ []byte) (int, string) {
				return http.StatusOK, systemOneScore(3.0, 0.9, 4) // norm 1.0 ≥ 0.6
			},
		})
		if res.ExitCode != 0 {
			t.Fatalf("result = %s/exit %d", res.Status, res.ExitCode)
		}
		if len(decBodies) != 1 || genCalls != 0 {
			t.Fatalf("calls = decision %d / generative %d, want 1/0", len(decBodies), genCalls)
		}
		if res.MetricMeans[MetricLLMJudge] != 1.0 {
			t.Errorf("llm_judge mean = %v, want 1.0", res.MetricMeans[MetricLLMJudge])
		}
		var st core.SampleTrace
		readSampleTrace(t, dir, "001-s-00.json", &st)
		if !strings.Contains(st.Diagnosis[MetricLLMJudge], "决策裁判采信") || !strings.Contains(st.Diagnosis[MetricLLMJudge], "0.9000") {
			t.Errorf("diagnosis = %q, want the placeholder with confidence 0.9000", st.Diagnosis[MetricLLMJudge])
		}
	})
}

// TestJudgeBackendZeroValueKeepsGenerative（用例 ⑥）: no --judge-backend
// (the zero value) keeps the generative judge even with a decision
// client wired — default runs are byte-identical to the pre-P7 shape.
func TestJudgeBackendZeroValueKeepsGenerative(t *testing.T) {
	res, decBodies, genCalls, _, _ := runDecisionEngine(t, decisionEngineOpts{
		zeroBackend: true,
	})
	if res.ExitCode != 0 {
		t.Fatalf("result = %s/exit %d", res.Status, res.ExitCode)
	}
	if len(decBodies) != 0 || genCalls != 1 {
		t.Errorf("calls = decision %d / generative %d, want 0/1 (zero value = llm)", len(decBodies), genCalls)
	}
	if res.MetricMeans[MetricLLMJudge] != 0.75 {
		t.Errorf("llm_judge mean = %v, want 0.75 (generative)", res.MetricMeans[MetricLLMJudge])
	}
}

// TestDecisionBackendWithoutClientFailsLoudly: a decision-configured
// engine without a client is a loud configuration error, never a
// silent generative downgrade (one judge per run — research 0001 §3.2).
func TestDecisionBackendWithoutClientFailsLoudly(t *testing.T) {
	res, decBodies, genCalls, events, _ := runDecisionEngine(t, decisionEngineOpts{
		noDecClient: true,
	})
	if res.ExitCode != 1 || len(res.FailedSamples) != 1 {
		t.Fatalf("result = %s/exit %d failed=%v, want failed/1", res.Status, res.ExitCode, res.FailedSamples)
	}
	if len(decBodies) != 0 || genCalls != 0 {
		t.Errorf("calls = decision %d / generative %d, want 0/0 (no silent fallback)", len(decBodies), genCalls)
	}
	for _, ev := range events {
		if ev.Type == EventSampleDone && !strings.Contains(ev.Error, "DecisionClient") {
			t.Errorf("event error = %q, want the DecisionClient attribution", ev.Error)
		}
	}
}

// TestDecisionCascadeMixedRunMean（用例 ⑧）: one run mixing trusted
// decision answers and generative fallbacks — both backends' scores
// feed the same mean on the same 0~1 scale.
func TestDecisionCascadeMixedRunMean(t *testing.T) {
	res, decBodies, genCalls, _, dir := runDecisionEngine(t, decisionEngineOpts{
		decision: func(call int, _ []byte) (int, string) {
			if call == 1 {
				return http.StatusOK, systemOneScore(3.0, 0.9, 4) // s-00: trusted → 1.0
			}
			return http.StatusOK, systemOneScore(1.0, 0.1, 4) // s-01: 0.1 < 0.5 → fallback
		},
		samples: 2,
	})
	if res.ExitCode != 0 {
		t.Fatalf("result = %s/exit %d", res.Status, res.ExitCode)
	}
	if len(decBodies) != 2 || genCalls != 1 {
		t.Fatalf("calls = decision %d / generative %d, want 2/1", len(decBodies), genCalls)
	}
	if want := (1.0 + 0.75) / 2; res.MetricMeans[MetricLLMJudge] != want {
		t.Errorf("llm_judge mean = %v, want %v (1.0 decision + 0.75 generative)", res.MetricMeans[MetricLLMJudge], want)
	}
	// Judge-side metering accumulates both backends: 123×2 + 7 = 253
	// prompt, 1×2 + 3 = 5 completion.
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 253 || u.CompletionTokens != 5 {
		t.Errorf("judge usage = %+v, want 253/5 (two decision calls + one generative)", u)
	}
	// Per-sample diagnosis provenance: trusted placeholder vs generative.
	// Sample trace names carry the executor call seq: s-00 → 001 (its
	// decision probe is 002), s-01 → 003 (probe 004, fallback verdict 005).
	for _, name := range []string{"001-s-00.json", "003-s-01.json"} {
		var st core.SampleTrace
		readSampleTrace(t, dir, name, &st)
		diag := st.Diagnosis[MetricLLMJudge]
		if name == "001-s-00.json" && !strings.Contains(diag, "决策裁判采信") {
			t.Errorf("s-00 diagnosis = %q, want the placeholder", diag)
		}
		if name == "002-s-01.json" && diag != "部分正确：少抽一个实体" {
			t.Errorf("s-01 diagnosis = %q, want the generative diagnosis", diag)
		}
	}
}

// readSampleTrace decodes one sample trace from the run dir.
func readSampleTrace(t *testing.T, dir, name string, st *core.SampleTrace) {
	t.Helper()
	b, err := os.ReadFile(dir + "/samples/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, st); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}
