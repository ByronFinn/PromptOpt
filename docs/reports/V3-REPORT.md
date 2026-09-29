# PromptOpt v2 V3 纵切实施报告

> **日期**: 2026-09-29 | **范围**: V3（GEPA 引擎），交付于提交 `aa1ee1f`（提交后工作区干净）
> **对照文档**: [ROADMAP.md](../../ROADMAP.md) V3 里程碑（第 106–117 行）、[PRD-0000](../prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md)、上期 [V2-REPORT.md](V2-REPORT.md)

## 1. 交付概述

本次交付兑现 ROADMAP V3 的全部六项，并闭合上期报告第 6.3 节声明的缺口——零配置流程的终点从 baseline 评估延伸到 **GEPA 反射进化优化**：`promptopt run "<提示词>"` 现在一气呵成跑完 合成 → p¹ 过滤 → 检查点 → baseline 评估 → 优化循环，落盘 lineage / frontier / report 三类新产物。测试函数从上期的 103 个增至 **132 个**（`go test ./... -v` 实测 132 个 `--- PASS`）：engine 新增 27，run_test +1（11→12，零配置优化链路等）、harness_test +1（22→23），与 `aa1ee1f` 提交统计逐一对照核实。

新增代码（`wc -l` 实测）：`internal/engine/` 13 文件 3497 行（实现 9 文件 2145 行 + 测试 4 文件 1352 行）；修改 `cmd/promptopt/run.go`（优化循环接线 + 6 个引擎 flag + 终端事件渲染，+263 行）、`internal/harness/pipeline.go`（管线延伸至优化边界）、`internal/config/config.go`（引擎默认值）、AGENTS.md / ROADMAP.md 等，合计 23 文件 +4072/−38（`git show --stat aa1ee1f`）。

配套文档更新：[ROADMAP.md](../../ROADMAP.md) V3 六项复选框勾选（核实结论见第 2 节）；[README.md](../../README.md) 新增"提示词优化（GEPA 引擎）"一节（优化示例 + `--max-rounds` 等参数表 + 优化产物说明），"当前状态"改为 V0→V3 已落地。

## 2. 交付清单（对照 ROADMAP V3 六项）

ROADMAP V3 六个复选框在 `aa1ee1f` 中已勾选；本次逐项对照代码与测试核实，**均如实兑现，无虚勾**：

| 条目 | 状态 | 证据 |
|---|---|---|
| Reflector：读 minibatch 全量 trace（ASI）生成自然语言诊断与假设 | ✅ | `internal/engine/reflector.go:58-77`（Reflect）：上下文含任务规格、父提示词、逐样本 输入/期望/输出/分数/ASI 诊断（buildReflectPrompt reflector.go:127-177）与祖先教训；输出至多 n 条假设（id/text/sample_ids 标注/confidence）。假设池规范化：裁剪到 n、补 id、剥 `{input}` 字面量、截断、置信度夹取（reflector.go:82-104）；JSON 三级防御（本地抽取 → 单次修复调用 → 原文摘录报错，defend reflector.go:296-327） |
| Mutator：沿假设 + 祖先经验教训突变候选；前沿互补合并 | ✅ | `internal/engine/mutator.go:26-40` 三算子 Rewrite / Merge / Fresh；Rewrite 携带选中假设与教训（mutator.go:108-123），Merge 以前沿互补成员为父 B、假设为合并指导（mutator.go:125-145；互补选取 `Frontier.Complement` frontier.go:98-128，触发于 gepa.go:237-240）；产物质量门（非空 + 含 `{input}`，一次修复机会 mutator.go:52-77）；教训来自 lineage 的 BFS 祖先假设链（`Lineage.Lessons` lineage.go:70-102，上限 6 条 gepa.go:37） |
| Frontier：per-example 分数矩阵 + 非支配排序 | ✅ | `internal/engine/frontier.go:37-51`（Dominates：逐样本主指标行，逐位 ≥ 且至少一位严格 >）；`Add` 非支配准入、被支配淘汰、向量克隆拒绝（frontier.go:65-81）；成员两两非支配的单调性由传递性保证——`TestFrontierScriptedSequence` 每步断言 + `TestFrontierRandomAdds` 500 随机向量回放验证 |
| Budget：token 用量 ∥ 评估次数双阀门，context 传递，耗尽优雅终止（另设 --budget-opt-tokens 优化侧独立阀门） | ✅ | 共享 `eval.Budget`（token/次数双上限，`SoftStopped` 只由 executor 角色触发——eval/budget.go:11,50-66）；优化侧用量记 optimizer 角色、只喂独立阀门不触发软停（calls.go:158，`ValveTripped` calls.go:65-71）；信号：context 取消优先于预算（gepa.go:131-141）；优雅终止：Result 契约"Best 恒非空"（optimizer.go:139-141，前沿以 baseline 行播种 gepa.go:108-113），轮门（gepa.go:137-140）、调用门（calls.go:91-94）、单元中断（gepa.go:187-197, 264-290）三处均收敛到 `budget_stopped` 并交付当前最优 |
| VistaGuard：假设生成与重写解耦、语义标注假设并行验证、随机重启、ε-greedy | ✅ | 解耦：Reflector 产出假设池 → 以"父提示词+补充指导"临时候选探针验证 lift（gepa.go:171-197）→ `VistaGuard.Select` 只从验证池选择（vista.go:47-55），Record 只记停滞、重启由主循环消费 `Stagnant()` 并显式 Reset（vista.go:57-72 注释，触发可观测）；验证并行性：假设间串行（预算顺序确定）、单个探针单元内 worker 并行（gepa.go:168-170 注释）；随机重启：停滞 ≥ `--stagnation-limit` → `Mutator.Fresh` + `vista_restart` 事件（gepa.go:229-236）；ε-greedy：`rng.Float64() < ε` 均匀探索否则 argmax-lift（vista.go:51-54），mode 记入 `hypotheses_validated` 事件（gepa.go:218-222） |
| 候选 lineage + 解释性报告（优化轨迹、预算消耗、最终前沿） | ✅ | `lineage.json`：追加式、原子重写（lineage.go:59-61, 126-154），记录 父代/算子/假设/逐样本分数/准入与否/预算中断标记（LineageRecord lineage.go:21-32）；`frontier.json` + `report.md`：中文报告含 概览（轮次/终止原因/分角色用量/优化侧阀门）、Top-1 提示词全文、前沿成员表（含独占占优样本数 `Wins` frontier.go:146-175）、取舍说明、谱系（WriteOutputs report.go:67-100，renderReport report.go:105-195） |

**验收三条**（ROADMAP 第 117 行）逐条核实：

1. **坏种子场景有防护且留事件记录（vista_restart 事件）**：TestGepaGoldenLoop 断言第 6 轮恰好一次 `vista_restart`（gepa_test.go:261-264）且 g06 算子为 restart（gepa_test.go:301-303）；停滞语义（不自动复位、只显式 Reset）由 TestVistaRecordSemantics 锁定。
2. **预算耗尽输出当前最优而非空手而归**：优化侧阀门（TestGepaOptValveBudgetStopped）、评估次数中断（TestGepaExecutorEvalLimitBudgetStopped / TestGepaIncompleteChildStaysOffFrontier）、探针全断（TestGepaAllProbesBudgetTruncatedStops）、context 取消（TestGepaAbortedByContext）四种终止路径均断言 Best 非空且前沿只含完整评估成员。
3. **lineage 全程可追溯**：黄金用例断言 lineage.json 7 条记录的 算子/父代/假设/分数/准入（gepa_test.go:267-303）；远端 e2e 实录见第 5.4 节。

## 3. 算法行为测试清单与结论

27 个引擎测试（本次 `go test ./internal/engine/ -v` 实测全部 PASS，与 `grep -c "func Test"` = 27 一致）：

**Frontier（frontier_test.go，6 个）**

| 测试 | 覆盖行为 |
|---|---|
| TestDominatesTruthTable | 支配真值表 9 例：全严格/一位严格/相等不支配/互不支配/被支配/不等长/空向量/分数值 |
| TestFrontierScriptedSequence | 设计文档脚本序列：共存→淘汰→再共存；每步断言两两非支配 + 被淘汰者不支配后继；克隆与被支配候选拒绝 |
| TestFrontierRandomAdds | 500 个随机分数向量逐一准入，每步后重断两两非支配（单调性不变量） |
| TestFrontierBestHardFilter | 约束均值=1 硬过滤、无约束纯主指标、全不达约束的兜底交付（satisfied=false + note）、主指标并列时约束均值/ID 决序 |
| TestFrontierWinsAndComplement | 独占占优样本计数、互补合并对象选取（在父代失利样本上得分最高者）、无失利样本时回退主指标均值 |
| TestFrontierUniformPickDeterminism | 同种子均匀抽取序列可复现 |

**VistaGuard（vista_test.go，4 个）**

| 测试 | 覆盖行为 |
|---|---|
| TestVistaEpsilonZeroAlwaysArgmax | ε=0 时 50 轮随机池全部走 exploit 且选中 argmax-lift |
| TestVistaEpsilonOneReplaysAndExplores | ε=1 同种子选择序列可复现；20 次抽取中实证出现非 argmax 探索 |
| TestVistaRecordSemantics | 停滞计数：无进展递增、进展复位、越过阈值不自动复位（保持触发可观测）、仅显式 Reset 清零 |
| TestVistaSelectEmptyPoolAndTies | 空池返回零值+空 mode；exploit 并列按 confidence → id 决序 |

**Reflector / Mutator / advisor（reflector_mutator_test.go，10 个）**

| 测试 | 覆盖行为 |
|---|---|
| TestReflectorParsesHypotheses | 假设 JSON 三形态解析：裸 JSON / 代码围栏 / 散文包裹围栏；marker 必现于反思提示词 |
| TestReflectorClipsAndCleans | 池裁剪到 n、缺失 id 补齐、`{input}` 字面量剥除、超长截断、置信度夹取 [0,1] |
| TestReflectorRepairsInvalidJSON | 非 JSON 响应恰好一次修复调用、修复提示词携带原稿、两次调用均有 opt-calls 留痕 |
| TestReflectorEmptyResponseShortCircuits | 空响应直接报错，不触发修复调用 |
| TestMutatorRewriteProducesCandidate | 选中假设文本进入改写提示词；产物含 `{input}` |
| TestMutatorRepairsMissingPlaceholder | 缺占位符产物恰好一次修复、修复提示词携带问题列表 |
| TestMutatorFailsWithExcerpt | 修复仍失败时报错携带原文摘录 |
| TestMutatorMergeCarriesBothParents | 合并提示词同时携带父 A、父 B 与合并指导 |
| TestMutatorFreshIgnoresParents | 重启提示词只含任务规格，不携带父代材料 |
| TestAdvisorEscalationLadderAndUsage | 空内容且 finish_reason=length 时 cap 逐次翻倍（8192→16384）、成功后记住下限；全部尝试计入 optimizer 角色且不触发 executor 软停 |

**主循环（gepa_test.go，7 个）**

| 测试 | 覆盖行为 |
|---|---|
| TestGepaGoldenLoop | 6 轮黄金脚本（mock LLM 按阶段标记词路由、按措辞脚本化逐样本答案）：baseline [1,0,0,0] → g01 rewrite [0,1,1,0] → g02 克隆被拒 → g03 merge [1,1,1,1] 全淘汰 → g04/g05 被支配 → g06 停滞重启（vista_restart）；断言轮内事件顺序（start→reflect→validated→mutate→frontier→done）、exploit 审计（选中 h2、lift 0.25）、sample_done 带 candidate/round 戳、lineage/frontier/report 三工件内容、评估单元与优化调用留痕数量（18/12）、同 seed 双跑 lineage/frontier 结构一致 |
| TestGepaBudgetRoleAccounting | 1 轮内 optimizer（2 次）/executor（12 次）分角色计量精确；纯优化侧花费不武装 executor 软停 |
| TestGepaOptValveBudgetStopped | --budget-opt-tokens 恰覆盖一次优化调用：budget_stopped、交付非空 baseline、budget_stop 事件带 role=optimizer |
| TestGepaExecutorEvalLimitBudgetStopped | 评估额度在探针验证中途耗尽：不完整假设跳过（lift 不可比）、半评估子代不入前沿、lineage 标记 Incomplete |
| TestGepaIncompleteChildStaysOffFrontier | 预算在子代全量评估中耗尽：零填充的部分行不入前沿（防止误淘汰）、报告标注"预算中断评估不完整" |
| TestGepaAllProbesBudgetTruncatedStops | 全部探针被截断：立即停止而非烧注定失败的轮次；仅 1 次反思调用、无突变调用 |
| TestGepaAbortedByContext | context 取消映射 reason=aborted 且仍交付 baseline |

**cmd 层关联测试**（run_test.go，12 个全部 PASS）：零配置全链路 e2e（TestRunZeroConfigEndToEnd，mock 端点直达优化产物）、探针吃穿预算退出码 2、interactive 审批恢复、headless JSON、用法校验等。

**结论**：V3 算法行为（支配单调性、克隆拒绝、ε-greedy 审计、停滞重启、预算分角色与优雅终止、谱系可追溯、同 seed 可复现）均有测试锁定且全绿；测试与实现的对应关系见第 2 节各行证据列。

## 4. 与 GEPA / VISTA 论文语义的对应关系

引用出处见 [ROADMAP.md](../../ROADMAP.md) 理论基础表（GEPA arXiv:2507.19457，VISTA arXiv:2603.18388，2026-09-28 已逐一查证）。

### 4.1 GEPA 反射进化 + Pareto 前沿 → V3 核心循环

| 论文语义 | 本实现落点 |
|---|---|
| 反射式提示词进化：从 minibatch 失败案例的结构化反馈中产出自然语言"文本梯度"（诊断与改写假设），再据此文本突变出新候选 | `Reflector.Reflect`（reflector.go:58）：反思上下文 = 任务规格 + 父提示词 + 逐样本 输入/期望/输出/分数/ASI 诊断（eval 层的 diagnosis 即 GEPA 语境的 textual feedback）+ 祖先教训；`Mutator.Rewrite` 沿选中假设突变（mutator.go:26） |
| minibatch 训练信号（不每轮全量评估） | 每轮随机抽 `--minibatch` 条（pickMinibatch gepa.go:579-589），反思与假设探针都在 minibatch 上进行 |
| Pareto 前沿按逐样本分数（非标量均值）维护候选群体 | `Frontier` 以保留集逐样本主指标行为支配判据（frontier.go:37-51）；非支配准入/被支配淘汰/克隆拒绝（frontier.go:65-81）；最终交付仍需标量决策：约束硬过滤 → 主指标均值（`Best` frontier.go:183-199），约束指标不混入支配（量纲隔离） |
| 祖先经验（prompt lineage / 可复用的历史教训） | `Lineage.Lessons` 沿父链 BFS 收集祖先选中假设文本（lineage.go:70-102），进入反思与突变上下文；lineage.json + 报告谱系节全程留痕 |
| 候选可解释性（每条改进可回答"为什么"） | 每条 lineage 记录携带 父代/算子/选中假设原文（lineage.go:21-32）；报告按候选给出算子与假设编号（report.go:177-193） |

**与论文的有意差异（如实声明）**：

- GEPA 论文用额外 held-out 评估估计候选期望改进再更新前沿；本实现轻量化为**探针验证**——每个假设拼成"父提示词+补充指导"临时候选，在 minibatch 上实测 lift 后才参与选择（gepa.go:171-197）。探针不是正式候选（不入前沿），其预算语义（截断即跳过、不参与比较）见 gepa.go:187-197 注释与第 3 节对应测试。
- 论文的反思器按 minibatch 聚合反馈；本实现把逐样本 trace 全量（含错误响应与 ASI 诊断）交给反思器，截断界限仅按字符数（reflector.go:30-35），属工程取舍。

### 4.2 VISTA 假设解耦 + 重启 + ε-greedy → V3 稳定性防护

| 论文语义 | 本实现落点 |
|---|---|
| 假设生成与选择解耦（防"自产自选"的坏种子放大） | 生成（Reflector）、验证（探针实测 lift）、选择（VistaGuard.Select）三段互不信任：选择只看验证证据（vista.go:15-19, 47-55） |
| ε-greedy 探索 | `Select` 以概率 ε 均匀抽取验证池成员（explore），否则 argmax-lift（exploit）（vista.go:51-54）；每次选择的 mode/lift/选中假设记入 `hypotheses_validated` 事件（gepa.go:218-222），审计轨迹在事件流可回放 |
| 停滞重启（防坏种子区域滞留） | 连续无改进 ≥ `--stagnation-limit` 轮 → `Mutator.Fresh` 仅凭任务描述重设计（随机重启，mutator.go:38-40）+ `vista_restart` 事件（gepa.go:229-236）；停滞计数不自动复位、仅主循环显式 Reset——重启触发始终可观测（vista.go:57-72） |
| 状态量语义 | Record 只计停滞；progress（子代主指标均值超历史最优）复位（gepa.go:307-311） |

**与论文的有意差异（如实声明）**：

- VISTA 原文针对 LLM 启发式/优化器设计中的坏种子退化；本实现取其三机制（生成-验证-选择解耦、停滞重启、ε-greedy）作用于提示词假设池，未复刻其评测场景的其他组件。
- ε-greedy 的 explore 在**已验证假设池内**均匀抽取，非全提示词空间随机；全空间探索由 Fresh 重启承担——两层探索互补而非同一机制。

## 5. 真实 LLM 优化 e2e（tcmsp-30，run `20260929-072755-b744f51e`）

**结论：成功。** 真实模型 jiuwei-tcm 上，零配置管线在 2 轮内把保留集 f1 均值从 **0.9028 提升到 1.0000**（json_validator 全程 1.0），退出码 0，终止原因 `rounds_done`；支配淘汰、克隆拒绝、exploit 审计、谱系留痕全部按设计发生。

**数据来源与一致性**：以下数据全部为本会话经 `ssh tcmsp-30` 读取 `/home/jiuwei/PromptOpt/runs/20260929-072755-b744f51e/` 与 `synth/20260929-072755-b744f51e/` 工件所得（命令：`cat {manifest,run,frontier,lineage,report}.json|.md`、`cat synth/<id>/{filter,checkpoint,spec,samples}.json`、`ls {opt-calls,evals}`、python3 统计 events 类型与提取事件 detail）。一致性佐证：远端 `internal/engine/` 全部 13 个 .go 文件与本仓库工作区 **md5 逐一相同**（本会话实测），该 run 为本报告所描述代码线的直接产物。ask 材料给出的前沿/报告摘录与远端 `frontier.json`、`report.md` 逐字一致。

### 5.1 运行配置（manifest.json）

```json
{"model": "jiuwei-tcm", "base_url": "http://192.168.56.39:59001/v1",
 "budget_tokens": 0, "budget_evals": 40, "mode": "autopilot",
 "prompt": "从中医病历文本中抽取症状、证型与方剂，输出JSON",
 "synth_samples": 4, "probe_variants": 2,
 "max_rounds": 2, "minibatch": 3, "epsilon": 0.2, "stagnation_limit": 3,
 "seed": 2328904875761596620}
```

（`--seed 0` 自动派生并回记实际值；`--budget-opt-tokens` 未设即 0 不限。）

### 5.2 管线：合成 → p¹ 过滤 → 检查点 → baseline → 优化

- 合成 4 样本（tcm001–tcm004），任务 `tcm_medical_record_extraction`，指标 `[json_validator, f1]`、主指标 f1
- p¹ 过滤（filter.json）：保留 **tcm001**（探针分 0.909/1，方差 0.00207）与 **tcm004**（0.897/1，方差 0.00268）；剔除 noisy ×2（tcm002 0.889/0.9、tcm003 0.857/0.857——方差为 0 的样本任何改写都动不了）
- autopilot 检查点自动放行（checkpoint.json `approved`）
- baseline 在保留集上 f1 行 **[0.9091, 0.8966]、均值 0.9028**，json_validator 1.0

### 5.3 优化轮次实录（lineage.json + events.jsonl）

| 轮 | 父代 | 假设验证 | 选择（mode/lift） | 算子→子代 | 子代 f1 行 | 前沿裁定 |
|---|---|---|---|---|---|---|
| 1 | baseline | 3 条假设探针全部有效 | h1（exploit，lift +0.0972）：方剂须保留"加减/化裁/合"等完整原名，禁止简化为基础方 | rewrite → **g01** | [1, 1]，均值 1.0000 | 支配 baseline → 准入，**淘汰 baseline** |
| 2 | g01（均匀抽取） | 3 条假设探针全部有效 | h2（exploit，lift 0）：强化方剂引导词规则、不抽治则短语 | rewrite → **g02** | [1, 1]，均值 1.0000 | 与 g01 分数向量相同 → **克隆拒绝**，未准入 |

第 2 轮 progress=false 记停滞 1 次（限 3，未及重启）；轮数到达 `--max-rounds 2` → `rounds_done` 终止。假设 h1 的原文与置信度（0.92）完整留痕于 lineage.json，最终最优提示词中"荆防败毒散加减不得简化为荆防败毒散"等表述即源于该假设——**反思假设 → 突变 → 指标提升的因果链在真实数据上可追溯**。

### 5.4 最终前沿（frontier.json / report.md 摘录，远端原文）

```json
{"primary": "f1", "constraint": "json_validator",
 "best": {"id": "g01", "constraint_satisfied": true,
          "means": {"f1": 1, "json_validator": 1}},
 "members": [{"id": "g01", "round": 1, "operator": "rewrite",
              "primary_mean": 1, "json_valid_rate": 1, "wins": 2}]}
```

报告谱系节：

```text
- baseline：第 0 轮，baseline，主指标均值 0.9028，准入前沿
- g01：第 1 轮，rewrite ← baseline（假设 h1），主指标均值 1.0000，准入前沿
- g02：第 2 轮，rewrite ← g01（假设 h2），主指标均值 1.0000，未准入
```

Top-1 提示词全文见 ask 材料（与远端 report.md 一致）：在 baseline 模板基础上补齐 JSON 输出契约与方剂原名保留规则，末尾保留 `{input}` 占位符。

### 5.5 预算与事件流

- 分角色用量（run.json）：executor prompt=7494 / completion=19385（合计 26879，18 次评估调用 = baseline 2 + 两轮探针 3×2×2 + 子代 2×2）；optimizer prompt=3878 / completion=11791（合计 15669，4 次调用 = 2 反思 + 2 rewrite，`opt-calls/` 实录 001-reflect → 004-rewrite，无修复调用）
- `--budget-evals 40` 未触及（实际 executor 派发 18 次）；优化侧阀门未启用
- 事件流（events.jsonl 类型计数）：`synth_done ×1、filter_done ×1、checkpoint ×2、run_start ×1、sample_start/done ×18、round_start ×2、reflect_done ×2、hypotheses_validated ×2、mutate_done ×2、frontier_updated ×2、round_done ×2、run_done ×1`——引擎内部单元不外泄 run 生命周期事件、终局 run_done 由 cmd 层单发（gepa.go:19-23 注释，TestGepaGoldenLoop 断言），设计语义与实测一致；全程无 `vista_restart`（停滞 1 < 限 3，符合预期）
- 时长：整跑约 12 分 22 秒（07:27:55 → 07:40:17 UTC），其中优化循环约 6 分 39 秒

## 6. 门禁结果（本次实际执行）

在仓库根目录、`aa1ee1f` 提交后的干净工作区执行：

| 命令 | 结果 |
|---|---|
| `go vet ./...` | ✅ 通过 |
| `go test -race ./...` | ✅ 8 个包全部 `ok`（cmd/promptopt、config、core、engine、eval、harness、provider、web） |
| `go test ./... -v` | ✅ `--- PASS` 计数 132，与 `grep -rh "func Test"` 统计的 132 个测试函数一致 |
| `go build ./...` | ✅ 通过 |
| modernize（与 CI 同命令：`go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest -test ./...`） | ✅ 退出码 0、无输出 |
| staticcheck | ⏸ not run（未接入 CI，未在本机运行）——ci.yml 实读为 vet → modernize → test -race → build 四步，与 V2 报告第 5 节记录一致，该缺口仍开放 |

远端 tcmsp-30 的 build/vet/test/modernize 已随 `aa1ee1f` 提交说明报告全绿（该 run 二进制即其产物）；本节为本地工作区的独立复核。

## 7. 已知边界与后续

1. **Web 侧尚未呈现优化过程**：lineage / frontier / report 已落盘可读，但 V4 的 Pareto 前沿看板、候选 diff、预算仪表未做——当前"可视化"是 report.md 与 `promptopt serve` 的 run 摘要
2. **探针验证成本**：每个假设在 minibatch 上全量评估（O(假设数 × minibatch)/轮），黄金用例与 e2e 均为每轮 3 假设；minibatch 大、假设多时评估次数增长可观，预算阀门可兜底
3. **假设间串行验证**：为预算顺序确定（gepa.go:168-170），并行度只在单元内 worker 层；若需跨假设并行需重新设计预算截断语义
4. **优化侧阀门是调用级前置检查**：`ValveTripped` 在每次 dial 前评估（calls.go:91-94），不中断进行中的调用——阀门触发点存在一次调用的粒度误差（测试 TestGepaOptValveBudgetStopped 以恰好一次调用的额度锁定了行为）
5. **上期遗留未清**（非本期范围，仍开放）：staticcheck 缺位、LICENSE 文件缺失（README badge 死链）、Anthropic 原生 Provider、SQLite store、LLM-judge 指标、demo GIF

---

*报告内所有结论均基于本会话实际读取的文件（本地代码与 `ssh tcmsp-30` 远端工件，命令见第 5 节）、实际执行的命令（第 3 节测试与第 6 节门禁）；未验证项已逐条标注 not run。*
