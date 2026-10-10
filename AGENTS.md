# PromptOpt 开发指南

> **v2（Go 重写）已落地 V0–V6**：GEPA 反射进化 + 多范式提示词优化平台，Go 同仓库原地重写。v1 Python 代码归档于 tag `v0.1-python`。新路线见 [ROADMAP.md](ROADMAP.md) 与 [PRD-0000](docs/prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md)（父 Issue #49）。Go 实现须遵守 [JetBrains go-modern-guidelines](https://github.com/JetBrains/go-modern-guidelines)。

## 基本原则

1. **第一性原理**: 所有结论必须基于严密的证据或可信的信源，不编造、不臆测
2. **遵守 Go 现代规范**: 必须遵守 [JetBrains go-modern-guidelines](https://github.com/JetBrains/go-modern-guidelines)，按 `go.mod` 声明的版本（Go 1.26）使用现代习语
3. **标准库优先**: 优先使用标准库，引入第三方依赖须有明确理由；SQLite 须用纯 Go 驱动（免 cgo）

## 构建与测试命令

```bash
# 构建单二进制
go build ./...

# 构建 CLI 到当前目录
go build -o promptopt ./cmd/promptopt

# 静态检查
go vet ./...

# 运行测试（完整跑加 -race）
go test ./...

# 安装 CLI 到 $GOPATH/bin
go install ./cmd/promptopt
```

CI（[.github/workflows/ci.yml](.github/workflows/ci.yml)）执行 `go vet ./...`、staticcheck（钉版 2026.2.1）、modernize、`go test -race ./...`、`go build ./...` 五道门禁，全绿才可合入；门禁语义与 PR 优化对比模板见 [docs/release.md](docs/release.md)。

## 核心架构

```text
PromptOpt/
├── cmd/promptopt/        # CLI 入口：run / serve / verify / rollback / replay / anchor / config / mcp / version（标准库 flag）
├── internal/
│   ├── anchors/          # 锚点库文件存储与流转：confirmed/staging 双层、input_hash 去重、Promote 人工确认（ADR 0001 治理机制化）
│   ├── mcp/              # MCP 单机 API 面（提案 §2.4 最小版）：stdio JSON-RPC 三工具 optimize/verify/runs，stdout 只走协议帧（见 docs/mcp.md）
│   ├── pool/             # 跨 run 候选池（提案 §3.3 最小版）：pool/<task-key>/pool.jsonl 追加制，同集（ID+input_hash 双一致）配对对照
│   ├── stats/            # 配对自助法单一实现（verify 门禁与候选池共用同一统计口径，PCG 固定种子可复现）
│   ├── config/           # flag 默认值与 PROMPTOPT_* 环境变量解析
│   ├── core/             # Task / Candidate / Dataset / RunResult 模型与 YAML 加载、split 过滤
│   ├── engine/           # 优化引擎共享骨架：Loop 脚手架 / Advisor 调用管道 / Reflector / 突变 / Pareto 前沿 / VistaGuard / lineage（Optimizer 接口落位于此）
│   ├── optimizers/       # 多范式注册表 + 范式路由器 + builtin 清单；子包 protegi / miprov2 / evoprompt / p1（插件开发见 docs/plugins.md）
│   ├── eval/             # 并行评估引擎：worker 池、指标（含 llm_judge）、Budget 阀门、事件流
│   ├── harness/          # 零配置合成管线：任务规格/样本合成、p¹ 方差过滤、检查点门
│   ├── provider/         # Provider 接口 + OpenAI 兼容实现 + Anthropic 原生实现（重试 / usage 统计）
│   └── web/              # net/http 看板：run 列表 / 详情、四视图 dashboard 新入口页（P10 1:1 原型落地 + /runs/{id}/api/* 五数据端点）、前沿看板、trace/diff/compare、SSE 实时事件流（go:embed 模板）
├── docs/                 # PRD / ADR / research / agents 约定 / 插件与发布指南
└── examples/             # 示例任务（json_extraction / sentiment_classification / text_summarization，见 examples/README.md）
```

### 关键模型

- **Task**: 任务定义（`task.yaml`），含 `prompt_template`（`{input}` 占位符）、`metrics`、`primary_metric`
- **Candidate**: 候选提示词（`candidate.yaml`），含 `id`、`prompt`
- **Dataset / Sample**: 数据集与样本，`expected` 为参考答案（字符串 / 数组 / 对象），`split` 分 train/dev/test
- **RunResult / SampleTrace / Event**: 运行摘要（指标均值、用量、退出码）、逐样本 trace、事件流（SSE 推送 + `events.jsonl` 回放）

### 评估指标

支持 `exact_match`、`f1`、`json_validator`（确定性）与 `llm_judge`（经裁判 Provider 按内置中文 rubric 打分 0~1 + 中文诊断；声明该指标的任务每次评估追加一次裁判调用，用量按 `judge` 角色单列进 UsageByRole、并继续武装 executor 软停阀——总评估成本口径不变），在 Task 的 `metrics` 中声明；`primary_metric` 指定主指标，缺省取第一个声明值。确定性指标走 eval 包内注册表（`MetricFunc` + 一行 `RegisterMetric`，`llm_judge` 为保留名禁注册，接入见 docs/plugins.md §8）；内置中医分维度指标 `tcm_f1_syndrome/treatment/formula/herb` 与宏平均 `tcm_f1_entity`（`internal/eval/metrics_tcm.go`）已注册并在 examples/json_extraction 试点。决策模型裁判级联（P7）：`--judge-backend decision` 把 llm_judge 样本改出 score 型问题发往 SystemOne 决策服务（`internal/provider/systemone.go`，POST `{url}/v1/systemone`，请求体 `{model, state, questions}` 名字索引），score 归一化 = `answers.q1.score/(levels−1)`（除数恒为该问题实际 levels 数−1，默认 4 级 rubric），与生成式裁判同尺度进均值/CI；级联回落——confidence 低于 `--judge-decision-confidence`（默认 0.5，实测 tev1:0.8b confidence 可低至 0.21/0.111）或归一化分低于 `--judge-decision-diag-below`（默认 0.6）→ 整体回落生成式 judge()（决策模型只覆盖「分数+中文诊断」合同的分数半边，回落喂饱 GEPA 反思闭环），不回落样本诊断为确定性占位（含 confidence 值可审计）；decision 调用 usage 同记 RoleJudge、CallTrace Stage=`judge-decision`；同 run 单一裁判口径贯穿探针/基线/优化循环/verify（防两种裁判静默混用），`judge_backend=decision` 缺 DecisionClient 时样本响败而非静默降级。

## 开发约定

- **遵守 [JetBrains go-modern-guidelines](https://github.com/JetBrains/go-modern-guidelines)**：按 go.mod 版本用现代习语（错误处理、接口设计、并发、slice/map 用法等）
- **输出约定**: 人类可读输出走 stderr；`--headless` 时 stdout 仅输出 JSON 运行摘要（见 [cmd/promptopt/run.go](cmd/promptopt/run.go)）
- **退出码契约**: `0` 成功；`1` 评估失败或用法错误；`2` 预算耗尽（优先于 `1`）；`3` verify 回归或约束违反（优先级 `2 > 1 > 3`，见 [cmd/promptopt/main.go](cmd/promptopt/main.go)）
- **命令面**: `run --web` 提供实时 SSE 看板，run 结束后看板驻留（可在前沿看板采纳候选）直至 Ctrl-C；`--addr`/`--port` 分立参数（run 与 serve 同规）：显式给定任一即隐含起看板（与 --web 等效，与 --headless 互斥），`--addr` 含端口以其为准原样监听（此时与 --port 互斥、用法错误退出 1），纯主机与 `--port` 组合（缺省 17700），两 flag 均未给 = `127.0.0.1:17700` 与现状逐字节一致；run 结束时人类模式 stderr 输出中文结论块（最优候选 · 主指标 Δ 与配对自助法置信判定——基线行回落 lineage.json，因严格支配会把 baseline 淘汰出前沿 · verify 三态结果或「未过门禁」指引 · `promptopt verify <run_id>` 与看板地址），--headless stdout 保持纯 JSON（结论块照走 stderr）；新版四视图看板 `/runs/{id}/dashboard`（`internal/web/templates/dashboard.html`，1:1 自 docs/prototype/dashboard-redesign.html：设计令牌 <style> 内嵌同源、四视图 tab roving JS 原样；mock 接缝替换——事件 feed 接 `/runs/{id}/events` SSE、采纳按钮接既有 POST /runs/{id}/adopt、回退卡片维持 rollback CLI 指引、总览顶部 verdict 横幅服务端读最新 verify/<ts>/verify.json 三态无则「未过门禁」），配套五数据端点 `GET /runs/{id}/api/{overview,trend,heatmap,lineage,verify}`（结论载荷+主指标均值±SD（SampleTrace.ScoresSD 聚合）+预算仪表+RoleJudge 行 / 每轮最优序列+误差条 SD / 前沿热力 Scores±SD+Reps+ε 内 near-noise 标记+约束行 / 谱系（复用 frontier 页 lineage 渲染）/ 最新 verify.json 三态+配对差分 rows 表+退出码契约表；无 verify 工件时空态）；SampleTrace 增 `rep_scores`（逐 rep 逐指标分数，reps>1 落 trace，rep-strip 柱状数据源）、frontier.json 成员增 `sd`/`reps`（omitempty，旧工件读作缺省）；既有 run/frontier/trace 页保留，index/run 页加看板卡片链接；远端冒烟 [scripts/smoke-web.sh](scripts/smoke-web.sh)（tcmsp-30 执行，`-addr 127.0.0.1 -port 17000` 起 serve 后 curl 断言页 200+CSS 同源锚点+四视图锚点、五端点关键 JSON 键、adopt 端点存在性；SKIP_REMOTE=1 本机干跑）；`serve` 浏览历史 run 并提供产物干预端点（`POST /runs/{id}/adopt` 写 adopted.json），仍不暴露实时端点；只读设置引导页 `GET /settings`（PRD-0001 D6）两模式挂载——run --web 起看板前 `config.PublishSnapshot` 发布合并快照优先（含 flag 层，「文件 0.7 + flag 显式 0」页面如实显示 0）、serve 回落发现链+env 并注明「当前环境」语义，逐键来源标注与 config list 同一 per-key 收集器口径，api_key/judge_api_key 恒掩码，缺参缺口给 YAML 片段/命令行/环境变量三途径指引，无配置写端点；`verify <run_id>` 对交付候选做锚点/锚点库/合成保留集回归门禁（退出码 3=回归或约束违反；`--anchor` 手传锚点集与 `--anchor-lib <task-key>` 加载库 confirmed 层互斥，`--promote` + `--task-key`（缺省回落 manifest.task_key）把本次验证集按逐样本 verdict 回流 staging 区，人工确认才升正式库——库加载点仅 verify 验证集一处、永不进优化循环）；`anchor` 子命令沉淀锚点库：`anchor add <dataset.yaml> --task-key K [--confirmed]`（默认 staging，按 input_hash 去重）/ `anchor list --task-key K` / `anchor promote --task-key K [--ids]`，库落 `anchors/<task-key>/`（samples.yaml 正式库 + staging/ 候选区，git 可版本化）；`run --task-key` 显式指定库键（缺省回落：零配置 = core.HashInput(用户提示词) 前 12 hex，三件套 = task 文件 stem；快照 manifest.task_key）；`rollback <run_id>` 回退采纳（历史追加 adopted-history.jsonl，`--emit` 导出 candidate.yaml）；`replay <run_id>` 输出完整调用与决策审计时间线（`--headless` 为 JSONL）。跨 run 候选池（提案 §3.3 最小版，`internal/pool`）：run 两模式评估完成后按 manifest task_key 把交付候选追加写池（`<out>/../pool/<task-key>/pool.jsonl`，一行一 run：候选 + 均值 + 逐样本 primary 行 + 逐样本 input_hash——仅 Means 无法配对），并对照池内历史最优：同集（样本 ID 序列一致【且】逐样本 input_hash 一致，防合成集确定性 ID 伪配对）→ 配对自助法 CI（`internal/stats`，与 verify 同一口径、同一固定种子），下界超阈值（默认 0.05）→ stderr 中文退化预警行 + manifest `pool_warning` 字段；不同集 → 仅 stderr 均值参考提示（「跨 run 样本集不同，仅均值参考」）。最小边界：只做存储+对照预警，不做准入去重、不喂回优化循环、不做池 UI；预警不进 verify 门禁，退出码契约不变；`mcp` 子命令提供 MCP 单机 API 面（提案 §2.4 最小版，`internal/mcp` + [docs/mcp.md](docs/mcp.md)）：stdio JSON-RPC 三工具 optimize/verify/runs——optimize 同步一次 headless run（载荷读回 runDir/summary.json 磁盘真源）、verify 对 run_id 跑门禁（连接参数走 flag > env > manifest 复现链，载荷读回 verifyDir/verify.json）、runs 列摘要卡片；stdout 只走 JSON-RPC 帧（finishRun/finishVerify 经 out io.Writer 注入，5 处 CLI 调用点传 os.Stdout 字节级不变），单机单会话、无鉴权、无多用户（PRD-0000 Out of Scope 不动摇），panic recover 转 JSON-RPC error，tools/call optimize 的 sink「每次调用一个、调用必关闭」（panic 路径 closeOrphanedSinks 兜底，countLiveSinks 探针断言无积压）；`config` 子命令管理 YAML 配置文件层（PRD-0001，`internal/config/file.go`）：发现序 `--config` > `PROMPTOPT_CONFIG` > `./promptopt.yaml` > 用户级 `promptopt/config.yaml` 首个存在文件命中即止、命中即严格解码（语法/类型/未知键硬错退出 1，shadowed 仅存在性标注），链序 run/mcp = flag > env > 文件 > 默认（10 键显式性标记，显式 flag 含显式 0 终判压过文件）、verify 主链 flag > env > manifest > 默认且仅 11 个连接身份白名单键补位（extra_body/rps/judge_max_tokens/judge_backend/两阈值等行为键 verify 一律不从 file 取值），22 环境身份键入文件、预算/轮数/温度/reps 等成本行为旋钮绝不入文件；五动作——`config init` 九问向导（非 tty 从 stdin 逐行读答案：空行取默认、EOF 余下全默认；`--api-key -` 在向导前从 stdin 前置读一行防串行错位；目标已存在 stderr 列出将被覆盖的键、非 tty 无 `--force` 拒绝覆盖退出 1）、`config list [--headless]`（人类可读走 stderr 首行 `# config:` 命中路径 + `# shadowed:` 遮蔽清单、--headless JSON 走 stdout；api_key/judge_api_key 恒掩码）、`config get <key>`、`config set <key> <value> [--scope user|project]`（仅单键校验复用 validate helper、跨键完整性留 run/verify 汇聚点；值 `-` 整读 stdin 避开 shell history/ps）、`config unset <key>`（幂等成功）；写回语义严格解码、损坏文件报错退出 1 且逐字节不变（yaml.Node 原位改写保注释）；写侧安全闸：文件 0600/目录 0700、project 作用域含密钥幂等追加 .gitignore、权限 `perm & 0o077 != 0` 位运算告警；run/verify 缺 base_url/model 报错升级为三/四途径指路（run 三途径 flag/env/`config init`、verify 加 manifest 快照第四途径、decision 缺参加 `config set judge_decision_url/judge_decision_model` 键指引，退出码 1，文案快照钉死在 cmd/promptopt/config_test.go）；`run --spec-metrics "llm_judge,f1"`（PRD-0001 D7）钉死零配置合成规格的指标——仅零配置模式合法（三件套模式显式给报错退出 1，指标以 task 文件为准），逐项经 validateMetricsList 校验（∈ core.ValidMetrics、无重复、无空项，llm_judge 合法），显式空串 = 显式交还 LLM 自选并压过文件值；config 键 `spec_metrics`（YAML 列表，显式性标记在十键集内）同链生效，文件键在 configured 模式与 optimizer/evo_variant 同句 stderr 提示不生效；覆盖点在 SynthesizeSpec 之后、样本合成之前并立即 spec.Validate()——非法指标在烧合成/探针预算前失败，原 primary 不在钉死列表时回落列表首项（不新增 --spec-primary）；manifest 快照 `spec_metrics`（omitempty，不给不出现）
- **多范式**: `--optimizer`（零配置模式专用，默认 gepa，可选 auto 或注册名——五范式 gepa / protegi / miprov2 / evoprompt / p1 均已注册，auto 默认档路由 ProTeGi、紧预算（`budgetEvals < (2+1+r_min)×kept`，r_min=2）路由 p1）+ `--evo-variant ga|de`；范式接入步骤见 [docs/plugins.md](docs/plugins.md)。`--provider openai|anthropic` 双后端可用（Anthropic 为原生 /v1/messages 客户端）。裁判隔离：`--judge-provider/--judge-base-url/--judge-model/--judge-api-key/--judge-max-tokens`（env `PROMPTOPT_JUDGE_*`，run/verify 同链）为 llm_judge 提供独立裁判 LLM 配置面，未配置时逐字段回落执行器配置、行为与历史一致；manifest 快照 `judge_*` 四字段（不含 api-key）供 verify 复现裁判现场。决策裁判后端：`--judge-backend llm|decision` + `--judge-decision-url`（env `PROMPTOPT_JUDGE_DECISION_URL`）+ `--judge-decision-model`（env `PROMPTOPT_JUDGE_DECISION_MODEL`）+ 两个阈值 flag（decision 时 url/model 必填，阈值 0=默认）；manifest 快照 `judge_backend/judge_decision_url/judge_decision_model/judge_decision_confidence/judge_decision_diag_below` 五字段，verify 按 flag > env > manifest 复现决策现场；实测基座 [scripts/smoke-decision.sh](scripts/smoke-decision.sh)（在 tcmsp-30 执行，`SKIP_REMOTE=1` 本机干跑）
- **Provider 出站面**: `--rps`（默认 0=off）对每个 Provider 客户端做令牌桶限速，与 429/5xx+Retry-After 退避叠加共存（重试请求同受限流约束）；`--timeout`（run/verify 同面，默认 180s，Go 时长如 `2m30s` 或裸秒数如 `300`；env `PROMPTOPT_TIMEOUT`，flag > env）贯穿三客户端 Config.Timeout（OpenAI 兼容/Anthropic/SystemOne 决策裁判同一 deadline）；单次尝试超时（deadline 到点）不重试（重试只会再撞同一堵墙——tcmsp-30 大基数实测 4×180s=723.9s 全废），错误信息带「调大 --timeout 或设置 PROMPTOPT_TIMEOUT」提示；run 显式值（整秒粒度）入 manifest `timeout_seconds`（缺省 180 省略），verify 按 flag > env > manifest 复现现场。`--extra-body '{"chat_template_kwargs":{"enable_thinking":false}}'` 把网关私有 JSON 字段合并到请求体顶层（同名键用户值优先，参数名随网关/模型家族而异、文档只给示例不写死契约；作用于执行器/合成/优化侧调用，裁判调用不透传）。verify 侧两 flag 走 flag > manifest 快照链（`rps`/`extra_body` 入 manifest 供复现）；逐调用审计锚点在 calls trace 的 `extra_body` 字段。部署与实测基座：[scripts/deploy-tcmsp30.sh](scripts/deploy-tcmsp30.sh)（交叉编译推送 tcmsp-30）与 [scripts/smoke-load.sh](scripts/smoke-load.sh)（大基数实测第一场，`SKIP_REMOTE=1` 本机干跑）

## 示例项目

三个示例覆盖全部指标与两种 `--optimizer` 用法，索引见 [examples/README.md](examples/README.md)（json_extraction 双指标多约束 / sentiment_classification 显式 gepa / text_summarization auto 路由）。最小工作流：

```bash
cd examples/json_extraction
promptopt run --task task.yaml --candidate candidate.yaml --dataset dataset.yaml --web
```

## 常见陷阱

1. **提示词渲染是字符串替换**: `{input}` 直接替换为样本输入，不是模板引擎——prompt 中的 JSON 字面花括号不会被吞掉
2. **flag 互斥**: `--web` 与 `--headless` 互斥；`--addr` 仅与 `--web` 搭配生效
3. **预算耗尽的退出码**: 存在因预算未派发的样本时退出码为 `2`，优先于评估失败的 `1`；Ctrl-C 中止仍算 `1`

## Agent skills

### Working principles

Apply first-principles reasoning to engineering work. Establish WHAT before determining HOW. Verify material facts before relying on them: inspect the actual code and relevant files, run the relevant commands or tests, and do not infer behavior beyond the available evidence. When verification is impossible, state the gap explicitly as an assumption; treat unstated goals and constraints the same way. Analogy is not evidence. Decompose a problem only until further decomposition can no longer change the next action. Trace every material conclusion to a fact, constraint, goal, or explicit assumption. Prefer the simplest solution that satisfies all real constraints and can be verified. Treat existing code and conventions as evidence about the system, not as unquestionable authority: understand why an existing solution works before extending, replacing, or reusing it; follow established conventions by default, and deviate only with a stated reason.

### Issue tracker

Issues live in GitHub Issues (this repo), operated via `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical triage roles with default label strings (needs-triage / needs-info / ready-for-agent / ready-for-human / wontfix). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` at repo root (lazy), `docs/prd/`, `docs/adr/`, `docs/research/`. See `docs/agents/domain.md`.

### Documentation language

All skill-produced human-facing prose (PRDs, ADRs, CONTEXT.md, issues, comments) is written in Chinese. See `docs/agents/language.md`.
