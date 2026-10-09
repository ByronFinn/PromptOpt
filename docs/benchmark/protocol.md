# 基准协议：GSM8K 单任务等预算对照（骨架）

> **Status**: 协议骨架（V7 提案 §1.3 的落地文档；完整基准发布属提案 P1#7，含 README 数字与全协议产物，不在本期交付）｜ **Created**: 2026-10-08 ｜ **依据**: [roadmap-v7-proposal.md](../roadmap-v7-proposal.md) §1.3、[docs/research/0001-llm-judge-independent-config.md](../research/0001-llm-judge-independent-config.md)（裁判不参与本基准——判分用确定性 exact_match）

## 0. 本期边界（先读）

本期交付的是**可运行脚手架**：`bench/` 目录（Python，仅服务基准对照，不进
`go build`、非运行时依赖）使「统计显著性层与指标注册表真的能用」这场考试
可执行。README 效果数字、多 seed 正式开跑与产物发布是 P1#7 的事；本文先把
口径钉住，避免正式开跑时临时造标准。

## 1. 任务与判分

- **任务**: GSM8K 固定子集，dev 200 / test 500，钉 seed 抽样（`bench/gsm8k_subset.py`，默认 seed 20260930，每 split 独立建流、扩缩规模不改变已抽构成）。
- **判分**: 数值归一化 exact_match（剥千分位逗号取最后一个数值）——单模块可解、判分确定性，**不引入 llm_judge 的跨实现不可比**（裁判隔离让 llm_judge 可比，但基准先走确定性指标）。
- **未核实标注（提案 §6-B）**: GEPA / MIPROv2 / p¹ 论文各自使用的 GSM8K 子集与划分未在本仓核实；正式发布前须从论文与官方 repo 逐一核对，决定对齐或声明分歧。子集规模（200/500）是预算估计（提案 §6-A4），实施时按费用调整。

## 2. 对照系统

| 系统 | 钉版 | 备注 |
|---|---|---|
| DSPy MIPROv2 | `dspy==3.4.0`（PyPI JSON API，2026-10-08 核取；正式开跑前复核） | `bench/run_dspy.py`，`dspy.Predict` 单模块最小对等 |
| gepa 原版 Python | 钉 commit（实施时钉，本期未定） | P1#7 接入 |
| PromptOpt | 钉 commit（实施时钉） | 手工三件套 `run` + `verify` |

五同：同 executor 模型、同温度、同 max_tokens、同数据划分、同一判分脚本。
PromptOpt 侧温度经 `--temperature` 显式钉（统计层 A 层交付）；脚手架侧
`dspy.LM(temperature=0)` 同钉。

**取舍（提案 §1.3）**: 单模块 vs DSPy 多 stage program 的公平性质疑——用
`dspy.Predict` 单模块做最小对等，结论边界声明为「单模块任务的等预算对比」。

## 3. 等预算定义（诚实口径的核心）

**executor 侧双上限同时钉**，先到先停、任一越界 run 作废：

- **N** = 评估调用次数（`--max-metric-calls`；对齐 GEPA `max_metric_calls` 语义与本项目 `--budget-evals`）——每次「候选执行并被判分」计 1；
- **T** = executor token 预算（对齐本项目 `--budget-tokens`）。

**优化侧开销（反思/提议/合成的调用与 token）不强行对齐**——各实现机制异构，
强行对齐是伪精度——但**必须全额计量并披露在结果表**，与评估侧分列：

- PromptOpt 侧：`core.Role` 分角色计量（executor / optimizer / judge 分列，
  verify/run 工件的 `usage_by_role` 原样引用）——分角色计量是本协议的卖点；
- dspy 侧：`run_dspy.py` 按相计量（`baseline_eval` / `optimize` /
  `final_eval` 三相的调用数与 prompt/completion token）。

**计量粒度差异（如实披露）**: dspy 侧 token 预算的复核点在相边界（粗粒度
软停，`budget_actual.within_t` 达标标记）；PromptOpt 侧是逐调用硬账。两侧
口径差异随结果表一并披露，不冒充同粒度。

## 4. 统计口径（自食其力）

- 每系统 ≥3 seed；报均值 ± sd。
- 配对自助法 CI：与本仓 verify 门禁**同一数学**（B=1000、2.5%/97.5% 百分位
  线性插值、`regressed` / `confident_pass` / `inconclusive` 三态、回归阈
  0.05）——实现见 `bench/bootstrap_ci.py`，对照 `cmd/promptopt/verify_stats.go`。
  重采样 RNG 不同（Go PCG 固定种子 vs Python random 固定种子），CI 数值
  允许末位差异，判定标准一致。
- **自食其力**: 我们要求用户「inconclusive 必须标注、regressed 必须过门禁」
  的标准，自己 README 的基准数字先达到；`inconclusive` 的对比不得写成
  「更优」。
- 配对前提：两侧在**同一 test 子集**上逐样本评估（`per_sample` 按 id 配对）。

## 5. 产物与流程

```text
1. bench/gsm8k_subset.py     → 固定子集（dev/test jsonl，随 runs/ 快照归档）
2. 各系统 × ≥3 seed 等预算 run → 原始产物归档（PromptOpt 侧 runs/ 快照原样；
                                dspy 侧 run_dspy.py 结果 JSON）
3. bench/bootstrap_ci.py     → 两两配对 CI + 三态判定
4. README 表格 + 方法论脚注（引用本协议）+ 原始产物链接
   （数字可能不好看也要诚实发布——提案 §1.3 对证据支柱的要求）
```

## 6. 与本仓机制的关系

- verify 的配对自助法 CI（退出码 0/3 语义不变）是本协议的统计工具来源（§1.1 依赖方向：基准协议钉版 reps/温度，统计机制互为依赖）。
- 裁判隔离（`--judge-*`）不直接参与本基准（exact_match 无裁判），是 llm_judge 任务后续对比的前提。
- 指标注册表（`internal/eval` 注册表 + `tcm_f1_*` 试点，docs/plugins.md §8）与基准无直接耦合；两者共同验证「扩展面真的能用」。
