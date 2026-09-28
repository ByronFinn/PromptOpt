# 0002 - GEPA+p¹ 为 v1 算法核心，多范式可插拔接口

Date: 2026-09-28

## Status

Accepted

## Context

PromptOpt v2 定位为多范式提示词优化平台（ProTeGi / MIPROv2 风格 / TextGrad 风格 / EvoPrompt 风格 / GEPA / p¹ 五流派能力），但 v1 必须选择一个能先跑通"合成评测→优化→交付"闭环的核心算法。候选各有所长：ProTeGi 最简单但无 Pareto/预算概念；MIPROv2 联合搜索强但依赖预设候选集；五流派全量实现则每个都浅、闭环验证晚。

## Decision

v1 仅实现 **GEPA 反射进化闭环**（反思突变 + Pareto 前沿 + ASI trace 捕获 + VISTA 稳定性防护：假设解耦/并行验证/随机重启/ε-greedy）与 **p¹ 方差过滤**（作为 Harness Builder 的评测集提纯层）。同时固化 `Optimizer` 多范式可插拔接口，其余范式（ProTeGi → MIPROv2 风格 → EvoPrompt 风格）在 V5 分期接入。

## Consequences

### Positive

* 论文证据最强的机制组合：GEPA 有效性（ICLR 2026 Oral，胜 RL 35 倍样本效率）+ p¹ 样本效率（2 条高区分度样本即泛化）+ VISTA 稳定性（修复 GEPA 坏种子退化）
* 单引擎闭环可验证，V5 之前不被多范式实现负担拖慢
* Pareto 前沿作为跨范式共享候选池原语，接口先行

### Negative

* 其余范式延后，平台上线初期只有一种优化能力
* GEPA 参考实现为 Python，Go 重实现的算法正确性需靠行为测试自行保证（见 PRD V3 验收标准）

## Alternatives Considered

* **五流派全量 v1**：平台竞争力最强但每个都浅、工期 3+ 月、闭环验证晚
* **ProTeGi 最简闭环起步**：最快出活但无 Pareto/预算/路由地基，与平台化目标不符

## References

* GEPA: Reflective Prompt Evolution Can Outperform Reinforcement Learning (arXiv:2507.19457)
* p1: Better Prompt Optimization with Fewer Prompts (arXiv:2604.08801)
* Reflection in the Dark (VISTA) (arXiv:2603.18388)
* PRD-0000（docs/prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md）
