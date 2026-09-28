# PromptOpt 领域术语表（CONTEXT）

> 本文件是领域词汇表（glossary），只记录概念定义，不记录实现细节。由 `/grill` 维护，术语随决议落地时更新。

## 核心概念

- **候选（Candidate）**：一条待评估的提示词版本，携带谱系（父候选、生成策略、变异依据）与评估记录。
- **Pareto 前沿（Frontier）**：候选池中互不支配的候选集合——每个候选至少在一个样本/指标上优于所有其他候选。跨优化范式共享的候选池原语。
- **反射式突变（Reflective Mutation）**：读 minibatch 全量执行 trace，由 LLM 反思诊断后沿"假设 + 祖先经验"改写候选的变异方式（GEPA 核心机制）。
- **ASI（Actionable Side Information，可行动侧信息）**：评估器随分数返回的文本诊断——"文本版梯度"，进入反思上下文驱动突变。
- **最小辨识集（Minimal Discriminative Set）**：经方差过滤筛出的高区分度样本子集（p¹ 范式）：Prompt 间方差高、单 Prompt 随机性低，极少量即可驱动收敛。
- **假设解耦（Hypothesis Decoupling）**：把"诊断假设的生成"与"沿假设重写提示词"拆成两步并并行验证，配合随机重启与 ε-greedy 防止反思优化器陷入坏种子局部循环（VISTA 机制）。

## 流程角色

- **Harness Builder**：从用户提示词合成任务规格 + 评测集 + 指标的组件；产出经 p¹ 方差过滤提纯。
- **锚点验证集（Anchor Validation Set）**：用户可选提供的 3~5 条真实样本，仅用于最终 verify，永不进入优化循环——对合成分布自证循环的唯一非合成防线（ADR 0001）。
- **预算阀门（Budget Valve）**：控制优化终止的全局约束。token 分角色计量（executor / 优化侧），任一或合计超限即触发；评估次数仅计 executor 调用；触发后软停（当前 minibatch 跑完再优雅退出）。
- **检查点（Checkpoint）**：流程中的可干预暂停点（合成集确认、候选采纳），默认自动放行（全托管），可切换为人工确认。
- **全托管（Autopilot）**：检查点全部自动放行、预算耗尽自动终止并交付结果的运行模式。

## 平台扩展

- **主指标（Primary Metric）**：候选排序的首选指标，由 Harness Builder 在任务规格中声明、用户可改；输出契约 = 约束硬过滤 → 主指标排序 → Top-1，前沿全量附送。
- **范式路由器（Paradigm Router）**：按任务特征（单模块纠偏 / 指令+Few-shot 复合 / 多维约束 / 预算紧张）选择优化范式的远期组件。
- **Optimizer 接口**：多范式可插拔优化器的统一抽象；v1 仅 GEPA 实现，ProTeGi / MIPROv2 风格 / EvoPrompt 风格分期接入。
