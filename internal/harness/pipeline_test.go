package harness

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// llmJudgeSpecJSON 是「LLM 自选 llm_judge」的合成规格夹具：--spec-metrics
// 覆盖切口用它验证原主指标不在钉死列表时的置空回落。
const llmJudgeSpecJSON = `{"name":"tcm_judge","description":"从中医医案文本判断证候","prompt_template":"判断证候：{input}","metrics":["llm_judge"],"primary_metric":"llm_judge"}`

// TestPipelineSpecMetricsOverrideBeforeSampleSynthesis 是 D7 覆盖点的
// 顺序切口（PRD-0001 切分 3）：钉死指标必须在 SynthesizeSamples 之前
// 生效——buildSamplesPrompt 整体 marshal spec，样本合成请求体里的规格
// 必须已是覆盖后的（expected 才按新指标出题）；落盘的 spec.json 同血。
// 原 primary=llm_judge 不在钉死列表 → 置空 → Task.Primary() 回落列表
// 首项 f1（不新增 --spec-primary 的回落语义）。
func TestPipelineSpecMetricsOverrideBeforeSampleSynthesis(t *testing.T) {
	dir := t.TempDir()
	runID := "20261009-100000-s"
	synthDir := filepath.Join(filepath.Join(dir, "synth"), runID)

	srv, bodies := startLLM(t, scriptRouter(t,
		func() string { return llmJudgeSpecJSON }, // LLM 自选 llm_judge
		func() string { return samplesJSON },
		func() string { return probesJSON },
		func(body string) string {
			for _, sc := range pipelineScript {
				if strings.Contains(body, sc.input) {
					return sc.expected
				}
			}
			t.Errorf("evaluation call matched no scripted sample: %.300s", body)
			return "{}"
		},
	))

	p := &Pipeline{
		RunID: runID, RunsDir: filepath.Join(dir, "runs"), SynthDir: synthDir,
		Prompt:   "从中医医案文本判断证候",
		Provider: provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Model:    "jiuwei-tcm", MaxTokens: 256, SynthMaxTokens: 256,
		// 钉死列表不含原 primary llm_judge：覆盖 + 置空回落一线 exercising。
		SpecMetrics: []string{"f1", "exact_match"},
		SamplesN:    4, ProbeVariants: 2, Workers: 1,
		Budget: eval.NewBudget(0, 0), Mode: ModeAutopilot,
	}
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}

	// 顺序切口：第 2 次调用是样本合成（第 1 次是 spec），其请求体 marshal
	// 了整份 spec——必须是覆盖后的指标，原 primary 已随置空消失。（请求
	// 体是 JSON-of-JSON，needle 用转义引号字节面。）
	if got := len(*bodies); got < 2 {
		t.Fatalf("llm calls = %d, want at least 2", got)
	}
	samplesPrompt := (*bodies)[1]
	if !strings.Contains(samplesPrompt, MarkerSamples) {
		t.Fatalf("second call is not the samples stage: %.200s", samplesPrompt)
	}
	if !strings.Contains(samplesPrompt, `\"metrics\":[\"f1\",\"exact_match\"]`) {
		t.Errorf("samples prompt spec was not overridden before synthesis:\n%.400s", samplesPrompt)
	}
	if strings.Contains(samplesPrompt, "llm_judge") || strings.Contains(samplesPrompt, "primary_metric") {
		t.Errorf("samples prompt still carries the unpinned metrics or a primary:\n%.400s", samplesPrompt)
	}

	// 落盘 spec.json 同血：指标为钉死列表，primary 置空后 Task.Primary()
	// 回落首项 f1（下游探针/基线/优化读同一份盘上规格）。
	specFile, err := LoadSpec(synthDir)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	if strings.Join(specFile.Task.Metrics, ",") != "f1,exact_match" {
		t.Errorf("saved spec metrics = %v, want [f1 exact_match]", specFile.Task.Metrics)
	}
	if specFile.Task.PrimaryMetric != "" {
		t.Errorf("saved spec primary = %q, want cleared (llm_judge is not in the pinned list)", specFile.Task.PrimaryMetric)
	}
	if got := specFile.Task.Primary(); got != "f1" {
		t.Errorf("Task.Primary() = %q, want the first pinned item f1", got)
	}

	// 基线按钉死指标评估（llm_judge 不在列 → 无裁判调用面）。
	if _, ok := res.MetricMeans["llm_judge"]; ok {
		t.Errorf("baseline means carry llm_judge: %v", res.MetricMeans)
	}
	if res.MetricMeans["f1"] != 1 || res.MetricMeans["exact_match"] != 1 {
		t.Errorf("baseline means = %v, want 1/1 under the pinned metrics", res.MetricMeans)
	}
}

// TestPipelineSpecMetricsInvalidFailsFast：非法指标在 SynthesizeSamples
// 之前快速失败——覆盖后立即 Validate()，绝不烧样本/探针合成预算。
func TestPipelineSpecMetricsInvalidFailsFast(t *testing.T) {
	dir := t.TempDir()
	synthDir := filepath.Join(filepath.Join(dir, "synth"), "20261009-101000-f")
	srv, bodies := startLLM(t, func(_ int, body string) (int, string) {
		switch {
		case strings.Contains(body, MarkerSamples),
			strings.Contains(body, MarkerProbes),
			strings.Contains(body, MarkerRepair):
			t.Errorf("synthesis must stop before the sample/probe stages: %.200s", body)
		}
		// spec 阶段照常应答（合法规格），失败必须来自覆盖校验本身。
		return http.StatusOK, completion(specJSON)
	})
	p := &Pipeline{
		RunID: "20261009-101000-f", RunsDir: filepath.Join(dir, "runs"), SynthDir: synthDir,
		Prompt:   "从中医医案文本判断证候",
		Provider: provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Model:    "jiuwei-tcm", MaxTokens: 256, SynthMaxTokens: 256,
		SpecMetrics: []string{"f1", "made_up_metric"},
		SamplesN:    4, ProbeVariants: 2, Workers: 1,
		Budget: eval.NewBudget(0, 0), Mode: ModeAutopilot,
	}
	_, err := p.Run(context.Background())
	if err == nil {
		t.Fatal("an invalid pinned metric must fail the pipeline")
	}
	if !strings.Contains(err.Error(), "made_up_metric") {
		t.Errorf("error = %v, want the unknown-metric attribution", err)
	}
	if got := len(*bodies); got != 1 {
		t.Errorf("llm calls = %d, want 1 (spec only — samples/probes never synthesized)", got)
	}
	if _, err := os.Stat(filepath.Join(synthDir, "samples.json")); err == nil {
		t.Error("samples.json must not exist when the override fails validation")
	}
}
