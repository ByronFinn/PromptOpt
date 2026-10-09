# V7 路线图提案：证据与信任

> **Status**: 提案（V7 尚未立项）｜ **Created**: 2026-09-30 ｜ **输入**: 三大支柱笔记（证据与信任 / 采用与生态 / 能力深化）
>
> 本文是下一期（V7）的范围规划与逐项精炼，**不改动 [ROADMAP.md](../ROADMAP.md) 正文**；V7 立项时以本文为输入走 `/grill` → PRD → issue 拆解。全部代码引用均已在本仓库当前 HEAD（92010d9）逐处核实，假设与未核实项集中列于 §6。

---

## 0. 三大支柱与优先级总览

| 支柱 | 一句话结论 | V7 处置 |
|---|---|---|
| 证据与信任 | **同意排第一，但论据与原笔记不同且更强**：当前 v2 的两个核心决策机制（前沿准入、verify 门禁）都被证实运行在单次采样的噪声之上；README 零数字 | P0 主体 |
| 采用与生态 | 裁判隔离（`--judge-*`）是「弱执行 + 强裁判」小模型路径与 llm_judge 可信度的共同前提，改动最小、提入 P0；其余进 P1；MCP 边界声明后放 V8 | 裁判隔离进 P0，其余 P1 |
| 能力深化 | p¹ 做最小预算分配式实现（P2，触发面在显式预算场景）；**TextGrad 决策：摘除**（死代码 + 定位排除）；跨 run 候选池 V8 本体、V7 只落任务键地基 | 摘除执行 + P2 |

**对「证据与信任优先级最高」的检验**（不照抄原笔记，理由重述如下）：

1. **评估天然带采样随机性，而决策机制假设它是确定的。** 执行器请求不设置温度（`internal/eval/engine.go:244-248`，`ChatRequest.Temperature` 零值经 `omitempty` 不上线，`internal/provider/provider.go:22`），由网关默认值接管（vLLM 默认 1.0、Ollama≈0.8——**假设**，见 §6-A1）；而裁判反而钉死 0（`internal/eval/judge.go:88`）。也就是说：被评分的执行路径是随机变量，给分的裁判是确定的——方向恰好反了。在此之上，前沿准入用**单次评估**的逐样本分数向量做非支配排序与完全相等判 clone（`internal/engine/frontier.go:37-51, 65-67`），一次随机波动即可决定候选生死：随机有利的候选可以挤掉真实更优的成员。
2. **verify 门禁——V6 的旗舰交付、ADR 0001 的「唯一可信闸门」、CI 的 PR 关卡——在锚点模式下统计上站不住。** 回归判定是纯均值差 vs 固定阈值（`cmd/promptopt/verify.go:416`，`reg.Regressed = Delta > o.maxRegression`，默认 0.05 `verify.go:43`），而锚点集仅 3~5 条（`verify.go:339-344`）：单样本翻转（1.0→0.0）移动均值 0.2~0.33，是阈值的 4~7 倍。门禁的判定被采样噪声主导——CI 上假回归会错误拦 PR，反向假通过会放进真退化（[docs/release.md](release.md) §2-4）。
3. **开源门面零证据。** README 无任何效果数字（全文核实，`README.md`），对一个「输出最优提示词」的工具，没有证据的「最优」只是宣称。
4. **为什么不是采用优先**：Provider/裁判隔离解决「能不能用」，统计显著性解决「信不信结果」。一个能跑、但在噪声上做采纳与门禁决策的工具，其产出不可信，会反噬采用。且裁判隔离本身就属于证据链——裁判模型不钉死，`llm_judge` 分数既不可比也不可复现。因此 V7 把两者合并推进：**证据与信任为主干，裁判隔离作为其最小采用前提同时落地**。

**排序修正**（相对原笔记）：① demo GIF + v0.2.0 tag 提到最前——GIF 从 V1 拖到 V6（[ROADMAP.md](../ROADMAP.md) V1/V6 验收项均注「环境受限待录」，`README.md:158` 占位注释），tag 将是**首个 v2 release**（`git tag -l` 实测仅 `v0.1-python`；`CHANGELOG.md` 仅有 Unreleased 段），成本一天级，先立发布容器，后面所有证据工作才有落点；② 裁判隔离从「采用」提入 P0（理由见 §2.1）；③ 对比基准压后到 P1 且先砍到 1 个任务——它依赖统计机制（用自家 bootstrap 报数字）与温度/裁判钉版（对比要控变量），且 API 成本最高。

---

## 1. 支柱一：证据与信任

### 1.1 统计显著性：多次采样 + 自助法置信区间

**结论**：V7 引入三层机制，成本递增、只把统计检验放在**决策终点**，不进优化热循环。

**现状证据**：裁判 `Temperature=0` 钉死（`judge.go:88`）；执行器温度不可设（`engine.go:244-248`）；全链路单次采样；`Dominates` 逐样本严格比较（`frontier.go:37-51`）；clone 判定 `slices.Equal`（`frontier.go:67`）；verify 判定纯均值差（`verify.go:408-442`）。

**机制草图**：

```text
A 层（噪声源显式化，先做，几乎零成本）
  新增 --temperature（executor 侧；缺省不发送 = 现状不变），文档声明：
  温度 > 0 时逐样本分数是随机变量，后续 B/C 层才有意义。裁判保持 0。

B 层（多次采样 reps，结构改动）
  --reps k（默认 1 = 现状）：verify 两侧、以及（可选的）优化循环评估单元
  每样本跑 k 次；逐样本分数 = k 次均值。
  schema：Member.Scores 语义变为均值行，新增 reps / per-sample sd 字段；
  旧工件按 reps=1 读（向后兼容）。
  预算语义：reps 消耗 executor 预算与评估次数，软停口径不变（budget.go:55-65
  只武装 RoleExecutor 阀门——裁判隔离后见 §2.1 的口径修订）。
  假设标注：温度=0 且网关命中前缀缓存（如 vLLM）时 reps>1 无信息——
  文档必须写明适用条件（§6-A2）。

C 层（决策规则换用噪声区间）
  verify（配对自助法，核心）：
    D_i = baseline_i − delivered_i（每样本 k 次均值的差；两侧同集评估，
    verify.go:147-158 固定同序同样本——天然配对，样本难度在差分中抵消）
    对 i 有放回重采样 B=1000 次 → Δ 的经验分布 → 百分位 CI [q_α/2, q_1−α/2]
    判回归 ⇔ CI 下界 > --max-regression（「退化超过噪声区间」）
    判置信通过 ⇔ CI 上界 ≤ 0
    两者之间 = 「不可判定」（inconclusive）
  退出码映射（契约 0/1/2/3 与优先级 2>1>3 不变）：
    不可判定 → 退出码 0，但 verify.json 新增 ci 字段（n/k/B/CI/结论），
    verify.md 与 PR 摘要强制标注「差异未超噪声区间，样本量不足」。
    理由：门禁必须保持可行动；给不可判定发新退出码会破坏 AGENTS.md 契约
    与全部 CI 消费方。可选 --strict（inconclusive 以 3 退出）延后决定。
    约束检查（json 合法率 / token / 延迟上限）保持确定性硬检查，不引入 CI。
    诚实预期：锚点 n=3~5 时 CI 宽到几乎必然 inconclusive——这是特性不是
    缺陷：把「假自信门禁」变成「样本不足」的诚实结论，制造 §1.2 锚点库的
    需求拉力。
  前沿准入（轻量，不做每准入自助法——k× 成本进热循环不合理）：
    噪声感知支配：新候选将挤掉现有成员（均实行严格支配）时，要求逐样本
    优势超过噪声余量 ε_i（来自 reps 的样本内标准差池化）：a_i ≥ b_i + ε_i；
    clone 判定从 slices.Equal 改为 max|a_i−b_i| ≤ ε。
    实现落点唯一：frontier.Add（frontier.go:65-81）内 Dominates 换带余量
    变体；准入错误本可被后续轮次自纠（GEPA 持续进化），所以只加余量、
    不上全量检验。
  交付决策（Best）：
    交付前对 Top 候选 vs baseline 跑一次配对自助法，报告标注置信度；
    不改 Best 排序本身——排序是启发式，标注是证据。
```

**理由**：原笔记「Δ 超过噪声区间才准入、才判回归」方向正确，但准入与门禁的噪声代价不对称——门禁与交付是**终点决策**（错了不可自纠），准入是**过程决策**（可自纠），统计火力应集中在终点。
**取舍**：reps 的 schema 改动（`Member.Scores`/trace/事件/热力表显示均值±sd）是本项最大成本；verify 先行、优化循环 reps 设为可选 flag，控制爆炸半径。
**依赖**：无外部依赖；C 层依赖 B 层的 per-rep 方差；verify 的 CI 数字是 §1.3 基准协议的统计工具，互为依赖（基准协议钉版 reps/温度）。

### 1.2 锚点库：从一次性 3~5 条到沉淀机制

**结论**：新增 `anchors/<task-key>/` 文件库 + `promptopt anchor` 子命令 + verify 双向接入（消费 `--anchor-lib`、回流 `--promote`），ADR 0001 的独立性边界原样保留。

**现状证据**：锚点 = 用户每次手传 `--anchor` YAML（`verify.go:334-345`），≥3 条硬校验、>5 条仅警告；verify 把验证集副本落 `verify/<ts>/anchor-dataset.json`（`verify.go:135-142`）但**无任何回流机制**；工具侧不存在跨 run 锚点资产。

**沉淀机制**：

```text
存储（沿用 artifact 文件决议，不引入 SQLite——PRD-0000 决议修订 2026-09-30）
  anchors/<task-key>/samples.yaml     正式库（confirmed=true）
  anchors/<task-key>/staging/         候选区（confirmed=false，待人工确认）
  每条样本元数据：source（manual / verify-failure / production-trace）、
  added_at、origin_run、input_hash（去重键）
  文件库天然可 git 版本化与跨机共享——相对 SQLite 的额外收益。

task-key（关键前提，本提案新发现的问题）：
  合成任务名由 LLM 每次 run 现场生成（internal/harness/synthesize.go:61-78，
  payload.Name 来自模型输出），跨 run 不稳定——不能用任务名当键。
  方案：显式 --task-key flag；缺省回落 = 零配置模式取用户提示词哈希前缀、
  三件套模式取 task 路径 stem。

回流通道（沉淀的三条入口）
  ① anchor add：promptopt anchor add <dataset.yaml> --task-key K
     人工导入真实样本（生产 trace、线上 badcase）——正门。
  ② verify --promote：锚点模式下把本次验证集按 verdict 分层入库，
     驱动回归判定的样本信息量最大、优先沉淀。
  ③ 保留集（合成）样本只进 staging/，人工确认后才升正式库——
     同族偏差样本不得自动获得「真实背书」地位（ADR 0001 精神）。
  独立性边界不破：入库 ≠ 进优化循环；加载点仍只有 verify 的
  verificationSet（verify.go:332-362）一个入口，永不喂给 Loop。

消费
  verify --anchor-lib K（与 --anchor 二选一）：加载 confirmed 样本；
  5 条上限警告随库自然生长而失效——库长到 20~50 条正是 §1.1 CI
  具备统计分辨率（n≥8）的前提。两个子项互为因果，必须同期落地。

取舍：V7 只做文件库 + CLI + verify 接入；审核页 UI 化确认流
（复用合成审核页先例，internal/web/templates/review.html）放 V8。
```

**理由**：「verify 功效随使用增强」的正确实现不是把 3~5 条变多变大这么简单，而是**让每次 verify 的证据留下来、让假数据进不了信任层**。staging/confirmed 两级是把 ADR 0001 的「真实性」从一次性声明变成持续执行的治理机制。
**取舍**：task-key 靠用户约定稳定性（无自动语义匹配——`--task-key` 拼错就分库，宁缺勿错）。
**依赖**：消费端依赖 §1.1 的 CI（n 变大后 inconclusive 带收窄，锚点库的价值才可感知）；task-key 同时是 §3.3 跨 run 候选池的地基。

### 1.3 对比基准：等预算实验协议

**结论**：V7 只做 1 个任务（GSM8K 固定子集）× 3 系统（PromptOpt / DSPy MIPROv2 / gepa 原版）× ≥3 seed，等预算 + 全额披露开销，数字进 README；第 2 个任务为 stretch 目标。

**现状证据**：README 零数字；release 流水线与 CI 门禁就绪（[docs/release.md](release.md) §1、§5）；CHANGELOG 无版本段（v0.2.0 将是首个 release，基准数字可随其发布）。

**协议**：

```text
任务：GSM8K 固定子集（建议 dev 200 / test 500，钉 seed），exact_match 判分。
  选它的理由：GEPA / MIPROv2 / p¹ 论文均报告该任务（跨论文可比）、
  单模块可解、判分确定性（不引入 llm_judge 的跨实现不可比）。
  未核实标注：论文各自使用的具体子集与划分，实施时须从论文与官方
  repo 逐一核对后钉死，本提案不预设。

对照系统：DSPy MIPROv2（钉版本号）、gepa 原版 Python（钉 commit）、
  PromptOpt（钉 commit）。三系统同 executor 模型、同温度、同 max_tokens、
  同数据划分、同一判分脚本。

等预算定义（诚实口径的核心）：
  executor 侧双上限同时钉：评估调用次数 N ∥ token 预算 T
  （对齐 GEPA max_metric_calls 语义与本项目 --budget-evals/--budget-tokens）。
  优化侧开销（反思/提议/合成的调用与 token）不强行对齐——各实现机制异构，
  强行对齐是伪精度——但必须全额计量并披露在结果表。
  理由：论文间互相质疑的正是隐藏开销；本项目分角色计量（core.Role）
  天然支持这个口径，应作为卖点展示。

统计：每系统 ≥3 seed；报均值 ± sd，并用 §1.1 的配对自助法给 CI
  （自食其力：我们要求用户的门禁标准，自己的 README 数字先达到）。

产物：docs/benchmark/ 协议文档 + bench/ 目录脚本（Python 侧仅服务基准，
  明确标注不进 go build、不是运行时依赖）；README 表格 + 方法论脚注 +
  原始产物（runs/ 快照）链接。
```

**取舍**：① 单模块 vs DSPy 多 stage program 的公平性质疑——用 `dspy.Predict` 单模块配置做最小对等对照，结论边界声明为「单模块任务的等预算对比」；② API 费用由维护者自付（**假设**，§6-A4）；③ **数字可能不好看也要诚实发布**——这是证据支柱对自己的要求，差结果进 README 比没有数字更能建立可信度。
**依赖**：§1.1（CI 计算）、§2.1（裁判隔离不直接需要——基准用 exact_match，但 llm_judge 任务的 compare 可比性受益）、v0.2.0 发布容器（§1.4）。

### 1.4 demo GIF 与 v0.2.0 tag

**结论**：P0 首项，一天级成本。录 GIF（前沿看板热力表 + 采纳候选流程，替换 `README.md:158` 占位注释）→ README 补基准/证据章节（若 §1.3 已出数）→ CHANGELOG Unreleased 落 v0.2.0 段 → 打 tag 走 goreleaser（钉版与流程见 [docs/release.md](release.md) §5）。
**理由**：GIF 待录已横跨 V1→V6 五个里程碑（[ROADMAP.md](../ROADMAP.md) 两处验收注记）；tag 是首个 v2 release，后面所有 V7 交付都需要一个发布容器。
**依赖**：goreleaser 已就绪且钉 v2.13.0（Go 1.26 工具链约束，解钉条件已记录于 release.md §5.1）。
**顺手的文档债**：`README.md:120` 与 `README.md:236` 的「已知边界：protegi 注册待补 / `--provider anthropic` 接线待补」已落后于 92010d9 的现状（`internal/optimizers/builtin` 四范式注册齐全，AGENTS.md 命令面亦已更新）——随 v0.2.0 一并修正。

---

## 2. 支柱二：采用与生态

### 2.1 裁判隔离（--judge-*）——从「采用」提入 V7 P0

**结论**：裁判获得独立的 Provider/Model/MaxTokens 配置面，用量按新角色 `RoleJudge` 单列报告；未配置时行为与现状完全一致。

**现状证据**（全部核实）：裁判与执行器共用同一 Provider 实例、复用 `e.Model/e.MaxTokens`、Role 硬编码 `core.RoleExecutor`（`judge.go:84-90`）；用量记入 executor 参与软停（`judge.go:117` + `budget.go:55-65` 只武装 RoleExecutor 阀门）；`UsageByRole` 无裁判维度（`internal/core/types.go:16-17` 仅 Executor/Optimizer）。接缝唯一：裁判 Provider 消费点仅 `judge.go:83-92` 的 req 构造一处。

**最小设计**：`eval.Engine` 加可选 `JudgeProvider/JudgeModel/JudgeMaxTokens`（nil/空回落 executor 值）；`core.Role` 加 `RoleJudge`（UsageByRole 是 map，加性变更，旧工件可读）；**预算口径决策**：judge 用量继续计入 executor 软停阀门（总评估成本口径不变，与 verify `--max-avg-tokens`「含 judge」口径一致，release.md §4.1），但按 `RoleJudge` 单列进 UsageByRole 与报告——先给可见性，预算分离策略（judge 独立阀门）留观察后再议；CLI 侧 `--judge-provider/--judge-base-url/--judge-model/--judge-api-key/--judge-max-tokens` + `PROMPTOPT_JUDGE_*` env（`internal/config/config.go:13-16` 加常量，沿用 `cmp.Or` 三级回退，`config.go:57-59` 先例）。

**理由**：① 1.8B 小模型路径的现实形态是「弱本地模型执行 + 云端强模型裁判」——裁判不隔离，这条路径既不可信（弱模型给自己打分）也不经济（judge 开销挤占 executor 软停预算且不可见）；② llm_judge 的可比性与可复现性要求裁判配置钉死，这是证据支柱的一部分；③ 改动集中（消费点唯一），是三大支柱里性价比最高的一项，故破例从「采用」提入 P0。
**取舍**：裁判独立预算阀门不做（口径分裂风险）；`Temperature` 沿用钉 0 不开放。
**依赖**：无；§1.3 的 llm_judge 任务对比受益。

### 2.2 Provider 增强：extra_body 透传、关思考、限流、小模型容错

**结论**：V7 P1 做 `ExtraBody` 透传 + executor `--temperature`；**不做主动客户端限流**；1.8B 容错的主路径 = 关思考（透传解决）+ 裁判隔离（§2.1），不单做模型特判。

**现状证据**：`ChatRequest` 仅 model/messages/max_tokens/temperature 四字段（`provider.go:16-23`），全仓无 extra-body / chat_template_kwargs 通道；无主动限流（无 golang.org/x/time 依赖），并发只受 `--workers` 约束；重试为 429/5xx + transport 错误指数退避（默认 4 次、500ms 基数，`openai.go:51-53`），**已尊重 Retry-After**（`retry.go:58-59`，上限 1 分钟，`retry.go:15-17`；解析在 `openai.go:108`）。

**设计**：`ChatRequest` 加 `ExtraBody map[string]any \`json:"-"\``（`attempt` 序列化时合并进 payload 顶层，`openai.go:81-90`）；CLI `--extra-body '{"chat_template_kwargs":{"enable_thinking":false}}'`。**理由**：本地网关（vLLM/Ollama 跑 qwen 系）以 `chat_template_kwargs.enable_thinking=false` 关思考是 1.8B 级模型的刚需——思考模式把 max_tokens 烧光、正文为空（**假设**：具体参数名随网关模型而异，文档给示例不写死，§6-A3）；通用 `ExtraBody` 比 `chat_template_kwargs` 专用字段覆盖 `top_k` 等所有网关私有面。**取舍**：限流不做——退避 + Retry-After 已是被动的正确机制，主动限流需引第三方依赖（违反标准库优先原则），且无实测需求支撑；**触发条件**：§1.3 基准或用户实测出现 429 风暴再议（V7 拉伸项）。
**依赖**：executor `--temperature` 已随 §1.1 A 层落地（同一 flag 面）。

### 2.3 指标插件化 + 中医指标包

**结论**：V7 P1 建 eval 包内指标注册表（仿 optimizers/builtin 先例），中医维度指标包（证候/方剂/症状分维度 F1）作为首个第二消费者试点。

**现状证据**：指标硬编码于 switch（`internal/eval/metrics.go:23-27`，case json_validator/exact_match/f1；llm_judge 在引擎指标循环特判分发，`engine.go:230-234` 注释）；`Task.Metrics` 是 `[]string` 名字引用，扩展无配置面。团队日常在中医抽取任务上使用（README 快速开始、三示例全部中医/通用抽取）。

**设计**：`MetricFunc` 注册表 + builtin 注册（一个指标 = 一个函数 + 注册一行，`docs/plugins.md` 的注册表模式平移）；task.yaml 仍名字引用不改格式；中医包 = 从 expected/output 的结构化 JSON 按维度键分组计算独立 F1（维度键从 examples/json_extraction 的 expected schema 取——**假设**：具体维度 taxonomy 实施时核对，§6-A5）。**取舍**：不做外部动态加载（Go plugin 破坏单二进制与交叉编译，且 V5 插件面本就是 in-repo 的，plugins.md §4 已诚实声明）；llm_judge rubric 可配置化不做（裁判隔离先行，rubric 配置面等真实需求）。
**依赖**：无；为远期指标生态（含 rubric 配置化）立地基。

### 2.4 MCP server / 单机服务化：V8 候选，V7 只写边界

**结论**：不进 V7。边界声明：只做单机 API 面、不做多用户（PRD-0000 Out of Scope 不动摇）；触发条件 = 出现真实的 agent 集成需求。
**理由**：CLI + Web 看板已覆盖单用户全闭环（run --web 驻留 + serve 历史 + adopt 端点，README「验证、回退与审计」节）；MCP 是新的 API 稳定性承诺，协议 spec 仍在演化（stdlib JSON-RPC over stdio 可实现，但 spec 跟随是持续成本）；V7 主干是证据，不宜并行开新面。**取舍**：V8 立项时优先评估官方 Go SDK 依赖 vs stdlib 自实现（标准库优先原则要求前者给出明确理由）。

---

## 3. 支柱三：能力深化

### 3.1 p¹ 范式：最小可行机制

**结论**：V7 P2。p¹ 做成「预算分配策略」而非新搜索引擎：复用全套 Loop/GEPA 反思脚手架，评估预算集中到最小辨识集 S\*，交付前全量终评。**比范式本身更重要的是修正 TightBudget 的死规则语义。**

**现状证据**：p¹ 已存在为 harness 方差过滤——探针重放（`internal/harness/filter.go:38-94`）产出逐样本探针分数与方差（`filter.go:44-70, 91`，FilterReport 落盘）；分类机制（`variance.go:52-77` Classify）完备。路由声明「预算极紧→p1」但恒降级 gepa（`router.go:13, 21`，`degradeTo{p1→gepa}`）。**该规则在零配置模式下几乎不可达**：合成管线本身消耗探针 + baseline ≥ 2×kept+kept 次评估，`budgetEvals < 2×len(kept)` 的 TightBudget 触发即意味着预算饿死 baseline、优化循环直接跳过——触发即失败（[docs/plugins.md](plugins.md) §3 已知边界，与 `run.go:389-399` routeFeatures 核实一致）。

**最小机制**：

```text
① 注册真 p1 范式（--optimizer p1 显式可用 + auto 路由可达）：
   读管线已产出的探针方差数据（免费——零配置 run 已跑过探针并落盘），
   选最小辨识集 S*（kept 中方差 Top-m，m 由剩余预算反推）；
   GEPA 反思循环只在 S* 上评估（Loop.Evaluate 的 samples 参数即接口，
   范式插件契约完全够用，docs/plugins.md §1）；
   交付前对 Best 在全 kept 上做一次终评（一次评估单元成本）。
   总预算 ≈ 探针(已花) + baseline(已花) + rounds×|S*| + kept(终评)。
② TightBudget 语义修正（路由修复的实质）：
   从「budgetEvals < 2×kept」改为「付不起 GEPA 最小有效轮次」
   （≈ (2+1+r_min)×kept，r_min=2）——修正后 auto 在紧预算下真正
   路由 p1，而不是路由去送死。
```

**诚实边界**：这是 p¹-inspired 的预算分配启发式，**不宣称复现论文数字**——「2 条高区分度样本即泛化」的主张出自 arXiv:2604.08801（[docs/adr/0002](adr/0002-gepa-p1-core-with-pluggable-optimizers.md) 引用），本仓未做该复现，文档措辞用「p¹ 式预算分配」。
**为何 P2 而非更高**：零配置下触发面几乎不存在（死规则），真实收益在显式预算场景（CI / 手工 `--optimizer p1`），用户面小；且 S\* 终评最好复用 §1.1 的 reps/CI 设施，先后有序。
**依赖**：§1.1 B 层（终评走同一次评估管道）；`degradeTo{p1→gepa}` 移除时路由器测试同步。

### 3.2 TextGrad：**摘除**（明确决策）

**结论：不做、从路由规则摘除。** 三条独立理由，任一成立即可摘除，此处三条同时成立：

1. **机制上已是死代码**：`TaskFeatures.Pipeline` 恒 false——无任何声明来源（`router.go:29-31` 注释明言 "no V5 declaration source exists"；`run.go:389-399` 只派生 JointFewShot/TightBudget/MultiConstraint 三个特征），路由规则①（Pipeline→textgrad，`router.go:80-84`）永不触发；且 textgrad 降级链终点 protegi（`router.go:10-12, 20`）与默认规则⑤的落点相同（`router.go:89-90`），规则存在与否**无任何行为差异**。
2. **产品定位排除**：多组件流水线优化在 PRD Out of Scope（[docs/prd/PRD-0000](prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md)「Out of Scope」：多轮对话 agent 与 RAG 全链路优化，远期 TextGrad 风格扩展**涵盖**多组件流水线——即做 TextGrad 就要推翻该边界）；TextGrad 的差异化价值恰在计算图多组件反向传播（[ROADMAP.md](../ROADMAP.md) 理论表），单模块场景下它严格弱于已注册且为 auto 默认的 ProTeGi（机制同源：文本梯度反馈循环）。
3. **保留成本为负价值**：一个永远不可能被路由到的范式名挂在 auto 规则表首位，误导贡献者与文档读者。

**执行方式**（随 V7 立项 PR）：删 `IntentTextGrad` 常量与 `degradeTo` 条目、`TaskFeatures.Pipeline` 字段及 `routeFeatures` 对应分支、规则①（`router.go:7-22, 39-44, 80-84` + `run.go:389-399`）；`Capabilities.SuitsPipeline` 一并摘除（同样无消费方，`docs/plugins.md` §2 同步）；规则表重编号。**重评触发条件**：task.yaml 出现任务级流水线结构化声明时再议（届时恢复规则①并真正实现范式）。[ROADMAP.md](../ROADMAP.md) 理论表 TextGrad 行的去留随立项 PR 处理（本次不动正文）。

### 3.3 跨 run 候选池：V8 本体，V7 只落任务键地基

**结论**：不进 V7 主体。V7 交付的 `--task-key`（§1.2）是它的全部前置——frontier 已按 run 落盘（`internal/engine/loop.go:243-270` Finish → WriteOutputs），V8 只需汇聚层。
**V8 机制方向**（记录不实施）：`pool/<task-key>/`（候选 + 逐样本分数 + run 溯源 + 采纳事件）；退化预警直接复用 §1.1 配对 CI（新 run 交付候选 vs 池内历史最优，超噪声区间才报警）。
**理由**：「长期资产」需要先有长期使用——V7 的 verify/锚点库正是在制造使用；键方案与统计机制先稳定，汇聚层几乎是纯增量。

---

## 4. V7 建议范围与优先级排序

**P0（核心，顺序即建议执行序）**

| # | 项 | 支柱 | 理由摘要 |
|---|---|---|---|
| 1 | demo GIF + v0.2.0 tag + README 已知边界修正 | 证据 | 一天级；发布容器先立；GIF 已拖五个里程碑 |
| 2 | 统计显著性：`--temperature` 显式化 → verify 配对自助法 CI（退出码契约不动，inconclusive 标注）→ frontier 噪声感知支配 + ε-clone | 证据 | 修复被噪声主导的两个核心决策机制 |
| 3 | 裁判隔离 `--judge-*` + `RoleJudge` 单列 | 证据/采用 | 接缝唯一改动小；小模型路径与 llm_judge 可信度共同前提 |
| 4 | 锚点库：`anchors/` 文件库 + `anchor` 子命令 + verify `--anchor-lib`/`--promote`（staging/confirmed 治理）+ `--task-key` | 证据 | CI 的 n 前提；ADR 0001 治理机制化 |

**P1**

| # | 项 | 支柱 | 理由摘要 |
|---|---|---|---|
| 5 | Provider：`--extra-body` 透传（关思考）+ 文档 | 采用 | 1.8B 刚需；改一处 req 构造 |
| 6 | 指标注册表 + 中医维度指标包试点 | 采用 | 团队日用受益；为指标生态立地基 |
| 7 | 对比基准 #1（GSM8K 子集等预算协议） | 证据 | 依赖 #2/#3 的机制稳定；成本最高故置后 |

**P2（拉伸 / 顺延 V8）**

| # | 项 | 处置 |
|---|---|---|
| 8 | p1 范式最小实现 + TightBudget 语义修正 | V7 拉伸；依赖 #2 落地 |
| 9 | reps 深化到优化循环（ε 全量启用） | V7 拉伸 |
| 10 | 客户端限流 | 仅当基准/实测出现 429 风暴 |
| 11 | 跨 run 候选池、MCP 单机 API 面、锚点库审核 UI、基准 #2 | V8 候选 |

**摘除**：textgrad intent + Pipeline 特征 + SuitsPipeline（随 V7 立项 PR 执行，§3.2）。
**明确不做（延续 PRD-0000 Out of Scope）**：多用户/多租户（MCP 边界同样受此约束）、模型微调与 RL、MCTS、外部插件市场。

---

## 5. 与既有决策的关系

* **ADR 0001**（合成评测 + 可选真实锚点）：锚点库是其治理机制化——独立性边界（永不进优化循环）原样保留，新增的是「真实性分级」（staging/confirmed）与回流通道。
* **ADR 0002**（GEPA+p¹ 核心，多范式可插拔）：p1 范式落地正是该 ADR「p¹ 为 v1 算法核心」承诺的补全（此前只有过滤层、无范式层）；textgrad 摘除不改接口面。
* **PRD-0000 决议修订（2026-09-30，持久化）**：锚点库与跨 run 候选池继续走 artifact 文件，不引入 SQLite——按「难逆转条件不满足」口径，若锚点库出现真实查询瓶颈再议。
* **退出码契约（AGENTS.md）**：0/1/2/3 与优先级 2>1>3 不变；统计显著性通过「报告标注」而非新退出码表达不可判定。

---

## 6. 假设与未核实清单

**A. 假设（本文推导依赖、但本仓不可直接验证）**

* **A1** 本地网关默认温度：vLLM 默认 1.0、Ollama≈0.8——executor 不发温度字段时由网关默认接管的具体值未实测，只实测了「字段不上线」（`provider.go:22` omitempty + `engine.go:244-248` 未设）。不影响结论方向（评估是随机的），影响的是噪声量级估计。
* **A2** reps 的信息条件：温度=0 且网关前缀缓存命中时，同 prompt 同输出，reps>1 无信息。适用条件须写进文档。
* **A3** 关思考参数名（`chat_template_kwargs.enable_thinking`）随网关与模型家族而异，文档给示例不写死契约。
* **A4** 基准 API 费用由维护者自付；GSM8K 子集规模（200/500）是预算估计，实施时按费用调整。
* **A5** 中医指标维度 taxonomy（证候/方剂/症状等）以 examples/json_extraction 的 expected schema 为准，实施时核对。
* **A6** verify 两侧「同集评估 → 配对差分」的有效性依赖样本间独立；合成集若存在近重复样本，配对 CI 的有效 n 会虚高——锚点库的 input_hash 去重部分缓解，合成侧去重不做（V7 范围外，记录为已知局限）。

**B. 未核实（论文与外部事实）**

* GEPA / MIPROv2 / p¹ 论文各自使用的 GSM8K 子集与划分、DSPy 与 gepa 原版的可复现配置——实施 §1.3 时逐一核对。
* p¹ 论文「2 条样本泛化」主张未在本仓复现（§3.1 已声明措辞边界）。

**C. 本次会话执行过的检查**

读文件：ROADMAP.md、docs/prd/PRD-0000、docs/release.md、docs/adr/0001、docs/adr/0002、docs/plugins.md、examples/README.md、README.md、CHANGELOG.md、internal/optimizers/router.go、internal/engine/frontier.go、internal/engine/loop.go、internal/harness/variance.go、internal/harness/filter.go、cmd/promptopt/verify.go、internal/eval/judge.go、internal/provider/openai.go、internal/provider/provider.go、internal/optimizers/builtin/builtin.go、cmd/promptopt/provider.go。命令：`ls docs`、`wc -l`（关键文件）、`grep -n routeFeatures/​Temperature/Env/Name/RetryAfter/case`（各处引用定位）、`sed -n`（budget.go 40-80、run.go 760-800、server.go 410-440、engine.go 230-260）、`git tag -l`（仅 v0.1-python）、`git log --oneline -3`。本任务未要求测试/构建检查，未运行 `go test`/`go vet`；此前勘察轮运行的 `go test ./internal/eval/`（ok 0.919s）与 `go test ./internal/web/`（ok 0.977s）系先前 ask 所为，不计入本次。
