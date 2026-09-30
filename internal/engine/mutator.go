package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// Mutator produces child candidates with three operators: Rewrite
// (parent + selected hypothesis + ancestor lessons), Merge (parent +
// frontier complement, the hypothesis as merge directive) and Fresh
// (task description only, the random restart). Every product passes
// the quality gate — non-empty prompt carrying the {input}
// placeholder — with one repair call before failing.
type Mutator struct {
	adv *Advisor
}

// NewMutator returns a mutator dialing through adv.
func NewMutator(adv *Advisor) *Mutator { return &Mutator{adv: adv} }

// Rewrite rewrites the parent under the selected hypothesis.
func (m *Mutator) Rewrite(ctx context.Context, task core.Task, parent core.Candidate, sel Hypothesis, lessons []string) (core.Candidate, error) {
	return m.produce(ctx, "rewrite", buildRewritePrompt(task, parent, sel, lessons))
}

// Merge merges the parent with its frontier complement; the selected
// hypothesis steers what the merge should preserve.
func (m *Mutator) Merge(ctx context.Context, task core.Task, parent, complement core.Candidate, sel Hypothesis, lessons []string) (core.Candidate, error) {
	return m.produce(ctx, "merge", buildMergePrompt(task, parent, complement, sel, lessons))
}

// Fresh redesigns the prompt from the task description alone — the
// random restart of the VistaGuard ladder.
func (m *Mutator) Fresh(ctx context.Context, task core.Task) (core.Candidate, error) {
	return m.produce(ctx, "fresh", buildFreshPrompt(task))
}

// produce runs one mutation call plus the quality gate and its single
// repair attempt — the shared engine.ProduceCandidate scaffold.
func (m *Mutator) produce(ctx context.Context, stage, prompt string) (core.Candidate, error) {
	return ProduceCandidate(ctx, m.adv, stage, prompt)
}

// --- meta prompts ---------------------------------------------------------

const mutationOutputSpec = `严格输出 JSON，除 JSON 外不要输出任何其他内容，不要使用代码围栏：
{"id": "短英文id", "name": "候选短名", "description": "一句话说明", "prompt": "完整新提示词"}

要求：
- prompt 必须包含 {input} 占位符，评测时样本输入将原样替换进该位置
- 保持任务目标不变，只改进达成方式
- 用中文撰写提示词`

func specSection(task core.Task) string {
	return fmt.Sprintf(`## 任务规格
- 名称：%s
- 描述：%s
- 指标：%s（主指标：%s）`, task.Name, task.Description, strings.Join(task.Metrics, ", "), task.Primary())
}

func lessonsSection(lessons []string) string {
	if len(lessons) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## 祖先教训\n")
	for _, lesson := range lessons {
		fmt.Fprintf(&b, "- %s\n", TruncateRunes(lesson, maxHypoTextRunes))
	}
	return b.String()
}

func buildRewritePrompt(task core.Task, parent core.Candidate, sel Hypothesis, lessons []string) string {
	return fmt.Sprintf(`你是提示词工程专家。请基于下面的优化假设%s。

%s

## 父提示词
%s

## 选中的优化假设（%s，置信度 %.2f）
%s
%s

%s`,
		MarkerRewrite, specSection(task), TruncateRunes(parent.Prompt, maxParentPromptRunes),
		sel.ID, sel.Confidence, sel.Text, lessonsSection(lessons), mutationOutputSpec)
}

func buildMergePrompt(task core.Task, parent, complement core.Candidate, sel Hypothesis, lessons []string) string {
	return fmt.Sprintf(`你是提示词工程专家。请%s：保留两者长处，取长补短。

%s

## 父提示词 A（当前）
%s

## 父提示词 B（互补：在 A 失利的样本上更强）
%s

## 合并指导（选中假设，%s）
%s
%s

%s`,
		MarkerMerge, specSection(task),
		TruncateRunes(parent.Prompt, maxParentPromptRunes),
		TruncateRunes(complement.Prompt, maxParentPromptRunes),
		sel.ID, sel.Text, lessonsSection(lessons), mutationOutputSpec)
}

func buildFreshPrompt(task core.Task) string {
	return fmt.Sprintf(`你是提示词工程专家。请忽略任何既有提示词，仅凭任务描述%s——这是一次随机重启。

%s
- 原始模板参考：%s

%s`,
		MarkerFresh, specSection(task),
		TruncateRunes(task.PromptTemplate, maxParentPromptRunes), mutationOutputSpec)
}
