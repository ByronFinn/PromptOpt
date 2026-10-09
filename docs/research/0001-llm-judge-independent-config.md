# 0001 - llm_judge 第二裁判 LLM：必要性与可配置化研究（v2，经红队复核修订）

Date: 2026-09-30（v1 同日；v2 依红队六条攻击逐条裁决后修订，裁决记录见 §9）

## Status

结论已定，供产品决策；本轮不含任何代码实现（实现草图见 §7，待确认后另立任务）。
v1 定级「强烈建议」，v2 经红队复核修订为 **「可选（默认回退执行器）」**——修订理由与证据变化见 §9 总评。

## 问题

三连问：**是否需要第二 LLM 做裁判？是不是必须的？是不是要做成可配置项？**

用户现实约束：单机单模型环境（tcmsp-30 上一个 llama.cpp 模型 jiuwei-tcm，无密钥——`internal/config/config.go:26-29` 的 `DefaultMaxTokens` 注释即为该环境的 e2e 记录）；PromptOpt 是治理型工具，verify 门禁（退出码 3）是核心卖点。

## 结论（TL;DR，v2 修订）

1. **要不要第二 LLM 做裁判？** 要「独立裁判 LLM 的配置位点」——**建议实现**（改动面已核实为最小：裁判 Provider 消费点唯一 `judge.go:84-92`，nil 回落即默认行为完全不变）；第二 LLM 本身非必需，按「先测后判」（§8 第一步）与场景触发。
2. **是不是必须的？** 不是。v1 的「实证自偏好」论证经复核不成立：自偏好的直接实证（Panickssery 2024）是**无参考答案的 pairwise 偏好场景**（本会话核实摘要："scores its own outputs higher than others'"，无 reference 锚定），迁移到 PromptOpt「参考答案锚定的 pointwise rubric 评分」形态属外推；且 verify 是**差分门禁**（`verify.go:413` Δ = baseline 均分 − delivered 均分，两侧同一裁判同一 rubric），对「偏爱 LLM 生成文本」「偏袒自家产物」等共同模式偏差在 Δ 中对称相消。剩余门禁威胁 = **差分偏差**（冗长/风格趋同通道——rubric 未设防）+ **run-to-run 噪声**（未测量），前者有比第二裁判便宜得多的对症干预（rubric 长度设防、`--max-avg-tokens`，见 §7 优先级），后者应先实测再定阈值。
3. **是不是要做成可配置项？** 是，作为「可选（默认回退执行器）」的载体**建议实现**。注意这不是 v1 所称「必须」：业界七对象普遍提供该配置是惯例参考而非必然（红队抽查 3/7 属实，四家未复核）；现状裁判配置=主配置，manifest 已完整快照，可复现性**今天没有缺口**——manifest 快照裁判配置是引入配置面**之后**的配套要求（已列入 §7 草图），不构成引入配置面本身的理由。

**necessity：可选（默认回退执行器）**。配置位点建议实现；「门禁场景配不同家族第二裁判」是**超出业界实践的自有判断**（七个已核框架无一提出该建议），其依据是差分偏差 + Goodhart 放大推理（标注为推理而非实证），启用与否应由 §8 的实测数字触发，实测前对当前环境不可执行。

## 代码现实（本会话逐行核实）

### 3.1 裁判与执行器当前完全同源

- `internal/eval/judge.go:83-123`（本会话 Read 全文）：judge 的 `ChatRequest` 复用 `e.Model` / `e.MaxTokens`（`:84-90`），`Role` 硬编码 `core.RoleExecutor`（`:87`），`Temperature: 0` 硬编码且注释明确是 deterministic scoring（`:88`），经 `e.Provider.Chat` 发出（`:92`）。
- `judge.go:26-34`：内置中文 rubric「只依据内容正确性评分，忽略空白、标点、大小写与 JSON 键序等格式差异」——**未对输出长度设防**；`judgeRubric` 为硬编码常量。
- `judge.go:117`：`e.Budget.RecordUsage(core.RoleExecutor, resp.Usage)`——裁判用量记入 executor 角色；`internal/eval/budget.go:60-62`（本会话 Read 全文）：软停仅 `role == core.RoleExecutor` 时武装。
- `internal/eval/engine.go:272-300`（本会话 Read）：指标循环对声明的全部指标**并行全量求值**，无「确定性可判则短路裁判」的条件逻辑；样本级 usage 是 executor+judge 总 spend，仅 `JudgeMS` 与 `CallTrace.Stage="judge"`（judge.go:100）区分。
- `internal/eval/engine.go:83-98`（本会话 Read）：`Engine` 全导出字段、无构造函数，`Provider` 字段即注入接缝；`Role` 字段已有 `cmp.Or(e.Role, core.RoleExecutor)` 零值回落先例（`engine.go:124`）。

### 3.2 verify 门禁的差分结构（v2 新增核实，回应攻击 2）

- `cmd/promptopt/verify.go:408-442`（本会话 Read）：`verdict()` 纯函数，`Delta = base.Means[primary] − deliv.Means[primary]`（`:413`），`Regressed = Delta > o.maxRegression`（`:416`）。baseline 与 delivered 两侧均由**同一裁判**（同一 Provider/Model/rubric）打分——对两侧共同的裁判偏差（偏爱 LLM 文本、偏袒自家产物的共同分量），Δ 对称相消；破坏门禁的是**差分偏差**（两侧偏差强度不同的分量，如冗长、风格趋同）与两侧各自的 run-to-run 噪声。
- `verify.go:424-431`：约束门（json_validator 交付率、avg tokens/latency 上限）是**绝对阈值、不经裁判**——冗长通道已有部分对冲手段：`--max-avg-tokens`（`verify.go:198`，**默认 0 = off**，本会话 grep 核实）。即治理工具箱里已有比第二裁判便宜的冗长对冲，只是默认未启用。
- `verify.go:65, 610, 634-637`：退出码 3 = 回归或约束违反。

### 3.3 注入接缝与构造点（本会话 grep 核实）

`grep -rn "eval.Engine{" --include="*.go" | grep -v _test` 输出恰 5 处非测试构造点：`cmd/promptopt/run.go:134`、`cmd/promptopt/verify.go:372`、`internal/harness/filter.go:48`、`internal/harness/pipeline.go:159`、`internal/engine/loop.go:138`。裁判 Provider 消费点**唯一**（judge.go req 构造一处）。

### 3.4 配置面与可复现性

- `internal/config/config.go:12-17, 57-59`（本会话 Read 全文）：四个 `PROMPTOPT_*` env 常量 + `resolve = cmp.Or(flag, env, fallback)` 三级语义。
- `cmd/promptopt/provider.go:12-21`（本会话 Read 全文）：`newProvider(name, baseURL, apiKey)` 是 Provider 唯一构造入口；model/max-tokens 是逐请求 `ChatRequest` 字段——只换 Provider 实例不够，模型名必须一起独立。
- `cmd/promptopt/run.go:639-641`（本会话 sed 核实）：`--provider` 白名单校验先例；`run.go:775-813` runManifest 快照 `Model/BaseURL/Provider`（无 api-key）。
- **可复现性现状无缺口**：裁判配置=主配置，manifest 已完整覆盖。若引入裁判配置面，manifest 必须同步快照（`verify.go:245` resolveVerifyConn 的 flag>env>manifest 回退链是现成模式），否则 verify/replay 无法复现裁判现场——这是配套要求，见 §7。

### 3.5 仓内 llm_judge 实例为空集（v2 新增核实，回应攻击 5a）

本会话运行 `grep -rn "metrics\|llm_judge\|primary_metric" examples/*/task.yaml`，输出：

- `examples/json_extraction/task.yaml:14`：`metrics: [json_validator, f1]`（primary f1）
- `examples/sentiment_classification/task.yaml:12`：`metrics: [exact_match]`
- `examples/text_summarization/task.yaml:12`：`metrics: [f1]`

**三个示例均未声明 llm_judge**——本报告所依赖的「llm_judge 做 primary + verify 门禁」场景在仓内无任何实例，属空集。连带发现：`AGENTS.md:71` 称「三个示例覆盖全部指标」，但四个指标（exact_match/f1/json_validator/llm_judge，`AGENTS.md:59`）实际只覆盖三个，该声明与 examples 现实不符（`examples/README.md:3` 的措辞「覆盖不同指标」则准确）——建议另行修订 AGENTS.md（非本报告范围，未改动）。

### 3.6 本会话运行过的检查

- `go test ./internal/eval/ -count=1` → `ok github.com/ByronFinn/PromptOpt/internal/eval 3.137s`。
- `grep -rn "eval.Engine{" ...`（§3.3）、`grep examples/*/task.yaml`（§3.5）、`grep maxAvgTokens verify.go`（§3.2）。
- `go vet`、staticcheck、全仓 `-race` 测试**未运行**（本 ask 为分析综合，非实现；如实报告）。

## 第一性原理论证链（v2 修订）

前提公理：**指标的有效性是治理工具一切结论的前提。**

**第一层：区分 llm_judge 分数的两个消费点，威胁模型不同（v2 关键修订）。**
- **消费点 A·优化回路内的绝对分数**：Pareto 前沿准入按样本级绝对分数选候选。裁判的**绝对偏差**（冗长偏好、风格偏好、能力塌缩）在此原样进入，且被优化回路放大——候选向任何可提分方向进化，评估偏差成为被优化的目标函数的一部分（Goodhart 机制，**标注：这是推理，非实证**）。威胁真实存在，但后果是「优化方向被带偏」，不是「门禁失效」。
- **消费点 B·verify 差分门禁**：Δ = base − deliv，两侧同一裁判同一 rubric（§3.2）。共同模式偏差在差分中**相消**；真正的门禁威胁收窄为三条：(1) **差分偏差**——两侧偏差强度不同的分量，主要是冗长通道（rubric 未设防 + 优化回路天然偏向变长的候选）与风格趋同；(2) **run-to-run 噪声**（温度 0 不保证服务端确定性，未测量）；(3) **能力上限**——裁判太弱时分数与质量的对应关系本身退化。
- v1 第一层「裁判偏差直接变成产品级假阳性/假阴性」对消费点 B 过宽，已按此收窄。

**第二层：偏差实证存在，但全部需要 regime 标注（v2 关键修订）。**
- **A 类·能力型**：裁判质量与模型能力强相关（GPT-4 裁判人际一致 85%；弱模型塌缩：位置一致率 GPT-4 65.0% / GPT-3.5 46.2% / Claude-v1 23.8%，MT-Bench Table 2，本会话在 arXiv html v4 定位核实）。**相左发现（v2 补收）**：同一研究（Thakur 2024，**13 个**裁判模型 × 9 个被测模型，本会话核实摘要）明确「在对 9 个被测模型的**排名**上，较小的模型甚至词汇指标也能给出合理信号」——**相对比较恰是小裁判尚可用的场景**，而 verify 门禁正是相对/差分形态。这直接削弱「门禁必须强裁判」的论证强度。
- **B 类·自偏好型**：Panickssery 2024 的自偏好实证是**无参考答案的 pairwise 偏好**场景（本会话核实摘要）；MT-Bench 自增强 +10%/+25% 为观测性且作者自注样本量有限。二者与 PromptOpt「参考答案锚定 pointwise rubric 评分」**regime 错位**，迁移属外推。且自偏好的共同分量在差分门禁中相消（§第一层），其差分分量（优化后候选输出「更像自家」导致抬分不对称）是**推理**。
- **C 类·风格/长度偏差**：冗长攻击失败率 Claude-v1 91.3% / GPT-3.5 91.3% / GPT-4 8.7%（MT-Bench Table 3，本会话核实归属；91.3% 属两个弱模型、8.7% 属 GPT-4，非实验-对照关系）；AlpacaEval 需长度统计校正（上游核实 abs）。G-Eval / Wu & Aji：裁判偏爱 LLM 生成文本、风格盖过实质。此类偏差在「差分冗长通道」上对消费点 B 有直接威胁形态。

**第三层：解耦在什么条件下值得做（v2 修订）。**
- 第二裁判**模型本身**在门禁场景的收益 = 消除共同偏差的差分分量 + 抬升能力上限 + 打开「更强裁判」旋钮；成本 = 单模型用户不可达（多占显存或需密钥）+ 引入配置面与记账复杂度。
- 更便宜的对症干预先于第二裁判存在（§7 优先级）：rubric 长度设防（纯常量改动）、`--max-avg-tokens` 启用（flag 已存在，默认 off）、确定性指标优先。
- 「先测后判」（§8）：任何干预之前，先在目标模型上实测裁判噪声与长度-分数相关性——没有数字，既定不了阈值下界，也判不了是否需要第二裁判。

**第四层：业界形态（v2 对称使用）。**
七对象全部提供独立裁判配置（惯例参考，非必然）；**同样重要的是：七个已核对象无一警告或建议「门禁场景应分离裁判」**——DSPy 的 judge 默认就跑在全局 LM 上。因此「门禁场景建议分离」是超出业界实践的自有判断，其强度只能来自差分偏差 + Goodhart 推理与未来实测，不能引用业界背书。

**结论推导**：配置位点建议实现（小改动、默认不变、为实测后的启用提供入口）；第二 LLM 非必须也非默认推荐——由实测与场景触发；一线干预（rubric 设防、约束门启用、确定性指标）优先于第二裁判。

## 文献证据链（v2 修订；核实状态逐条标注）

### 5.1 支撑证据（经 regime 限定后仍成立的部分）

- GPT-4 裁判达人际一致水平（MT-Bench 85% vs 人-人 81%）——裁判**可以**很强，且能力是选型变量（Zheng 2023；上游全文核实 + 本会话抽查 Table 2/3）。
- 裁判能力可经专门训练与任务解耦：13B Prometheus 达 0.897 人际相关（Kim 2024；上游 abs 核实，本会话未复核）——若走第二裁判路线，「不同模型可独立选型」有支撑。
- 冗长/风格偏差有量化幅度且弱模型更糟（Table 3：91.3%/8.7% 归属见 §第二层 C 类；Dubois 2024；Wu & Aji 2023）——差分冗长通道的威胁形态有文献形态学支撑。
- 「只有最强模型合理对齐」（Thakur 2024：13 个裁判模型中仅最大型的与人类合理对齐，本会话核实摘要）——绝对分数形态下小模型裁判上限低的证据。

### 5.2 相左证据（v2 大幅扩充，均如实纳入权衡）

1. **差分门禁抵消共同偏差**（本会话代码核实 verify.go:408-442）：v1 未限定的「偏差→门禁失效」不成立；verify 威胁收窄为差分偏差 + 噪声 + 能力上限。
2. **自偏好实证 regime 错位**（本会话核实 Panickssery 摘要）：pairwise 无参考锚定 vs PromptOpt 参考锚定 pointwise；迁移属外推。
3. **排名/相对形态小裁判尚可用**（Thakur 摘要原文："In terms of their ranking of the nine exam-taker models, instead, also smaller models and even the lexical metric … may provide a reasonable signal"，本会话核实）——对以差分/相对比较为主的门禁形态构成直接削弱。
4. **PromptOpt 裁判形态已在文献可靠区间**：pointwise（位置偏差不直接适用；质量差距大时位置偏差几乎消失 98.8%，Zheng）+ 参考答案锚定（裁判失败率 70%→15%）+ 温度 0 + rubric。
5. **仓内零 llm_judge 实例**（本会话 grep §3.5）：「核心卖点场景」当前是空集，门禁-裁判耦合是潜在风险而非现实故障。
6. **业界无分离建议**（七对象否定性核查，上游核实；红队抽查 3/7 属实）：不仅不禁，也不建议——分离推荐是自有判断。
7. **未检索到直接研究「同一模型在优化回路中兼任执行与裁判」的论文**（上游如实标注）——本报告所有该特例结论均为外推。
8. **「自评限非门禁用途可接受」无直接文献**——工程推断，未核实。

## 框架实践对照（七对象；红队抽查 3/7 属实，四家为上游转述未复核）

| 对象 | 裁判/评估 LLM 配置 | 回退语义 | 红队复核 |
|---|---|---|---|
| GEPA `optimize()` | `reflection_lm` 顶级参数 | 无策略时 **assert 报错** | ✅ gh api 源码属实 |
| GEPA `optimize_anything` | `ReflectionConfig.reflection_lm` | 内置默认 `openai/gpt-5.1` | 上游转述 |
| DSPy MIPROv2 | `prompt_model`/`task_model` | None → 回退全局 LM，双空 ValueError | ✅ gh api 源码属实 |
| RAGAS | `MetricWithLLM.llm` | None → init() raise | 未复核 |
| promptfoo | `--grader` 多级级联 | 多级兜底 | 未复核 |
| DeepEval | GEval `model` | env → 内置默认常量表 | 未复核 |
| LangSmith | 独立配置面 | 无默认值 | 未复核 |
| W&B Weave | `model` 必填无默认 | — | ✅ gh api 源码属实 |

对称结论（v2）：七对象**提供**独立配置（惯例：配置面值得做）；七对象**无一建议门禁场景分离**（惯例：不构成「强烈建议第二裁判」的背书）。业界无「同模型自评」警告——PromptOpt 亦无需硬约束。

## 措施与优先级（v2 重排，回应攻击 2 的措施错位）

**一线干预（对症差分偏差与噪声，成本从低到高）：**
1. **先测后判**（§8 第一步，零代码：用现有 `run` 命令重复执行即可）。
2. **rubric 长度设防**：`judgeRubric`（judge.go:26-34 硬编码常量）显式声明「输出长度不加分、与参考答案等价即可」——纯常量改动。**标注：收益未量化，文献只证明偏差存在（AlpacaEval 校正是事后统计而非 prompt 内声明），prompt 声明的实际效果需在目标模型上按 §8 实测。**
3. **`--max-avg-tokens` 启用**：约束门已存在（verify.go:198，默认 off），对冗长通道是直接对冲（Δ 判回归之外的绝对护栏），verify 报告已渲染该行（verify.go:656）。
4. **确定性指标优先**：有唯一正确答案的任务用 exact_match/f1/json_validator（零裁判偏差）；注意当前指标循环全量并行（engine.go:272-300），「确定性可判则短路裁判」是未来独立改动。

**二线（配置位点，建议实现但不紧急）：**
5. `--judge-*` 配置面 + Engine 可选字段 nil 回落（草图见 §7.x）——价值：为实测后确需第二裁判的场景提供入口；改动面小、默认行为不变。
6. manifest 快照裁判配置——**仅在 5 落地后**成为可复现性配套。

**明确不做（维持 v1 判断，理由更新）**：pairwise 模式（位置偏差风险，需换位双评才可信）、裁判独立 Role/预算（记账语义代价）、web 配置面板、本轮的确定性短路逻辑。
**从「明确不做」移出（v2 修订）**：rubric 长度设防——提为一线干预第 2 项。可配置 rubric（任务级）仍不在本轮。

### 7.x 配置面参数草图（供确认后实现，本轮不实现）

解析优先级（单一回退链，全链 cmp.Or）：`--judge-*` flag > `PROMPTOPT_JUDGE_*` env > 对应主配置解析值（回落执行器）> 行为与现状一致。

| flag | 默认 | 回落 | 说明 |
|---|---|---|---|
| `--judge-provider` | 空 | `--provider` | `openai\|anthropic` 白名单校验（仿 run.go:639-641）；非空时经 `newProvider` 构造第二实例 |
| `--judge-model` | 空 | `--model` | 独立 provider 时必须显式设置，否则第二后端收到执行器模型名 |
| `--judge-base-url` | 空 | `--base-url` | 文案写明 OpenAI `/v1` 前缀 vs Anthropic API root 差异 |
| `--judge-api-key` | 空 | `--api-key` 链（含默认 `"1"`） | env：`PROMPTOPT_JUDGE_API_KEY` |
| `--judge-max-tokens` | 0 | `--max-tokens` | 裁判输出仅 JSON verdict，可给小值 |

env：`PROMPTOPT_JUDGE_PROVIDER/_MODEL/_BASE_URL/_API_KEY/_MAX_TOKENS`（config.go Env 常量 + `JudgeXxx()` resolver）。
Engine：加全导出可选字段 `JudgeProvider/JudgeModel/JudgeMaxTokens`（零值回落，同 `Role` 的 cmp.Or 先例 engine.go:124）；`judge()` req 构造改读回落链；**兼容性验收标准：现有 judge_test.go 不设新字段须原样通过**。
透传：5 个构造点（§3.3）+ `engine.Request`（optimizer.go:99-131）；runManifest 加 `judge_*` 四字段（omitempty、无 api-key）。
记账与软停第一版不动（executor 角色、退出码 2、契约测试均不变）；UsageByRole 拆 judge 角色留作独立演进。
配套提示：llm_judge 已声明且 JudgeProvider 未设时 stderr 一行提示（不污染 headless stdout JSON）。

## 决策框架（v2 重写：先测后判）

**第 0 步·先测后判（任何场景的入口，零代码）：**
对固定候选 + 固定数据集，同一 candidate 重复 `promptopt run` k 次（k≥5）：
- 实测 llm_judge 分数 run-to-run 标准差 σ（温度 0 不保证服务端确定性）；
- 记录输出长度与分数的相关性（冗长通道是否实际打开）；
- 用数字决定：verify `--max-regression` 下界（如 ≥2σ 量级；**注意 tradeoff：放宽阈值压误报的同时加剧漏报真回归**——这不是免费午餐，须连同保留集规模一起权衡）；以及是否需要二线干预。
无此实测，任何「建议配第二裁判 / 放宽阈值」都是没有依据的（v1 的相应建议已撤回）。

**实测后若冗长通道确认打开（长度-分数相关显著）：** 优先一线干预 2/3（rubric 设防、--max-avg-tokens），复测。

**实测后仍考虑第二裁判，当同时满足：** llm_judge 是 primary/门禁指标（注意：仓内当前无此实例，§3.5）；σ 或差分偏差实测超出可接受门禁误差；存在裁判能力更强的第二模型可达（API key 或 llama.cpp 第二模型文件——本机显存可行性未评估，jiuwei-tcm n_ctx=20736 已占大头，config.go:33-37）；且一线干预实测无效。

**自评可接受（默认回落）当：** 未声明 llm_judge；或原型/粗筛用途（分数只看趋势与人工审阅 diagnosis）；或实测后 σ 与长度相关性均在门禁误差预算内。
**确定性指标优先当：** 任务有唯一规范化正确答案。
**人工抽检**：文献认可的校准环节（Wang 2023 HITLC），但样本量与流程**无定义**（统计功效分析未做）——只能作为方向，不能作为可执行规程引用。

## 相左证据与开放问题（诚实清单，v2 更新）

1. verify 差分门禁抵消共同模式偏差（本会话代码核实）——v1「实证自偏好→门禁失效」论证撤回，改为差分偏差 + 推理。
2. 自偏好/自增强实证全部 regime 错位（pairwise 无参考锚定 / 观测性小样本）→ 迁移到 PromptOpt 形态属外推。
3. 排名/相对形态小裁判尚可用（Thakur 摘要）——对门禁（相对形态）构成直接削弱；该研究的绝对对齐结论（仅最大模型合理）适用于绝对分数形态（前沿准入）。
4. 仓内 llm_judge 实例为空集（本会话 grep）；AGENTS.md:71「覆盖全部指标」与 examples 现实不符（本会话核实）。
5. 「门禁场景建议分离裁判」是超出业界实践的自有判断（七对象无一提出），依据是差分偏差 + Goodhart 推理，后者非实证。
6. verify 阈值与裁判噪声的定量关系未测量（开放实验，§8 第 0 步）；放宽阈值的误报/漏报 tradeoff 未定量化。
7. rubric 长度设防的收益未量化（文献只证明偏差存在；prompt 内声明的实际效果待实测）。
8. 本机第二裁判模型的显存可行性未评估。
9. 拆 judge Role 记账对退出码 2 语义的影响已识别未设计。
10. 「自评限非门禁用途可接受」无直接文献，工程推断未核实。
11. 未运行 go vet / staticcheck / 全仓 -race（分析类 ask，非实现；如实声明）。

## 红队复核与修订记录（v2）

| 攻击 | 裁决 | 本会话核实动作 | 修订 |
|---|---|---|---|
| 1. TL;DR「实证自偏好」与诚实清单自相矛盾；Panickssery 是 pairwise 无参考锚定 | **成立** | WebFetch arXiv:2404.13076 abs：摘要证实 pairwise 偏好（"scores its own outputs higher than others'"），无参考答案锚定 | TL;DR #2 重写；§第二层 B 类加 regime 标注；门禁风险改挂差分偏差+Goodhart 推理并标注推理 |
| 2. 差分门禁抵消共同偏差；措施错位（rubric 设防更对症且更便宜） | **成立** | Read verify.go:400-459：`Delta = base−deliv`（:413）、`Regressed = Δ>maxRegression`（:416），两侧同一裁判；grep 证实 `--max-avg-tokens` 默认 0=off（:198） | 新增 §3.2；第一层收窄为两个消费点；§7 一线干预重排（rubric 设防提级、max-avg-tokens 入列） |
| 3. Thakur 是 13 个裁判模型非 24；遗漏排名相左发现 | **成立** | WebFetch arXiv:2406.12624 abs：13 个 judge 模型 × 9 个被测；原文 "only the best (and largest) models achieve reasonable alignment"；"…also smaller models and even the lexical metric … may provide a reasonable signal" | 24→13 全文修正；§5.2 补第 3 条相左发现；上游「24」记为转述错误（或读过旧版，未核实成因） |
| 4. 「配置面必须」靠业界类比；可复现性才是第一性支柱；「无分离建议」应同等压低「强烈建议」 | **部分成立** | 代码核实：现状裁判配置=主配置，manifest 已快照（run.go:775-813），可复现性现状无缺口——可复现性是引入配置面后的配套要求，不能作为引入理由；「业界类比支撑必须」认可撤回 | 「必须落地」降为「建议实现」；业界共识降级为惯例参考；「门禁场景分离」明示为超出业界实践的自有判断；necessity 相应降级 |
| 5. 决策框架不可操作：仓内 llm_judge 空集；阈值放宽无依据且有漏报 tradeoff；第二模型显存未评估 | **成立** | grep examples/*/task.yaml：三个示例均无 llm_judge；比对 AGENTS.md:71（「覆盖全部指标」不实，缺 llm_judge） | 新增 §3.5；§8 重写为先测后判（k 次重复评估实测 σ 与长度-分数相关性，数字定阈值下界）；阈值放宽的误报/漏报 tradeoff 明示；实测前分离建议标注为对当前环境不可执行；AGENTS.md 出入如实指出（未改 AGENTS.md） |
| 6. 91.3%/8.7% 模型归属；65.0/46.2/23.8 未定位 | **成立（a）/本会话补核成功（b）** | WebFetch arXiv:2306.05685 html v4：Table 2 default prompt 下 GPT-4 65.0%、GPT-3.5 46.2%、Claude-v1 23.8%（另 rename 66.2%）；Table 3：Claude-v1 91.3%、GPT-3.5 91.3%、GPT-4 8.7% | 引用表注明模型归属与表号；红队在 ar5iv 未定位 65.0/46.2/23.8 与本会话在 arxiv html v4 定位成功的差异如实记录（渲染版本差异可能性，未深究） |

**总评（v2）**：六条攻击经证据裁决全部或部分成立。结论方向保留（可配置位点建议实现 + 默认回落行为不变 + 门禁场景分离作为先测后判触发的自有建议），但定级由「强烈建议」降为「可选（默认回退执行器）」：支撑「强烈建议」的三根柱子——实证自偏好（regime 错位）、共同偏差→门禁失效（差分相消）、业界背书（无分离建议）——均被削弱；剩余支撑（差分冗长/风格通道 + Goodhart 推理 + 能力上限）要么有更便宜的一线干预，要么须实测触发。

## 引用列表（v2 修正；核实状态分三档：本会话核实 / 上游核实（本会话转述）/ 未核实）

**代码（本会话 Read/grep/测试直接核实）**

| 引用 | 内容 | 核实 |
|---|---|---|
| `internal/eval/judge.go:26-34, 39-54, 83-123` | rubric（无长度设防）/ 参考答案锚定 prompt / judge 主体（e.Model、Role=executor、Temperature=0、RecordUsage executor） | 本会话 Read 全文 |
| `internal/eval/engine.go:83-98, 124, 203-204, 272-300` | Engine 接缝 / cmp.Or 先例 / Snapshot / 指标循环全量并行 | 本会话 Read |
| `internal/eval/budget.go:53-63` | per-role 记账；软停仅 RoleExecutor | 本会话 Read 全文 |
| `cmd/promptopt/verify.go:65, 198, 245-253, 408-442, 610, 634-637, 656` | 退出码 3 / --max-avg-tokens 默认 0=off / resolveVerifyConn / **verdict 差分门禁** / 约束门绝对阈值 | 本会话 Read + grep |
| `internal/config/config.go:12-17, 21, 26-29, 47, 57-59` | env 常量 / jiuwei-tcm e2e 注释 / resolve 三级 | 本会话 Read 全文 |
| `cmd/promptopt/provider.go:12-21` | newProvider 工厂 | 本会话 Read 全文 |
| `cmd/promptopt/run.go:639-641, 775-813` | provider 白名单 / runManifest 快照（现状含主配置即含裁判配置，无缺口） | 本会话 sed |
| 5 处 `eval.Engine{}` 构造点 | run.go:134 / verify.go:372 / filter.go:48 / pipeline.go:159 / loop.go:138 | 本会话 grep |
| `examples/*/task.yaml` | 三示例指标：[json_validator,f1] / [exact_match] / [f1]，**均无 llm_judge**；AGENTS.md:71「覆盖全部指标」不实 | 本会话 grep |
| 基线 | `go test ./internal/eval/ -count=1` → ok 3.137s | 本会话运行 |

**文献（核实状态逐条标注）**

| 引用 | 关键结论 | 核实 |
|---|---|---|
| Zheng et al. 2023, arXiv:2306.05685 | GPT-4 裁判 85%/人-人 81%；Table 2 位置一致性 GPT-4 65.0%/GPT-3.5 46.2%/Claude-v1 23.8%；Table 3 冗长失败率 **Claude-v1 91.3%、GPT-3.5 91.3%、GPT-4 8.7%**（模型归属已注明）；自增强 +10%/+25%（作者自注样本量有限）；参考答案 70%→15%；质量差距大时位置偏差消失 98.8% | Table 2/3 与归属为本会话 WebFetch arXiv html v4 核实；其余为上游全文核实、本会话转述 |
| Panickssery et al. 2024, arXiv:2404.13076 | 自我识别与自偏好线性相关、因果链；**实验形态为无参考答案的 pairwise 偏好**（regime 与 PromptOpt 参考锚定 pointwise 错位） | 本会话 WebFetch abs 核实形态与关键句 |
| Thakur et al. 2024, arXiv:2406.12624 | **13 个**裁判模型 × 9 个被测（v1 误作 24，已修正）；仅最大模型与人类合理对齐；**排名任务上较小模型甚至词汇指标也能给出合理信号**（相左发现） | 本会话 WebFetch abs 核实（摘要级） |
| Wang, Peiyi et al. 2023, arXiv:2305.17926 | 顺序操纵 66/80；MEC/BAL/HITLC；一作 Peiyi Wang（背景材料误记「Liu et al.」已纠正） | 上游核实，本会话转述 |
| Liu, Yang et al. 2023, arXiv:2303.16634（G-Eval） | Spearman 0.514；裁判偏爱 LLM 生成文本 | 上游核实（0.514 本会话未复核） |
| Wu & Aji 2023, arXiv:2307.03025 | 风格盖过实质 | 上游核实（红队复核通过） |
| Dubois et al. 2024, arXiv:2404.04475 | 长度校正后与人类榜 Spearman 0.94→0.98 | 上游核实（数字本会话未复核） |
| Shi et al. 2024, arXiv:2406.07791 | 位置偏差由质量差距驱动 | 上游核实（红队复核通过，摘要级） |
| Kim et al. 2024, arXiv:2310.08491 / 2405.01535 | Prometheus 0.897；自定义 rubric | 上游核实（数字本会话未复核） |
| Wang, Jiaan et al. 2023, arXiv:2303.04048 | reference-biased 数据集上裁判分化 | 上游核实（本会话未复核） |
| Agrawal et al. 2025, arXiv:2507.19457 | GEPA 胜 RL；反思模型配置摘要未载（未核实） | 上游核实（本会话未复核） |
| Gu et al. 2024, arXiv:2411.15594 | 综述地图 | 上游核实（摘要级，本会话未复核） |

**框架（红队抽查 3/7 属实；RAGAS/promptfoo/DeepEval/LangSmith 四家为上游转述未复核）**

gepa-ai/gepa `api.py:54, 241-247`（assert 派，红队复核属实）、`gepa_launcher.py:735-765`（内置默认）；stanfordnlp/dspy `mipro_optimizer_v2.py:64-65, 90-91, 106-107`（回退派，红队复核属实）；explodinggradients/ragas `base.py:166, 183-186, 379-386`（raise 派，未复核）；promptfoo model-graded docs（级联，未复核）；confident-ai/deepeval `g_eval.py:61, 83` + `constants.py:5-33`（混合派，未复核）；LangSmith docs（独立配置面，未复核）；wandb/weave `llm_as_a_judge_scorer.py:38`（必填派，红队复核属实）。

**任务输入材料**：代码勘察三份与文献证据三份由工作流上游产出，其代码行号经本会话 §3 抽查全部吻合；上游文献核实方式（WebFetch/arXiv API/curl、WebSearch 429 改道）在各输入 summary 中声明，本报告如实沿用并标注三档核实状态。
