# PromptOpt

[![CI](https://github.com/ByronFinn/PromptOpt/actions/workflows/ci.yml/badge.svg)](https://github.com/ByronFinn/PromptOpt/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/ByronFinn/PromptOpt.svg)](https://pkg.go.dev/github.com/ByronFinn/PromptOpt)
[![Go Report Card](https://goreportcard.com/badge/github.com/ByronFinn/PromptOpt)](https://goreportcard.com/report/github.com/ByronFinn/PromptOpt)
[![License: MIT](https://img.shields.io/badge/License-MIT-informational.svg)](LICENSE)

**输入提示词，输出最优提示词——开源的多范式提示词优化平台。**

PromptOpt 把提示词调优从"手工试错"变成预算受控、可干预、可托管的自动化流程：

```text
你的提示词
    ↓
① Harness Builder   AI 合成任务规格 + 评测集 + 指标（p¹ 方差过滤提纯）
② 优化引擎           GEPA 反射进化 + Pareto 前沿，预算阀门控制
③ 输出               最优提示词 + lineage + 解释性报告

全程：localhost 仪表盘实时可看（htmx 局部刷新 + 原生 SSE）· 检查点可干预 · --headless 全托管
```

- **预算受控**：token 用量 ∥ 评估次数双上限，耗尽优雅终止并输出当前最优
- **可干预**：Web 仪表盘实时查看运行状态与事件流，检查点处可审核合成评测集、采纳候选
- **可托管**：`--headless` 无头模式 + JSON 输出 + 规范退出码（`0` 成功 / `1` 评估失败 / `2` 预算耗尽 / `3` verify 回归或约束违反）

完整路线与理论基础见 [ROADMAP.md](ROADMAP.md)，设计决策见 [PRD-0000](docs/prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md) 与 [ADR](docs/adr/)。

## 快速开始

环境要求：Go 1.26+。

### 安装

```bash
# 方式一：go install（仓库发布 release tag 后可用）
go install github.com/ByronFinn/PromptOpt/cmd/promptopt@latest

# 方式二：源码构建（当前推荐）
git clone https://github.com/ByronFinn/PromptOpt.git
cd PromptOpt
go build -o promptopt ./cmd/promptopt
```

### 零配置模式：只有一句提示词

还没有任务 YAML？直接把自然语言提示词作为位置参数交给 `run`，Harness Builder 会自动合成任务规格、评测集与指标（p¹ 方差过滤提纯），经检查点确认后跑 baseline 评估，并接着进入 GEPA 提示词优化：

```bash
export PROMPTOPT_BASE_URL=http://localhost:11434/v1   # 任意 OpenAI 兼容端点
export PROMPTOPT_MODEL=qwen2.5

promptopt run "从中医病历文本中抽取症状、证型与方剂，输出 JSON" --web
```

- 默认全托管：合成 → p¹ 过滤 → 检查点自动放行 → baseline 评估 → GEPA 优化，一气呵成
- 加 `--interactive` 则在合成集检查点暂停：浏览器打开审核页增删改样本后点"批准并继续"，或直接编辑 `synth/<run_id>/checkpoint.json`
- 合成产物落 `synth/<run_id>/`（manifest / spec / samples / filter / checkpoint），评估与优化产物落 `runs/<run_id>/`；看板会给出审核页入口

### 提示词优化（GEPA 引擎）

零配置模式在 baseline 评估之后自动进入 GEPA 反射进化循环：每轮从保留集中抽取 minibatch，反思父提示词的逐样本失败生成自然语言优化假设，假设以"父提示词 + 补充指导"的临时候选实测提升（lift），ε-greedy 选定后经 rewrite / merge / restart 算子突变出新候选，按逐样本分数向量做 Pareto 非支配排序准入前沿；预算耗尽或轮数到达即优雅终止，交付当前最优提示词。

```bash
promptopt run "从中医病历文本中抽取症状、证型与方剂，输出 JSON" \
  --max-rounds 5 \
  --minibatch 4 \
  --stagnation-limit 3 \
  --epsilon 0.2 \
  --seed 0 \
  --budget-opt-tokens 0
```

优化相关 flag（均为零配置模式参数，默认值见 `internal/config/config.go`）：

| flag | 默认 | 说明 |
|---|---|---|
| `--max-rounds` | 5 | 优化轮数上限（≥1）；跑满即以 `rounds_done` 终止 |
| `--minibatch` | 4 | 每轮反思抽取的样本数（不足则全取） |
| `--epsilon` | 0.2 | 假设选择探索率（0~1）：以 ε 概率均匀探索，否则选 minibatch 实测提升最大的假设 |
| `--stagnation-limit` | 3 | 连续无改进轮数达到该值触发随机重启（`vista_restart` 事件，算子 `restart`） |
| `--seed` | 0 | 优化随机种子；`0` = 自动派生并记入 manifest，同种子可复现同一条优化轨迹 |
| `--budget-opt-tokens` | 0（不限） | 优化侧（反思/突变调用）累计 token 阀门，触发后以 `budget_stopped` 终止并交付当前最优 |
| `--budget-tokens` / `--budget-evals` | 0（不限） | 共享预算：评估侧 token / 评估次数上限，耗尽同样优雅终止输出当前最优（退出码 `2`） |

优化产物（与评估产物同落 `runs/<run_id>/`）：

```text
runs/<run_id>/
├── lineage.json         # 候选谱系：父代、算子、假设、逐样本分数、是否准入（全程可追溯）
├── frontier.json        # 最终 Pareto 前沿 + Top-1 最优提示词全文
├── report.md            # 中文解释性报告：轮次、预算消耗（分角色）、前沿成员表、取舍说明、谱系
├── evals/NN-<候选>/     # 每个评估单元（假设探针 / 子代）的逐样本 trace
└── opt-calls/NNN-<阶段>.json  # 优化侧调用留痕（反思 / 突变 / 修复），含完整请求响应与用量
```

### 多范式优化（--optimizer）

优化引擎不止 GEPA 一家：`--optimizer` 在零配置模式下选择优化范式（手工三件套模式只做单轮评估，显式传该 flag 是用法错误）。范式以"插件"形态接入——一个范式 = 一个实现 `engine.Optimizer` 的子包 + `internal/optimizers/builtin` 里一行注册，接入指南见 [docs/plugins.md](docs/plugins.md)：

```bash
# 显式指定范式
promptopt run "从中医病历文本中抽取症状、证型与方剂，输出 JSON" --optimizer miprov2
promptopt run "把电商评论归类为正面/中性/负面" --optimizer evoprompt --evo-variant de

# auto：按任务特征自动路由（决策与原因落 manifest）
promptopt run "把这条资讯压缩成一句话摘要" --optimizer auto
```

已注册范式（以 `--optimizer bogus` 的报错清单为准）：

| 范式 | 机制 | 适用 |
|---|---|---|
| `gepa`（默认） | minibatch 反思 + 逐样本 Pareto 前沿 + VISTA 防护 | 多指标权衡、生产多维约束 |
| `protegi` | UCB1 折臂选 minibatch、失败批评为文本梯度、沿梯度改写 | 通用提示词改进（auto 默认档） |
| `miprov2` | 指令变体 + train 样本 few-shot 子集联合搜索（去泄漏 minibatch 打分） | 指令 + Few-shot 复合任务 |
| `evoprompt` | LLM 充当进化算子：锦标赛交叉 / 变异 / 差分变异（`--evo-variant ga\|de`） | 群体搜索、探索型任务 |
| `p1` | p¹ 式预算分配启发式：探针方差选最小辨识集 S*，GEPA 反思只在 S* 上评估，交付前全保留集终评（预算分配策略，非论文复现） | 紧评估预算（CI / 小额度 API） |
| `auto` | 范式路由器：四级规则（预算紧张 → few-shot 复合 → 多指标 → 默认）；未注册范式回退 gepa | 不知道选哪个时 |

路由决策（`optimizer` / `optimizer_requested` / `optimizer_route_reason`）与范式专属事件（文本梯度、种群演化等）全量落 manifest 与事件流，compare 页有"范式"行可直接对比不同范式在同一任务上的结果（示例见 [examples/README.md](examples/README.md)）。

### 运行评估（配置模式）

已有任务 YAML 时，用显式的 task / candidate / dataset 三件套只做评估（不合成、不优化）。以 [examples/json_extraction](examples/json_extraction/)（中医医疗 NER 抽取）为例：

```bash
export PROMPTOPT_BASE_URL=http://localhost:11434/v1   # 任意 OpenAI 兼容端点：vLLM / Ollama / 网关
export PROMPTOPT_MODEL=qwen2.5

cd examples/json_extraction
promptopt run \
  --task task.yaml \
  --candidate candidate.yaml \
  --dataset dataset.yaml \
  --web
```

加 `--web` 后浏览器打开 <http://127.0.0.1:17700> 实时查看事件流；退出码与指标汇总见终端输出。

任务声明 `llm_judge` 时，裁判默认与执行器共用同一 Provider/模型（stderr 会有一行回退提示）；需要「弱执行 + 强裁判」时用 `--judge-provider/--judge-base-url/--judge-model/--judge-api-key/--judge-max-tokens`（env `PROMPTOPT_JUDGE_*`）给裁判独立的 LLM 连接——任一连接字段非空即构造第二实例，未设字段逐项回落执行器配置；裁判用量按 `judge` 角色单列进 UsageByRole，仍与执行器共用同一 token 软停阀（总评估成本口径不变）。

裁判还可以换成决策模型级联：`--judge-backend decision --judge-decision-url http://127.0.0.1:11434 --judge-decision-model tev1:0.8b` 把每个 llm_judge 样本改写为一道 score 型选择题发往 SystemOne 决策服务（`/v1/systemone`），概率加权分归一化到 0~1（除数 = 题目级数−1），与生成式裁判同尺度进指标均值与 CI——便宜且快若干个数量级，但决策模型只覆盖「分数+中文诊断」合同的分数半边，因此是级联而非替代：confidence 低于 `--judge-decision-confidence`（默认 0.5）或分数低于 `--judge-decision-diag-below`（默认 0.6）的样本回落生成式裁判拿中文诊断（喂饱 GEPA 反思闭环），可信样本诊断位写含 confidence 值的确定性占位（可审计）。两个阈值 flag 与连接字段均入 manifest 快照，verify 按 flag > env > manifest 复现决策现场；`judge-decision` 调用各自落 Stage=`judge-decision` 的调用 trace。同 run 单一裁判：探针/基线/优化循环/verify 全部跟随 `--judge-backend`，decision 缺客户端时样本响败而非静默混用。tcmsp-30 实测入口见 [scripts/smoke-decision.sh](scripts/smoke-decision.sh)。

面向本地小模型网关还有两个出站旋钮：`--rps 8` 客户端令牌桶限速（默认 0=off，与 429/5xx+Retry-After 退避叠加共存）；`--extra-body '{"chat_template_kwargs":{"enable_thinking":false}}'` 把网关私有 JSON 字段合并到请求体顶层（同名键用户值优先；关思考正是 1.8B 级思考模型烧光 `max_tokens` 的对症手段，参数名随网关/模型家族而异）。两 flag 均入 manifest 快照（`rps`/`extra_body`），verify 侧 flag > manifest 复现现场，逐调用审计看 calls trace 的 `extra_body` 字段。

评估产物落在 `runs/<run_id>/`：

```text
runs/<run_id>/
├── manifest.json        # 运行配置快照（复现依据）
├── events.jsonl         # 事件流，SSE 回放源
├── samples/<id>.json    # 逐样本 trace：渲染后 prompt、响应、得分、用量
└── summary.json         # 运行摘要：状态、退出码、指标均值、用量
```

### Web 仪表盘

`run --web` 随运行启动实时看板（htmx 局部刷新 + 原生 SSE，内嵌单二进制，无前端构建链），run 结束后看板驻留——可在前沿看板采纳候选，直至 Ctrl-C。

监听地址走 `--addr` / `--port` 分立参数：显式给出任一即隐含起看板（与 `--web` 等效，均与 `--headless` 互斥）；`--addr` 给完整 `host:port` 时以其为准原样监听（此时不得再给 `--port`），给纯主机时与 `--port` 组合（缺省 17700）；两者都未给即 `127.0.0.1:17700`：

```bash
promptopt run "从中医病历文本中抽取症状、证型与方剂，输出 JSON" --web
promptopt run "…" -addr 0.0.0.0 -port 17000          # 纯主机 + 分立端口
promptopt run "…" --web --addr 0.0.0.0:18080         # 完整地址原样监听
```

run 结束时人类模式 stderr 输出中文结论块：最优候选 · 主指标 Δ 与配对自助法置信判定 · verify 门禁三态结果（未跑过即「未过门禁」指引）· `promptopt verify <run_id>` 与看板地址；`--headless` 的 stdout 保持纯 JSON 不变。

<!-- TODO(V5 后补 GIF)：仪表盘截图 / demo GIF 占位（前沿看板热力表 + 采纳候选流程） -->

页面清单：

| 页面 | 路由 | 能做什么 |
|---|---|---|
| 首页 | `/` | run 卡片列表：状态、退出码、指标均值、用量（每卡片带四视图看板入口） |
| 四视图看板 | `/runs/{id}/dashboard` | 新入口页（1:1 重设计）：① 总览监控 ② 前沿与采纳（接 adopt 端点）③ 样本证据 ④ 交付门禁（顶部 verdict 横幅读 verify.json 三态，未跑过 verify 即「未过门禁」）；事件流接 `/runs/{id}/events` SSE 回放；页脚披露演示数据卡片与结构化真源（`GET /runs/{id}/api/{overview,trend,heatmap,lineage,verify}` 五数据端点：结论载荷 / 每轮最优±SD / 前沿热力±SD 与 ε 内 near-noise 标记 / 谱系 / verify 三态与配对差分表） |
| run 详情 | `/runs/{id}` | 逐样本 trace（prompt / 响应 / 得分 / 诊断）、**预算仪表**（token ∥ 评估次数实时消耗与余量）、SSE 事件流 |
| 前沿看板 | `/runs/{id}/frontier` | Pareto 候选对比：per-sample 热力表、支配关系标注、Top-1、谱系准入记录、**采纳候选**（原子写 `adopted.json`，幂等，SSE 广播） |
| trace 浏览器 | `/runs/{id}/trace` | 顶层 / `evals/*` / `opt-calls` 单元逐层下钻：样本详情、LLM 调用请求响应全文、与 dataset.json 联查 |
| 候选 diff | `/runs/{id}/diff?a=&b=` | 两候选提示词 side-by-side 行级对比（新增 / 删除统计） |
| run 对比 | `/compare?a=&b=` | 两次 run 的指标均值、分角色用量、Top-1 并排对照（含 Δ 列） |
| 优化报告 | `/runs/{id}/report` | 中文解释性报告站内渲染，下载 `report.md` 附件或自包含 `report.html` |
| 合成集审核 | `/synth/{id}` | 零配置模式检查点：增删改合成样本、批准放行（`--interactive` 时暂停等待） |

`promptopt serve` 用同一套页面浏览历史 run（含 events.jsonl 事件回放与 adopt 产物干预端点，但不暴露实时端点）；监听地址处置同 `run`（`-addr 127.0.0.1 -port 17000` 纯主机与分立端口组合）：

```bash
promptopt serve    # http://127.0.0.1:17700
promptopt serve -addr 127.0.0.1 -port 17000
```

## 验证、回退与审计

优化交付的提示词不是终点——V6 补齐了"防退化"的工程闭环：

### verify：锚点验证与回归门禁

`promptopt verify <run_id>` 对已交付候选 vs baseline 在验证集上复评：**优先用锚点验证集**（`--anchor` 传 3~5 条真实样本的 dataset YAML，或 `--anchor-lib <task-key>` 加载锚点库 confirmed 正式库样本——两者互斥，库模式无 3~5 条上限、随库生长；验证集仅用于最终验证、永不进入优化循环，ADR 0001）；未提供锚点时按原 run spec 独立重合成同契约保留集对照，报告强制标注"结论未经真实数据验证"（示例锚点见 [examples/json_extraction/anchor.yaml](examples/json_extraction/anchor.yaml)）：

```bash
promptopt verify 20260930-120001-xxxx --anchor my-anchor.yaml \
  --max-regression 0.05 --max-avg-tokens 4000 --max-avg-latency-ms 20000
```

- **回归判定**：主指标均值退化超过 `--max-regression`（默认 0.05）即回归；报告附配对自助法 CI，样本不足时诚实标注"不可判定"
- **约束硬检查**：`json_validator` 任务交付侧 JSON 合法率须 100%；`--max-avg-tokens` / `--max-avg-latency-ms` 成本与延迟上限（口径含 llm_judge 裁判开销）
- **锚点回流**：`--promote`（配合 `--task-key`，缺省回落 run manifest 的 `task_key` 快照）把本次验证集全量落库 staging 区、逐样本标注 verdict——人工 `anchor promote` 确认后才升正式库，同族偏差样本不得自动获得真实背书
- **退出码**：`0` 通过；`1` 用法或评估失败；`2` 证据不完整（预算内跑不完，优先）；`3` 回归或约束违反
- 报告落 `runs/<id>/verify/<时间戳>/`（verify.json + markdown），连接参数按 flag > `PROMPTOPT_*` 环境变量 > run manifest 回退（含裁判连接：`--judge-*` 按 flag > `PROMPTOPT_JUDGE_*` > `manifest.judge_*` 复现裁判现场；决策后端 `--judge-backend/--judge-decision-*` 同链复现，与 run 保持同一裁判口径）

### anchor：锚点库（真实样本沉淀）

`anchors/<task-key>/` 文件库（可 git 版本化，PRD-0000 决议不引入 SQLite）按任务沉淀真实样本：`samples.yaml` 为 confirmed 正式库、`staging/samples.yaml` 为候选区。`run --task-key` / `verify --task-key` 显式指定库键（缺省回落：零配置模式取用户提示词经 `core.HashInput` 规范化哈希前 12 hex——合成任务名由 LLM 现场生成不可作键；三件套模式取 task 文件名 stem），快照进 manifest 供 verify 复现。

```bash
promptopt anchor add badcase.yaml --task-key tcm --origin-run 20261007-aaaa  # 导入生产 badcase（默认落 staging）
promptopt anchor add real.yaml --task-key tcm --confirmed                    # 人工确认直接进正式库
promptopt anchor list --task-key tcm                                         # 两层清单与来源
promptopt anchor promote --task-key tcm [--ids id1,id2]                      # 人工确认后 staging → confirmed
```

每条样本带 `source`（manual / verify-failure / production-trace）、`added_at`、`origin_run`、`input_hash`（`core.HashInput` 规范化哈希，按输入去重——同一输入跨层只占一槽）。入库 ≠ 进优化循环：锚点库唯一读取点是 verify 的验证集，永不喂给 Loop（ADR 0001 独立性边界）。

### rollback：一键回退采纳

`promptopt rollback <run_id>` 回退 `adopted.json` 采纳：缺省回退到上一不同采纳、兜底 baseline，`--to <候选>` 指定目标，旧采纳追加 `adopted-history.jsonl` 留痕，`--emit candidate.yaml` 把回退结果导出为可复用的候选文件。

### replay：完整审计时间线

`promptopt replay <run_id>` 按时间排序合并 events.jsonl 与各阶段 LLM 调用留痕（合成 / baseline / 优化单元 / 优化侧 / 裁判），输出完整调用与决策审计时间线及覆盖率摘要；`--full` 展开全文，`--headless` 输出 JSONL 供脚本消费。

### mcp：MCP 单机 API 面（agent 集成）

`promptopt mcp` 在 stdin/stdout 上提供 newline-delimited JSON-RPC 2.0 三工具——`optimize`（同步一次 headless run，载荷读回 `summary.json` 磁盘真源）、`verify`（回归门禁，连接参数按 manifest 复现链、载荷读回 `verify.json`）、`runs`（运行摘要卡片）；stdout 只走协议帧，人类可读输出走 stderr。单机单会话、无鉴权、无多用户（PRD Out of Scope 不动摇）；协议取舍与工具契约详见 [docs/mcp.md](docs/mcp.md)。

### CI 集成与 Release

- [`.github/workflows/optimize.yml`](.github/workflows/optimize.yml)：PR 优化对比模板——PR 上自动跑零配置优化 → `verify` 同契约保留集门禁（退出码 3 = PR 检查失败），缺 secrets 自动跳过，指标对照表渲染进 PR 摘要页
- [`.github/workflows/ci.yml`](.github/workflows/ci.yml)：五道门禁（vet / staticcheck / modernize / test -race / build）
- [`.github/workflows/release.yml`](.github/workflows/release.yml) + [`.goreleaser.yaml`](.goreleaser.yaml)：推送 `v*` tag 触发 goreleaser，出 darwin/linux × amd64/arm64 纯 Go 单二进制 + checksums；changelog 人工维护于 [CHANGELOG.md](CHANGELOG.md)
- 门禁语义、secrets 约定与发布流程详见 [docs/release.md](docs/release.md)

## 架构一览

Go 单二进制，标准库优先：

```text
PromptOpt/
├── cmd/promptopt/        # CLI 入口：run / serve / verify / rollback / replay / version（标准库 flag）
├── internal/
│   ├── core/             # Task / Candidate / Dataset / RunResult 核心模型与 YAML 加载
│   ├── config/           # flag 默认值与 PROMPTOPT_* 环境变量解析
│   ├── provider/         # Provider 接口 + OpenAI 兼容实现 + Anthropic 原生实现（共享重试核心 / usage 统计）
│   ├── eval/             # 并行评估引擎：worker 池、exact_match / f1 / json_validator / llm_judge、预算阀门
│   ├── harness/          # 零配置合成管线：任务规格/样本合成、p¹ 方差过滤、检查点门
│   ├── engine/           # 优化引擎共享骨架：Loop 脚手架 / Reflector / Mutator / Pareto 前沿 / VistaGuard / lineage / 报告（Optimizer 接口落位于此）
│   ├── optimizers/       # 多范式可插拔：注册表 + 范式路由器 + builtin 清单 + protegi / miprov2 / evoprompt / p1 子包
│   └── web/              # 内嵌 Web 看板：run 列表/详情、前沿看板、trace 浏览器、diff/对比、报告导出、SSE 实时事件流（go:embed 模板）
├── docs/                 # PRD / ADR / research / reports / 插件与发布指南
└── examples/             # 示例任务（3 个，见 examples/README.md）
```

按 [ROADMAP.md](ROADMAP.md) 推进中的模块：`store/`（SQLite，纯 Go 驱动）——当前由 artifact 文件承担全部持久化（PRD-0000 决议修订，2026-09-30）。

## 当前状态

v2 处于 V0 → V6 已落地阶段：Go 骨架、Provider（OpenAI 兼容 + Anthropic 原生）、并行评估引擎（含 llm_judge）、Web 看板、零配置 Harness Builder（合成 → p¹ 过滤 → 检查点 → baseline 评估）、GEPA 优化引擎与完整化 Web 干预体验之上，V5 落地多范式扩展（Optimizer 注册表 + 范式路由器 + ProTeGi / MIPROv2 / EvoPrompt 实现），V6 落地工程化闭环（verify 锚点验证与回归门禁、rollback / replay、退出码 3、CI 五道门禁 + PR 优化对比模板、goreleaser Release 规范）。待办：demo GIF 待录。

## v1（Python）归档

v1 为 Python 实现，已随 v2 转向停止维护，代码归档于 tag `v0.1-python`：

```bash
git checkout v0.1-python
```

## 贡献

见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## License

MIT
