# PromptOpt 领域术语表（CONTEXT）

> 本文件是领域词汇表（glossary），只记录概念定义，不记录实现细节。由 `/grill` 维护，术语随决议落地时更新。

## 核心概念

- **候选（Candidate）**：一条待评估的提示词版本，携带谱系（父候选、生成策略、变异依据）与评估记录。
- **Pareto 前沿（Frontier）**：候选池中互不支配的候选集合——每个候选至少在一个样本/指标上优于所有其他候选。跨优化范式共享的候选池原语。
- **反射式突变（Reflective Mutation）**：读 minibatch 全量执行 trace，由 LLM 反思诊断后沿"假设 + 祖先经验"改写候选的变异方式（GEPA 核心机制）。
- **ASI（Actionable Side Information，可行动侧信息）**：评估器随分数返回的文本诊断——"文本版梯度"，进入反思上下文驱动突变。
- **文本梯度（Textual Gradient）**：LLM 对失败批次证据反思出的自然语言批评与改法，充当"梯度"指导提示词改写（ProTeGi 机制）。与 ASI 互补：ASI 是评估侧随分数返回的诊断，文本梯度是优化侧沿失败证据产出的定向改写指令。
- **最小辨识集（Minimal Discriminative Set）**：经方差过滤筛出的高区分度样本子集（p¹ 范式）：Prompt 间方差高、单 Prompt 随机性低，极少量即可驱动收敛。
- **假设解耦（Hypothesis Decoupling）**：把"诊断假设的生成"与"沿假设重写提示词"拆成两步并并行验证，配合随机重启与 ε-greedy 防止反思优化器陷入坏种子局部循环（VISTA 机制）。

## 流程角色

- **Harness Builder**：从用户提示词合成任务规格 + 评测集 + 指标的组件；产出经 p¹ 方差过滤提纯。
- **锚点验证集（Anchor Validation Set）**：用户可选提供的 3~5 条真实样本，仅用于最终 verify，永不进入优化循环——对合成分布自证循环的唯一非合成防线（ADR 0001）。
- **合成保留集（Holdout Set）**：verify 在用户未提供锚点验证集时的降级对照集——按原 run spec 独立重合成，保持任务契约与输出 schema 一致但不做 p¹ 探针；结论必须标注"未经真实数据验证"（ADR 0001 声明的降级路径）。
- **回归门禁（Regression Gate）**：verify 对交付候选 vs baseline 的守门判定——主指标均值退化阈值（默认 0.05）+ 约束硬检查（json_validator 任务 JSON 合法率 100%、成本、延迟）；违反以退出码 3 表达，供 PR CI 当作检查失败信号。
- **预算阀门（Budget Valve）**：控制优化终止的全局约束。token 分角色计量（executor / 优化侧），任一或合计超限即触发；评估次数仅计 executor 调用；触发后软停（当前 minibatch 跑完再优雅退出）。
- **检查点（Checkpoint）**：流程中的可干预暂停点（合成集确认、候选采纳），默认自动放行（全托管），可切换为人工确认。
- **全托管（Autopilot）**：检查点全部自动放行、预算耗尽自动终止并交付结果的运行模式。

## 平台扩展

- **主指标（Primary Metric）**：候选排序的首选指标，由 Harness Builder 在任务规格中声明、用户可改；输出契约 = 约束硬过滤 → 主指标排序 → Top-1，前沿全量附送。
- **范式路由器（Paradigm Router）**：按任务特征选择优化范式的组件（V5 落地）：五级有序规则——流水线→textgrad、预算紧张→p1、指令+Few-shot 复合→miprov2、多维约束→gepa、默认→protegi；附"机制等价"降级链（textgrad→protegi、p1→gepa）与未注册范式回退 gepa。决策（范式/请求值/原因/是否降级）全量落 run manifest。
- **Optimizer 接口**：多范式可插拔优化器的统一抽象（落位 internal/engine，ADR 0002）；范式实现经 internal/optimizers 注册表接入——已注册 gepa / miprov2 / evoprompt，protegi 实现已合入、注册行待补。共享脚手架（Loop：baseline 播种/评估单元/前沿准入/产物落盘）保证任何范式的 Best 永不为空。
