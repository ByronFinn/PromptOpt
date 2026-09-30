package miprov2

import (
	"cmp"
	"errors"
	"fmt"
	"strings"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// Markers embedded in the proposal meta-prompt — the seam fake LLM
// servers key on to tell the proposal (and its repair) apart from
// evaluation traffic (engine marker precedent, gepa_test.go).
const (
	MarkerPropose    = "提议多条候选指令"
	MarkerProposeFix = "修复为合法的指令列表 JSON"
)

// Proposal prompt bounds.
const (
	maxInstructionRunes  = 3000 // mirrors engine.maxParentPromptRunes
	summarySamples       = 6    // sample summaries shown to the proposer
	maxSummaryFieldRunes = 600
)

// instructionsPayload is the proposal wire format: one JSON object
// carrying the instruction variants.
type instructionsPayload struct {
	Instructions []string `json:"instructions"`
}

// checkInstructions backs the Defend gate: at least one usable variant
// must survive normalization.
func checkInstructions(p instructionsPayload) error {
	if len(normalizeInstructions(p.Instructions, numInstructions)) == 0 {
		return errors.New("响应中没有可用的指令变体")
	}
	return nil
}

// normalizeInstructions clips the pool to n and keeps only usable
// variants: non-empty after trimming, carrying {input} (rendering is
// plain replacement — a variant without the placeholder can never see
// the sample input), truncated to maxInstructionRunes and deduped in
// first-seen order.
func normalizeInstructions(pool []string, n int) []string {
	out := make([]string, 0, min(len(pool), n))
	seen := make(map[string]bool, len(pool))
	for _, ins := range pool {
		if len(out) >= n {
			break
		}
		text := engine.TruncateRunes(strings.TrimSpace(ins), maxInstructionRunes)
		if text == "" || !strings.Contains(text, core.InputPlaceholder) || seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, text)
	}
	return out
}

// buildProposePrompt assembles the one-shot instruction proposal: task
// spec, the baseline prompt, train-first sample summaries and the JSON
// output spec.
func buildProposePrompt(task core.Task, baseline string, pool []core.Sample, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `你是提示词工程专家。请基于任务与样本，%s（共 %d 条互不相同的完整指令变体）。

## 任务规格
- 名称：%s
- 描述：%s
- 指标：%s（主指标：%s）
- 原始模板：%s

## 基线提示词
%s

## 样本摘要（共 %d 条，train 优先）
`, MarkerPropose, n, task.Name, task.Description, strings.Join(task.Metrics, ", "), task.Primary(),
		engine.TruncateRunes(task.PromptTemplate, maxInstructionRunes),
		engine.TruncateRunes(baseline, maxInstructionRunes), len(pool))
	for _, s := range pool {
		fmt.Fprintf(&b, `
### %s（split=%s）
- 输入：%s
- 期望：%s
`, s.ID, cmp.Or(s.Split, "未标注"),
			engine.TruncateRunes(s.Input, maxSummaryFieldRunes),
			engine.TruncateRunes(expectedText(s.Expected), maxSummaryFieldRunes))
	}
	fmt.Fprintf(&b, `
严格输出 JSON，除 JSON 外不要输出任何其他内容，不要使用代码围栏：
{"instructions": ["完整指令变体一", "完整指令变体二", "完整指令变体三", "完整指令变体四"]}

要求：
- 每条指令都是完整可独立使用的提示词，必须包含 {input} 占位符，评测时样本输入将原样替换进该位置
- 指令之间侧重不同的改进策略
- 用中文撰写
`)
	return b.String()
}
