package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- decision-judge passthrough（P7 用例 ⑦）---------------------------------
//
// The optimization loop's unit engine must grade llm_judge through the
// decision backend when the request carries it — the same single-judge
// surface as the baseline and verify sides (research 0001 §3.2), never
// a silent mixed pair within one run.

// decisionTask grades llm_judge through the decision stub: confidence
// 0.9, score 3.0 over the default 4 levels → trusted 1.0.
func startDecisionStub(t *testing.T, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"tev1:0.8b","answers":{"q1":{"type":"score",
			"probabilities":{"0":0,"1":0,"2":0,"3":1},"confidence":0.9,"score":3.0,
			"legend":{"0":"完全错误","1":"部分正确","2":"基本正确","3":"与参考答案完全等价"},"selected":3}},
			"usage":{"input_tokens":123,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// startCountingLLM answers every chat call with a fixed completion and
// counts; used for both the executor side and the never-called
// generative judge side.
func startCountingLLM(t *testing.T, calls *int, mu *sync.Mutex, content string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, content)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestLoopEvaluateForwardsDecisionBackend: the unit eval engine grades
// through the decision stub and never dials the generative judge.
func TestLoopEvaluateForwardsDecisionBackend(t *testing.T) {
	var mu sync.Mutex
	decCalls, genCalls, execCalls := 0, 0, 0
	decSrv := startDecisionStub(t, &decCalls, &mu)
	genSrv := startCountingLLM(t, &genCalls, &mu, `{"score": 0.75, "diagnosis": "生成式"}`)
	execSrv := startCountingLLM(t, &execCalls, &mu, `{"ok": true}`)

	task := core.Task{Name: "judged", PromptTemplate: "基础助手。{input}", Metrics: []string{eval.MetricLLMJudge}}
	dir := t.TempDir()
	req := Request{
		Task:     task,
		Params:   Params{MaxRounds: 2, Minibatch: 1, StagnationLimit: 2, Epsilon: 0, Seed: 7},
		Initial:  core.Candidate{ID: "baseline", Prompt: task.PromptTemplate},
		Samples:  []core.Sample{{ID: "s1", Input: "甲恶寒发热", Expected: "风寒", Split: "train"}},
		Provider: provider.NewOpenAI(execSrv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		// The generative surface is wired (the cascade's fallback
		// target) but a trusted decision answer must never reach it.
		JudgeProvider:           provider.NewOpenAI(genSrv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		JudgeModel:              "judge-model",
		JudgeBackend:            eval.JudgeBackendDecision,
		DecisionClient:          provider.NewSystemOne(decSrv.URL, "tev1:0.8b", provider.SystemOneConfig{}),
		JudgeDecisionConfidence: 0.5,
		JudgeDecisionDiagBelow:  0.6,
		Model:                   "fake-model", MaxTokens: 64, OptMaxTokens: 64, Workers: 1,
		Budget: eval.NewBudget(0, 0),
		RunID:  "decision-loop", RunDir: dir,
	}
	l, err := NewLoop(req)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	res, _, err := l.Evaluate(context.Background(), core.Candidate{ID: "c1", Prompt: task.PromptTemplate}, req.Samples, 1)
	if err != nil {
		t.Fatalf("Loop.Evaluate: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("unit result = %s/exit %d", res.Status, res.ExitCode)
	}
	if decCalls != 1 || execCalls != 1 || genCalls != 0 {
		t.Errorf("calls = decision %d / executor %d / generative %d, want 1/1/0 (no silent judge mix)", decCalls, execCalls, genCalls)
	}
	if mean := res.MetricMeans[eval.MetricLLMJudge]; mean != 1.0 {
		t.Errorf("llm_judge mean = %v, want 1.0 (3.0 / (4-1) … trusted decision answer)", mean)
	}
	// The unit's call trace carries the decision stage.
	entries, err := os.ReadDir(filepath.Join(dir, "evals", "01-c1", "calls"))
	if err != nil {
		t.Fatalf("read unit calls dir: %v", err)
	}
	sawDecision := false
	for _, en := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, "evals", "01-c1", "calls", en.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var ct struct {
			Stage string `json:"stage"`
		}
		if err := json.Unmarshal(raw, &ct); err != nil {
			t.Fatal(err)
		}
		if ct.Stage == "judge-decision" {
			sawDecision = true
		}
	}
	if !sawDecision {
		t.Error("unit call traces missing the judge-decision stage")
	}
}
