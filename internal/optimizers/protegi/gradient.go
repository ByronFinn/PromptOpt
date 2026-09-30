package protegi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// Markers embedded in the ProTeGi meta-prompts — the seam fake LLM
// servers key on to tell the gradient and apply stages apart
// (evaluation prompts never contain them).
const (
	MarkerGradient = "生成文本梯度"
	MarkerApply    = "沿文本梯度改写提示词"
)

// Context truncation bounds (runes), mirroring the engine reflection
// caps: maxGradientRunes caps the gradient fed forward into the apply
// prompt.
const (
	maxGradientRunes     = 600
	maxSampleInputRunes  = 600
	maxResponseRunes     = 600
	maxParentPromptRunes = 3000
)

// gradient runs the textual-gradient stage: the fold's failure
// evidence becomes one natural-language criticism plus a fix
// direction — plain text, the ProTeGi "gradient" is not JSON.
func gradient(ctx context.Context, adv *engine.Advisor, task core.Task, parent core.Candidate, failures []engine.SampleRecord) (string, error) {
	raw, err := adv.Call(ctx, "gradient", buildGradientPrompt(task, parent, failures))
	if err != nil {
		return "", err
	}
	grad := cleanGradient(raw)
	if grad == "" {
		return "", errors.New("文本梯度为空")
	}
	return grad, nil
}

// apply rewrites the parent along the gradient into a whole new
// candidate — the shared engine.ProduceCandidate scaffold (Call →
// Defend through the quality gate, one repair attempt).
func apply(ctx context.Context, adv *engine.Advisor, task core.Task, parent core.Candidate, grad string) (core.Candidate, error) {
	return engine.ProduceCandidate(ctx, adv, "apply", buildApplyPrompt(task, parent, grad))
}

// cleanGradient strips whitespace and stray {input} literals
// (rendering is plain replacement — a literal inside guidance would
// inject sample text) and caps the length.
func cleanGradient(raw string) string {
	return engine.TruncateRunes(strings.TrimSpace(engine.CleanInputLiteral(raw)), maxGradientRunes)
}

// --- meta prompts ---------------------------------------------------------

func buildGradientPrompt(task core.Task, parent core.Candidate, failures []engine.SampleRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, `你是提示词优化专家。当前提示词在部分样本上出错，请%s：一段自然语言批评，指出错误模式，并给出具体改法。

## 任务规格
- 名称：%s
- 描述：%s
- 指标：%s（主指标：%s）
- 原始模板：%s

## 当前提示词
%s

## 失败样本（共 %d 条）
`, MarkerGradient, task.Name, task.Description, strings.Join(task.Metrics, ", "), task.Primary(),
		engine.TruncateRunes(task.PromptTemplate, maxParentPromptRunes),
		engine.TruncateRunes(parent.Prompt, maxParentPromptRunes), len(failures))
	for _, rec := range failures {
		expected, _ := json.Marshal(rec.Sample.Expected)
		fmt.Fprintf(&b, `
### %s
- 输入：%s
- 期望：%s
- 实际输出：%s
- 分数：%s
- 诊断：%s
`,
			rec.Sample.ID,
			engine.TruncateRunes(rec.Sample.Input, maxSampleInputRunes),
			engine.TruncateRunes(string(expected), maxSampleInputRunes),
			engine.TruncateRunes(rec.Response, maxResponseRunes),
			formatScores(rec.Scores),
			formatDiagnosis(rec.Diagnosis),
		)
	}
	fmt.Fprintf(&b, `
请直接输出这段文本梯度（批评 + 改法）：纯文本，不要 JSON、不要代码围栏、不要包含 {input} 占位符字面量。`)
	return b.String()
}

// applyOutputSpec is the candidate wire-format contract shared with
// the engine mutators: one JSON object whose prompt carries the
// {input} placeholder.
const applyOutputSpec = `严格输出 JSON，除 JSON 外不要输出任何其他内容，不要使用代码围栏：
{"id": "短英文id", "name": "候选短名", "description": "一句话说明", "prompt": "完整新提示词"}

要求：
- prompt 必须包含 {input} 占位符，评测时样本输入将原样替换进该位置
- 沿文本梯度的批评与改法改写当前提示词，保持任务目标不变
- 用中文撰写提示词`

func buildApplyPrompt(task core.Task, parent core.Candidate, grad string) string {
	return fmt.Sprintf(`你是提示词工程专家。请%s。

## 任务规格
- 名称：%s
- 描述：%s
- 指标：%s（主指标：%s）

## 当前提示词
%s

## 文本梯度（批评与改法）
%s

%s`,
		MarkerApply, task.Name, task.Description, strings.Join(task.Metrics, ", "), task.Primary(),
		engine.TruncateRunes(parent.Prompt, maxParentPromptRunes),
		grad,
		applyOutputSpec)
}

// formatScores renders a score map deterministically (sorted keys);
// local copies of the engine helpers, which stay unexported there.
func formatScores(scores map[string]float64) string {
	if len(scores) == 0 {
		return "无（记 0）"
	}
	parts := make([]string, 0, len(scores))
	for _, m := range slices.Sorted(maps.Keys(scores)) {
		parts = append(parts, fmt.Sprintf("%s=%.4f", m, scores[m]))
	}
	return strings.Join(parts, " ")
}

func formatDiagnosis(diagnosis map[string]string) string {
	if len(diagnosis) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(diagnosis))
	for _, k := range slices.Sorted(maps.Keys(diagnosis)) {
		parts = append(parts, fmt.Sprintf("%s: %s", k, engine.TruncateRunes(diagnosis[k], 200)))
	}
	return strings.Join(parts, "; ")
}
