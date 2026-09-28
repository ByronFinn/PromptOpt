# PromptOpt v2：多范式提示词优化平台（GEPA 式闭环 + Go 重构）

> **Status**: Draft | **PRD**: PRD-0000 | **Created**: 2026-09-28 | **Last updated**: 2026-09-28

## Goal

将 PromptOpt 从"工程师手写 task.yaml/dataset.yaml 的评估框架"升级为"更大更全面的提示词优化平台"：用户只输入一个提示词，AI 自动构建评测流程，在 token/测试次数预算内运行 GEPA（反射突变 + Pareto 前沿）优化闭环并输出最优提示词；人可在 Web 仪表盘查看/干预也可全托管。架构上预留多范式可插拔优化器接口（ProTeGi / MIPROv2 风格 / TextGrad 风格 / EvoPrompt 风格分期实现）。同时参考 JetBrains go-modern-guidelines 用 Go 同仓库原地重写，清理 45 个旧 Python 路线 issue 与旧 roadmap。

## What I already know

* **用户愿景**：输入提示词 → AI 构建流程 → 输出最优提示词；交互极简；评测在内部执行；人可查看/干预也可全托管；通过 token 用量或测试次数限制预算。（注："sub-agent driven"指规划/开发过程中优先使用子代理并行作业的工作方式，不是平台架构需求——平台内部为常规组件 + LLM 调用点，不强加 agent 运行时抽象。）
* **技术栈转向**：参考 JetBrains/go-modern-guidelines 用 Go 重构（按 go.mod 版本使用现代习语：`max`/`slices.Contains`/`cmp.Or`/`for i := range n`/Go 1.26 `new(v)`/`errors.AsType[T]`，配套 `modernize` analyzer 做 CI 检查）。
* **GEPA 仓库**（github.com/gepa-ai/gepa，6.8k stars）：核心循环 = 从 Pareto 前沿选候选 → minibatch 执行捕获全量 trace → LLM 反思诊断（ASI，Actionable Side Information）→ 突变生成改进候选 → 达标则入前沿；支持前沿候选互补合并；预算由 `max_metric_calls` 控制（100–500 次评估即可）；Python API `gepa.optimize(...)`，无 CLI；adapter 层（`GEPAAdapter.evaluate` + `make_reflective_dataset`）。
* **GEPA 论文**（arXiv:2507.19457，ICLR 2026 Oral ✅）：自然语言作为学习介质优于标量奖励的 policy gradient；胜 GRPO ~6%（最高 20%）且 rollout 少 35 倍；胜 MIPROv2 10%+（AIME-2025 +12%）；几十次 rollout 即可显著提升。
* **现有 Python 代码**（src/promptopt，29 文件）：core 模型、3 个 evaluator、litellm adapter、optimizer 桩、storage 桩、diagnostics 桩、CLI 桩。仅数据集加载器、task.yaml/candidate.yaml 解析已合并（PR #45/#47）。
* **远程 issue**：45 个 open（#2–#46，M1–M7 旧 Python 路线），已关闭 #1；3 个 PR 已合并。
* **旧 roadmap**（ROADMAP.md）：M1 进行中，M2–M7 待开始；README 明确定位"结构化任务回归测试框架"。
* **核心矛盾（已解决）**：GEPA 需要数据集+指标，新愿景用户只输入提示词 → 评测集由 AI 合成 + p¹ 方差过滤提纯 + 人工检查点可干预。
* **用户提供的多范式理论矩阵**（两批输入合并，引用已逐一查证）：
  * **流派一：贝叶斯/代理模型** — MIPRO/MIPROv2（DSPy）[arXiv:2406.11695 ✅, arXiv:2407.21787 ✅]：TPE 联合搜索 Instruction+Few-shot；瓶颈在预设候选集受限。BAPO ❌ 未核实（arXiv:2305.12524 实为 TheoremQA；该标题论文不存在）；BO 流已核实替代：TextBO [arXiv:2511.12063 ✅]、Embedding by Elicitation [arXiv:2605.19093 ✅]、Sabbatella [arXiv:2312.00471 ✅]。
  * **流派二：进化算法** — EvoPrompt [arXiv:2309.08532, ICLR 2024 ✅]：LLM 充当 GA/DE 算子；瓶颈是语义交叉失真。Promptbreeder [arXiv:2309.16797 ✅]：任务 Prompt 与变异算子 Prompt 自指双层演化。
  * **流派三：伪梯度与反思** — APE [arXiv:2211.01910, ICLR 2023 ✅] / ProTeGi [arXiv:2305.03495, EMNLP 2023 ✅]：文本伪梯度 + Beam Search + Bandit 预算分配；失效模式"修批次 A 塌批次 B"。TextGrad [arXiv:2406.07496 ✅]：计算图自然语言反向传播；瓶颈是梯度稀释。
  * **流派四：RL/策略搜索** — RLPrompt [arXiv:2205.12548, EMNLP 2022 ✅]：离散 Q-Learning。TEMPER（MCTS）❌ 无可核实出处，不采纳。
  * **流派五：2025–2026 前沿** — GEPA [arXiv:2507.19457 ✅]：复合信号 + Pareto 前沿互补解保留；瓶颈是目标维度 >3 时收敛慢。p¹ [arXiv:2604.08801 ✅]：奖励方差解构（响应方差 vs 系统 Prompt 方差），最小辨识集（2 条 AIME24 prompt 即泛化），报告优于 GEPA。VISTA [arXiv:2603.18388 ✅, ACL SRW 2026]：反思优化器坏种子退化实证（GEPA 使 GSM8K 23.81%→13.50%）；假设生成/重写解耦 + 并行 minibatch 验证 + 重启 + ε-greedy 恢复至 87.57%。
  * **选型决策准则**（用户总结，产品化为远期范式路由器策略）：单模块定向纠偏→ProTeGi；多组件流水线→TextGrad；指令+Few-shot 复合→MIPROv2；生产多维约束→GEPA；预算极紧/测试集大→p¹。
* **架构启示**：p¹ 是评测集构建器的数据选择层（合成集提纯）；VISTA 的解耦/重启是反思循环的内建稳定性保障；Pareto 前沿是跨范式共享的候选池原语。

## Assumptions (temporary)

* 目标/优化模型经 OpenAI-compatible API 或 Anthropic API 访问；本地模型（Ollama/vLLM）走 OpenAI-compatible。
* 单机单用户优先，Web UI 为 localhost 内嵌服务，不做多租户。
* GEPA 核心机制按论文与仓库语义用 Go 重实现（参考实现为 Python，需以论文为准自行验证，不逐行移植）。
* 用户在本会话后走 /grill → /story（或直接实现工作流）落地代码。

## Open Questions

（已全部收敛，2026-09-28）

## Requirements

* **一键入口**：`promptopt run "<prompt>"` 单命令从提示词到最优提示词全流程，无需手写任何配置文件。
* **评测数据构建**：Harness Builder 从用户提示词自动合成任务规格 + 评测集 + 指标；默认全托管直跑；支持暂停审核、增删样本、导入自有数据后继续；合成集经 p¹ 式方差过滤提纯（筛掉死样本与高随机性样本，保留高区分度核心样本）。
* **GEPA 优化闭环（v1 核心）**：minibatch 执行 + 全量 trace 捕获（ASI）→ LLM 反思诊断 → 突变生成候选 → Pareto 前沿维护（per-example 分数 + 非支配排序）→ 前沿互补合并；内建 VISTA 稳定性保障（假设生成与重写解耦、语义标注假设并行验证、随机重启、ε-greedy 采样）。
* **多范式可插拔（接口先行，实现分期）**：`Optimizer` 接口统一约束；v1 仅 GEPA 实现；ProTeGi / MIPROv2 风格 / TextGrad 风格 / EvoPrompt 风格为后续里程碑。
* **预算阀门**：token 用量上限与评估次数上限双模式，可组合；耗尽即优雅终止并输出当前前沿最优 + 解释性报告；token 计量来自 provider usage 回传。
* **Web 仪表盘**：Go 单二进制 `go:embed` 前端，localhost 启动；htmx + 模板 + SSE 实时事件流；**V1 即交付看板骨架页**（运行状态 + 事件流，开源门面），V2 补合成集审核页，V4 完整视图（Pareto 前沿、预算消耗、trace 浏览、候选 diff、检查点干预）；CLI `--headless` 无头模式输出 JSON 供自动化。
* **开源工程**：面向开源传播——V0 完成 README 重写（定位/badge/快速开始/架构图）+ CONTRIBUTING.md + LICENSE（MIT）确认；V1 起每个里程碑录制 demo GIF 进 README；V6 用 goreleaser 出多平台单二进制 Release + changelog。
* **干预与全托管**：检查点默认自动放行（全托管），`--interactive` 或仪表盘中切换为人工确认。
* **Provider**：接口抽象；v1 实现 OpenAI-compatible 通用适配（OpenAI/DeepSeek/Qwen/Ollama/vLLM）+ Anthropic 原生；带重试/限流/usage 统计。
* **持久化**：SQLite（run/candidate/lineage/样本级结果）+ artifact 文件（YAML/JSON，可直接人工编辑干预）。
* **工程规范**：遵守 go-modern-guidelines；CI 跑 `go vet` / `staticcheck` / `modernize` / 全量测试；同仓库原地重写，Python 代码删除并打 tag `v0.1-python` 留档。
* **遗留处置**：45 个旧 issue 批量关闭（评论指向本 PRD 与父 Issue），能力在新 roadmap 中重新拆解为新 issue。

## Acceptance Criteria

* [ ] `promptopt run "<一个自然语言提示词>"` 在零配置下完成合成评测→优化→输出最优提示词，全程预算受控
* [ ] 合成评测集经过方差过滤；用户可在检查点审核/增删/替换样本（Web + artifact 文件两条路径）
* [ ] 优化循环产生反思 trace、候选 lineage、Pareto 前沿记录，全部可追溯查询
* [ ] 坏种子场景有防护：重启与 ε-greedy 生效时留有事件记录（VISTA 机制）
* [ ] 预算耗尽时优雅终止：退出码规范、输出当前最优候选与已消耗预算报告
* [ ] Web 仪表盘实时展示前沿/预算/trace，检查点可人工干预；`--headless` 输出 JSON
* [ ] `go vet` / `staticcheck` / `modernize` / `go test ./...` 全绿；构建产物为单二进制
* [ ] Python 源码从工作区移除，`v0.1-python` tag 存在
* [ ] 旧 issue 全部关闭且评论含指向说明；新 roadmap 里程碑拆解为 GitHub issue/里程碑
* [ ] V1 结束时浏览器可打开看板骨架页（实时事件流可见）
* [ ] 仓库具备开源门面：badge / 快速开始 / 贡献指南 / demo GIF 齐备

## Definition of Done

* Tests added/updated (unit/integration where appropriate；LLM 调用全部 mock)
* Lint / typecheck / CI green（go vet / staticcheck / modernize）
* Docs/notes updated（README、ROADMAP、AGENTS.md 同步 Go 工作流）
* Rollout/rollback considered（tag 留档即回滚锚点）

## Out of Scope

* 多用户/云部署/SaaS 化
* 模型微调与 RL 训练类优化（RLPrompt 流派）
* 多轮对话 agent 与 RAG 全链路优化（远期 TextGrad 风格扩展涵盖多组件流水线）
* MCTS 类搜索（无可核实文献支撑）
* 第三方插件市场（V5 仅做接口开放，不做生态运营）
* 与 MLflow/Opik 等 GEPA 生态集成

## Technical Approach

```
promptopt/                      # Go module（同仓库原地重写）
├── cmd/promptopt/              # 入口：run / serve / resume / inspect 子命令
├── internal/
│   ├── core/                   # Task, Candidate, Dataset, RunResult, Trace（structs）
│   ├── provider/               # Provider 接口；openai-compat 通用 + anthropic；重试/限流/usage
│   ├── harness/                # Harness Builder：合成任务规格+数据集+指标；p¹ 方差过滤
│   ├── eval/                   # 评估引擎：goroutine+errgroup 并行、trace 捕获、exact/f1/json/judge
│   ├── engine/                 # GEPA 循环：Reflector、Mutator、Frontier、Budget、VistaGuard
│   ├── optimizers/             # Optimizer 接口（多范式可插拔；v1 仅 GEPA 实现）
│   ├── store/                  # SQLite + artifact 读写
│   └── web/                    # net/http + htmx 模板 + SSE；go:embed 静态资源
├── web/                        # 前端模板与静态资源（嵌入源）
└── examples/                   # 端到端示例（JSON 抽取等，随里程碑更新）
```

* **预算**：`Budget` 随 `context.Context` 传递；token 计量以 provider usage 回传为准，评估次数在 eval 引擎计数。
* **前沿**：per-example 分数矩阵 + 非支配排序；候选池跨范式共享。
* **ASI**：evaluator 返回 `(score, diagnosis)`，diagnosis 文本进入反思上下文。
* **检查点**：事件总线 → Web SSE / CLI 提示；artifact 文件同时落盘供离线编辑。
* **依赖原则**：标准库优先（`net/http`、`database/sql` + modernc.org/sqlite 纯 Go 驱动保持单二进制/无 cgo）；cobra 只在需要时引入。

## Research References

（本次为临时调研，未走 /research 持久化；引用矩阵见 What I already know，均为 arXiv 一手来源）

## Feasible Approaches

**Approach A: GEPA+p¹ 核心，接口多范式，Go 单二进制，Web UI 优先**（选定）

* How it works：如上 Technical Approach。
* Pros：单引擎闭环可验证；论文证据最强的两个机制（GEPA 有效性 + p¹ 样本效率 + VISTA 稳定性）组合；架构为多范式留位。
* Cons：其余范式延后；Python 参考实现需语义级重写。

**Approach B: 五流派全量 v1**

* Pros：平台竞争力最强。
* Cons：每个都浅、闭环验证晚、工期 3+ 月。

**Approach C: ProTeGi 最简闭环起步**

* Pros：最快出活。
* Cons：与平台化目标不符，无 Pareto/预算/路由基础。

## Decision (ADR-lite)

**Context**：平台转向需定算法核心、技术栈、交互形态、数据来源与遗留处置。
**Decision**：v1 = GEPA 反射进化（含 VISTA 稳定性机制）+ p¹ 方差过滤；Go 同仓库原地重写（单二进制、go:embed 前端、htmx+SSE 仪表盘，CLI 无头模式）；评测数据 AI 合成+可干预；45 旧 issue 批量关闭+新 roadmap 重拆；Provider 走 OpenAI-compatible 通用 + Anthropic。**追加决策（2026-09-28）**：开源工具定位强化——Web 看板骨架页前置到 V1 并作为门面打磨（验收含可视化交付），V0 加开源配套（README/badge/CONTRIBUTING），V6 加 goreleaser Release 规范；前端维持 htmx 不升级 SPA。
**Consequences**：放弃 Python 生态与 gepa 参考实现的直接复用，算法正确性以论文语义+自建测试保证；自证循环风险由 p¹ 过滤+人工检查点+held-out 验证缓解；单人维护负担由 V0–V6 分期控制；页面前置使 V1 工期略增，换来早期可传播的开源门面。

## Implementation Plan (small PRs)

* V0 地基：Go module 脚手架、Provider 接口 + 两个实现、config、storage、CI（vet/staticcheck/modernize/test）、删除 Python + tag 留档、开源门面（README/badge/CONTRIBUTING）
* V1 评估闭环：core 模型、evaluators、评估引擎（并行+trace）、run 持久化、`run` 最小闭环（手工数据集可跑）、Web 看板骨架页（htmx+SSE）、首个 demo GIF
* V2 Harness Builder：AI 合成任务规格+数据集+指标、p¹ 方差过滤、检查点机制、合成集审核页
* V3 GEPA 引擎：Reflector/Mutator/Frontier/Budget/VistaGuard、lineage、报告
* V4 Web UI 完整化：前沿看板、预算仪表、trace 浏览、候选 diff、采纳干预、报告导出
* V5 多范式扩展：Optimizer 插件接口固化、ProTeGi → MIPROv2 风格 → EvoPrompt 风格分期、范式路由器
* V6 工程化：无头模式退出码规范、GitHub Actions 集成、回归门禁（verify/select/rollback）、goreleaser Release 规范

## Technical Notes

* GEPA 关键机制备忘：ASI = 文本版梯度；前沿按 per-example 分数维护互补优势；`max_metric_calls` 是全局预算阀门。
* go-modern-guidelines README 只含摘要，细则在 FEATURES.md（V0 实现时通读）；`modernize` analyzer 已入 gopls，可做 CI 检查。
* 失败/边界（发散扫描结论）：①自证循环——AI 合成评测集有偏，靠 p¹ 过滤 + 人工检查点 + 优化结束在保留集上 held-out 验证缓解；②坏种子退化——VISTA 机制内建；③预算耗尽——优雅终止输出当前最优；④API 抖动——provider 层重试 + 评估结果缓存。
* 远期演化（记录不进 v1）：范式路由器（用户选型准则产品化）、多组件流水线优化（TextGrad 风格）、`optimize_anything` 泛化（任意文本参数）。
* 已检查文件：ROADMAP.md、README.md、AGENTS.md、pyproject.toml、src/promptopt/**、examples/json_extraction/**。

## Traceability

- **Created by**: `/think` (2026-09-28)
- **New terms**: GEPA, 反射式突变 (reflective mutation), Pareto 前沿 (Pareto frontier), ASI (Actionable Side Information), 文本梯度 (textual gradient), 方差过滤 (variance filtering) / 最小辨识集, 假设解耦 (hypothesis decoupling), Harness Builder, 预算阀门 (budget valve), 全托管 (autopilot), 检查点干预 (checkpoint intervention)

## Issue

#49（父 Issue：https://github.com/ByronFinn/PromptOpt/issues/49）
