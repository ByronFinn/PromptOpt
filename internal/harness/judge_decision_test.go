package harness

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- decision-judge passthrough（P7 用例 ⑦）---------------------------------
//
// A synthesized spec may declare llm_judge; the pipeline's probes and
// baseline (and the later optimization units, via engine.Request) must
// grade through the run's single judge surface — here the decision
// backend, never a silent generative mix.

const decisionAnswerJSON = `{"model":"tev1:0.8b","answers":{"q1":{"type":"score",
"probabilities":{"0":0,"1":0,"2":0,"3":1},"confidence":0.9,"score":3.0,
"legend":{"0":"完全错误","1":"部分正确","2":"基本正确","3":"与参考答案完全等价"},"selected":3}},
"usage":{"input_tokens":123,"output_tokens":1}}`

// judgedSpecJSON is the pipeline spec fixture declaring llm_judge.
const judgedSpecJSON = `{"name":"tcm_zhenghou","description":"从中医医案文本中判断证候名称","prompt_template":"你是中医辨证助手。阅读下面的文本，判断证候名称，只输出证候名称本身。文本：{input}","metrics":["llm_judge"],"primary_metric":"llm_judge"}`

// TestFilterProbeUsesDecisionBackend: a llm_judge spec's probe replay
// grades through the decision client.
func TestFilterProbeUsesDecisionBackend(t *testing.T) {
	var mu sync.Mutex
	decCalls, judgePromptOnMain := 0, 0
	decSrv, _ := startLLM(t, func(_ int, _ string) (int, string) {
		mu.Lock()
		decCalls++
		mu.Unlock()
		// The decision stub answers any body; the client posts to
		// <root>/v1/systemone regardless.
		return http.StatusOK, decisionAnswerJSON
	})
	dec := provider.NewSystemOne(decSrv.URL, "tev1:0.8b", provider.SystemOneConfig{})
	// The main server answers the executor calls and must never see a
	// judge prompt (the generative fallback would leak it there).
	mainSrv, _ := startLLM(t, func(_ int, body string) (int, string) {
		if strings.Contains(body, "评估裁判") {
			mu.Lock()
			judgePromptOnMain++
			mu.Unlock()
		}
		return http.StatusOK, completion(`{"ok": true}`)
	})

	task := core.Task{Name: "judged_synth", PromptTemplate: "你是中医辨证助手。{input}",
		Metrics: []string{eval.MetricLLMJudge}}
	samples := []core.Sample{
		{ID: "s1", Input: "恶寒发热", Expected: "风寒", Split: "train"},
		{ID: "s2", Input: "面红身热", Expected: "风热", Split: "train"},
	}
	f := &Filter{
		RunID: "decision-filter", SynthDir: filepath.Join(t.TempDir(), "synth"),
		Model: "fake-model", MaxTokens: 64, Workers: 1,
		Provider: provider.NewOpenAI(mainSrv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Budget:   eval.NewBudget(0, 0),
		// The decision surface rides the same fields the CLI forwards.
		JudgeBackend:            eval.JudgeBackendDecision,
		DecisionClient:          dec,
		JudgeDecisionConfidence: 0.5,
		JudgeDecisionDiagBelow:  0.6,
	}
	spec := SpecFile{Task: task, Probes: []string{"变体甲：{input}", "变体乙：{input}"}}
	report, err := f.Apply(context.Background(), spec, samples, DefaultThresholds())
	if err != nil {
		t.Fatalf("Filter.Apply: %v", err)
	}
	// 2 probe variants × 2 samples = 4 decision gradings; the main
	// server saw zero judge prompts (no silent generative mix).
	if decCalls != 4 {
		t.Errorf("decision calls = %d, want 4 (2 variants × 2 samples)", decCalls)
	}
	if judgePromptOnMain != 0 {
		t.Errorf("judge prompts on the executor server = %d, want 0", judgePromptOnMain)
	}
	for _, pv := range report.PerSample {
		if pv.Verdict != VerdictDeadEasy {
			t.Errorf("sample %s verdict = %s, want dead-easy (constant 1.0 probes)", pv.ID, pv.Verdict)
		}
	}
}

// TestPipelineBaselineUsesDecisionBackend: the full synthesized
// pipeline (spec/samples/probes synthesis + probes + baseline) grades
// every llm_judge sample through the decision client, and the baseline
// means carry the normalized decision score.
func TestPipelineBaselineUsesDecisionBackend(t *testing.T) {
	var mu sync.Mutex
	decCalls, judgePromptOnMain := 0, 0
	decSrv, _ := startLLM(t, func(_ int, _ string) (int, string) {
		mu.Lock()
		decCalls++
		mu.Unlock()
		return http.StatusOK, decisionAnswerJSON
	})
	dec := provider.NewSystemOne(decSrv.URL, "tev1:0.8b", provider.SystemOneConfig{})
	mainSrv, _ := startLLM(t, func(_ int, body string) (int, string) {
		switch {
		case strings.Contains(body, MarkerSpec):
			return http.StatusOK, completion(judgedSpecJSON)
		case strings.Contains(body, MarkerSamples):
			return http.StatusOK, completion(samplesJSON)
		case strings.Contains(body, MarkerProbes):
			return http.StatusOK, completion(probesJSON)
		case strings.Contains(body, MarkerRepair):
			t.Errorf("unexpected repair call: %.200s", body)
			return http.StatusOK, completion("{}")
		case strings.Contains(body, "评估裁判"):
			mu.Lock()
			judgePromptOnMain++
			mu.Unlock()
			return http.StatusOK, completion(`{"score": 0.75, "diagnosis": "生成式回落，不应发生"}`)
		default:
			return http.StatusOK, completion(`{"ok": true}`)
		}
	})

	dir := t.TempDir()
	p := &Pipeline{
		RunID: "decision-pipeline", RunsDir: filepath.Join(dir, "runs"),
		SynthDir: filepath.Join(dir, "synth", "decision-pipeline"),
		Prompt:   "从中医医案文本判断证候",
		Provider: provider.NewOpenAI(mainSrv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Model:    "fake-model", MaxTokens: 256, SynthMaxTokens: 256,
		// The pipeline口径 follows --judge-backend: probes AND baseline
		// grade through the decision client.
		JudgeBackend:            eval.JudgeBackendDecision,
		DecisionClient:          dec,
		JudgeDecisionConfidence: 0.5,
		JudgeDecisionDiagBelow:  0.6,
		SamplesN:                4, ProbeVariants: 2, Workers: 1,
		Budget: eval.NewBudget(0, 0), Mode: ModeAutopilot,
	}
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("baseline result = %s/exit %d", res.Status, res.ExitCode)
	}
	// 2 probe variants × 4 samples = 8 probe gradings + 4 baseline
	// (constant 1.0 probes classify every sample dead-easy → kept=0 →
	// the full-set fallback) = 12 decision calls; the generative judge
	// prompt never reached the pipeline server.
	if decCalls != 12 {
		t.Errorf("decision calls = %d, want 12 (8 probes + 4 baseline)", decCalls)
	}
	if judgePromptOnMain != 0 {
		t.Errorf("judge prompts on the pipeline server = %d, want 0", judgePromptOnMain)
	}
	if mean := res.MetricMeans[eval.MetricLLMJudge]; mean != 1.0 {
		t.Errorf("baseline llm_judge mean = %v, want 1.0 (trusted decision scores)", mean)
	}
}
