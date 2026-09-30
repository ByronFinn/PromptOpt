package evoprompt

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// Markers embedded in the evolutionary meta-prompts — the seam fake
// LLM servers key on to tell the operators apart (evaluation prompts
// never contain them). They are distinct from the engine's reflection
// markers so one scripted server can serve every paradigm stage.
const (
	MarkerInitPop   = "生成初始种群变体"
	MarkerCrossover = "交叉两个提示词"
	MarkerMutation  = "变异提示词"
	MarkerDE        = "做差分进化变异"
)

// Operator names recorded in lineage and frontier members.
const (
	OpInit      = "init"      // population seeding (baseline → variant)
	OpCrossover = "crossover" // two parents → child (GA)
	OpMutation  = "mutation"  // one parent → child (GA, and the degenerate fallback)
	OpDE        = "de"        // parent + (a−b) differential (DE variant)
)

// maxPromptRunes caps any parent prompt embedded in a meta-prompt.
const maxPromptRunes = 3000

// tournamentSize is the tournament draw count (k=2, the EvoPrompt
// paper's setting): two distinct members are drawn and the fitter one
// becomes a parent.
const tournamentSize = 2

// --- population bookkeeping -------------------------------------------------

// tournament draws tournamentSize distinct population members
// uniformly (excluding index exclude, -1 for none) and returns the
// fittest drawn index; equal fitness breaks toward the lower index, a
// deterministic total order. A single-candidate pool wins by default.
// Draws come from the run's rng, so identical seeds replay identical
// parent choices.
func (r *evoRun) tournament(exclude int) int {
	pool := make([]int, 0, len(r.pop))
	for i := range r.pop {
		if i != exclude {
			pool = append(pool, i)
		}
	}
	if len(pool) == 1 {
		return pool[0]
	}
	drawn := make([]int, 0, min(tournamentSize, len(pool)))
	for len(drawn) < min(tournamentSize, len(pool)) {
		idx := pool[r.rng.IntN(len(pool))]
		if !slices.Contains(drawn, idx) {
			drawn = append(drawn, idx)
		}
	}
	best := drawn[0]
	for _, idx := range drawn[1:] {
		switch {
		case r.pop[idx].fitness > r.pop[best].fitness:
			best = idx
		case r.pop[idx].fitness == r.pop[best].fitness && idx < best:
			best = idx
		}
	}
	return best
}

// truncate keeps the top size individuals by fitness (desc) with the
// candidate id (asc) breaking ties — the elitism step: the population's
// current elites always survive a generation boundary.
func truncate(pop []individual, size int) []individual {
	out := slices.Clone(pop)
	slices.SortFunc(out, func(a, b individual) int {
		if a.fitness != b.fitness {
			return cmp.Compare(b.fitness, a.fitness)
		}
		return strings.Compare(a.cand.ID, b.cand.ID)
	})
	return out[:min(size, len(out))]
}

// primaryMean projects one candidate's records onto the fixed sample
// order — missing cells count as 0, matching the Loop's row projection
// (recordsRow) — and averages. It is the population's fitness measure:
// the primary-metric mean over the retained (dev-filtered) set.
func primaryMean(records []engine.SampleRecord, samples []core.Sample, primary string) float64 {
	byID := make(map[string]float64, len(records))
	for _, rec := range records {
		byID[rec.Sample.ID] = rec.Scores[primary]
	}
	sum := 0.0
	for _, s := range samples {
		sum += byID[s.ID]
	}
	return sum / float64(len(samples))
}

// --- evolutionary meta-prompts ----------------------------------------------

// outputSpec is the candidate JSON contract shared by every
// evolutionary meta-prompt (the engine mutation spec, mirrored so the
// shared ProduceCandidate quality gate and repair path apply verbatim).
const outputSpec = `严格输出 JSON，除 JSON 外不要输出任何其他内容，不要使用代码围栏：
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

func truncPrompt(c core.Candidate) string {
	return engine.TruncateRunes(c.Prompt, maxPromptRunes)
}

// buildInitPopPrompt asks for one diverse seed variant: the population
// initialization operator. idx is the variant's 1-based position, so
// the model is nudged away from the variants already seeded.
func buildInitPopPrompt(task core.Task, seed core.Candidate, idx, total int) string {
	return fmt.Sprintf(`你是提示词进化引擎。请围绕种子提示词%s——这是初始种群的第 %d/%d 个变体，请与其他变体保持明显的风格差异。

%s

## 种子提示词
%s

%s`,
		MarkerInitPop, idx, total, specSection(task), truncPrompt(seed), outputSpec)
}

// buildCrossoverPrompt breeds one child from two parents: the LLM
// crossover operator (E1 + E2 → child).
func buildCrossoverPrompt(task core.Task, a, b core.Candidate) string {
	return fmt.Sprintf(`你是提示词进化引擎。请%s：交叉融合两个亲代，继承两者的长处产出一个子代提示词。

%s

## 亲代 A（适应度较高者）
%s

## 亲代 B
%s

%s`,
		MarkerCrossover, specSection(task), truncPrompt(a), truncPrompt(b), outputSpec)
}

// buildMutationPrompt mutates one parent: the LLM mutation operator
// (E → E'). It also serves as the degenerate single-parent operator
// when the population holds only the baseline.
func buildMutationPrompt(task core.Task, parent core.Candidate) string {
	return fmt.Sprintf(`你是提示词进化引擎。请%s：在保持任务语义的前提下，对亲代做一处有方向性的改进。

%s

## 亲代
%s

%s`,
		MarkerMutation, specSection(task), truncPrompt(parent), outputSpec)
}

// buildDEPrompt applies one differential step: the child moves from
// parent along the a−b direction — "produce a prompt that improves
// parent following the direction in which a outperforms b" (the DE
// mutant scaled onto the parent).
func buildDEPrompt(task core.Task, parent, a, b core.Candidate) string {
	return fmt.Sprintf(`你是提示词进化引擎。请%s：差分方向是“参考 A 相对参考 B 的优势”（A−B）。请以亲代为起点，把该方向上的改进施加到亲代，得到一个子代提示词。

%s

## 亲代（进化起点）
%s

## 参考 A（差分方向的正端）
%s

## 参考 B（差分基点）
%s

%s`,
		MarkerDE, specSection(task), truncPrompt(parent), truncPrompt(a), truncPrompt(b), outputSpec)
}
