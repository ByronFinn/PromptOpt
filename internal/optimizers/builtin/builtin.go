// Package builtin registers every in-repo optimizer paradigm into one
// registry: it is the single paradigm manifest and the only place a
// new paradigm joins the --optimizer surface (docs/plugins.md). The
// optimizers package root stays free of paradigm imports, so the
// dependency edge builtin → paradigms → engine cannot cycle; the web
// and cmd layers resolve paradigms exclusively through this registry.
package builtin

import (
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/optimizers"
	"github.com/ByronFinn/PromptOpt/internal/optimizers/evoprompt"
	"github.com/ByronFinn/PromptOpt/internal/optimizers/miprov2"
	"github.com/ByronFinn/PromptOpt/internal/optimizers/protegi"
)

// Registry returns the in-repo paradigm registry. GEPA ships in the
// engine package; the V5 paradigm subpackages land under
// internal/optimizers and append their registration lines inside this
// function — one Register call each, no other core change
// (docs/plugins.md).
func Registry() *optimizers.Registry {
	reg := optimizers.NewRegistry()
	reg.Register(optimizers.Descriptor{
		Capabilities: optimizers.Capabilities{
			Name:    "gepa",
			Label:   "GEPA 反射进化",
			Summary: "minibatch 反思生成假设、VISTA ε-greedy 选择、三级突变算子与逐样本帕累托前沿",
			Paper:   "GEPA: Reflective Prompt Evolution Can Outperform Reinforcement Learning and Strict Self-Improvement (Agarwal et al., 2024)",
			// Serves the p1 intent (the retained set it optimizes over
			// is already p¹-filtered) and multi-metric Pareto trade-offs.
			SuitsDirectional:     true,
			SuitsMultiConstraint: true,
			SuitsTightBudget:     true,
		},
		Factory: func() engine.Optimizer { return &engine.Gepa{} },
	})
	reg.Register(optimizers.Descriptor{
		Capabilities: optimizers.Capabilities{
			Name:  "protegi",
			Label: "ProTeGi 文本梯度",
			Summary: "UCB1 折臂 bandit 选 minibatch、失败批评为自然语言文本梯度、沿梯度改写候选后全保留集评估，共享 Loop 帕累托准入（beam width 1 简化）",
			Paper:  "Automatic Prompt Optimization with Gradient Descent and Beam Search (Yang et al., EMNLP 2023)",
			// Directional text-gradient feedback loop; the auto router's
			// default rule and the textgrad degrade chain both land here.
			SuitsDirectional: true,
		},
		Factory: func() engine.Optimizer { return protegi.New() },
	})
	reg.Register(optimizers.Descriptor{
		Capabilities: optimizers.Capabilities{
			Name:    "miprov2",
			Label:   "MIPROv2 指令示例联合搜索",
			Summary: "一次提议多条指令变体，与 train 样本抽样的 few-shot 示例子集联合搜索，去泄漏 minibatch 打分后胜者全量准入",
			Paper:   "Optimizing Instructions and Demonstrations for Multi-Stage Language Model Programs (Salesky et al., 2024)",
			// Joint instruction + few-shot demo search over verifiable
			// metrics.
			SuitsJointFewShot: true,
		},
		Factory: func() engine.Optimizer { return miprov2.New() },
	})
	reg.Register(optimizers.Descriptor{
		Capabilities: optimizers.Capabilities{
			Name:    "evoprompt",
			Label:   "EvoPrompt 进化算法",
			Summary: "LLM 充当进化算子：种群初始化、按适应度锦标赛选亲本、LLM 交叉与变异（--evo-variant ga|de 切换差分变异）、精英保留",
			Paper:   "Connecting Large Language Models with Evolutionary Algorithms for Prompt Optimization (Guo et al., 2023)",
			// General population-based instruction search: no Suits*
			// claim feeds the auto router (Route never targets it); it
			// is reachable through an explicit --optimizer evoprompt.
		},
		Factory: func() engine.Optimizer { return evoprompt.New() },
	})
	return reg
}
