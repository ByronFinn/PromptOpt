# PromptOpt 开发指南

> ⚠️ **v2 转向中（2026-09-28）**：项目已立项 v2 —— GEPA 反射进化 + 多范式提示词优化平台，**Go 同仓库原地重写**。本文件下述 Python/uv 工作流仅适用于归档的 v1 代码（tag `v0.1-python`），V0 落地时将按 Go 工作流重写本文件。新路线见 [ROADMAP.md](ROADMAP.md) 与 [PRD-0000](docs/prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md)（父 Issue #49）。Go 实现须遵守 [JetBrains go-modern-guidelines](https://github.com/JetBrains/go-modern-guidelines)。

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

CI（[.github/workflows/ci.yml](.github/workflows/ci.yml)）执行 `go vet ./...`、`go test -race ./...`、`go build ./...`，全绿才可合入。

## 核心架构

```text
PromptOpt/
├── cmd/promptopt/        # CLI 入口：run / serve / version（标准库 flag）
├── internal/
│   ├── config/           # flag 默认值与 PROMPTOPT_* 环境变量解析
│   ├── core/             # Task / Candidate / Dataset / RunResult 模型与 YAML 加载、split 过滤
│   ├── engine/           # GEPA 优化引擎：反思 / 突变 / Pareto 前沿 / lineage / VistaGuard（Optimizer 接口落位于此）
│   ├── eval/             # 并行评估引擎：worker 池、指标、Budget 阀门、事件流
│   ├── harness/          # 零配置合成管线：任务规格/样本合成、p¹ 方差过滤、检查点门
│   ├── provider/         # Provider 接口 + OpenAI 兼容实现（重试 / usage 统计）
│   └── web/              # net/http 看板：run 列表 / 详情、SSE 实时事件流（go:embed 模板）
├── docs/                 # PRD / ADR / research / agents 约定
└── examples/             # 示例任务（json_extraction）
```

### 关键模型

- **Task**: 任务定义（`task.yaml`），含 `prompt_template`（`{input}` 占位符）、`metrics`、`primary_metric`
- **Candidate**: 候选提示词（`candidate.yaml`），含 `id`、`prompt`
- **Dataset / Sample**: 数据集与样本，`expected` 为参考答案（字符串 / 数组 / 对象），`split` 分 train/dev/test
- **RunResult / SampleTrace / Event**: 运行摘要（指标均值、用量、退出码）、逐样本 trace、事件流（SSE 推送 + `events.jsonl` 回放）

### 评估指标

支持 `exact_match`、`f1`、`json_validator`，在 Task 的 `metrics` 中声明；`primary_metric` 指定主指标，缺省取第一个声明值。

## 开发约定

- **遵守 [JetBrains go-modern-guidelines](https://github.com/JetBrains/go-modern-guidelines)**：按 go.mod 版本用现代习语（错误处理、接口设计、并发、slice/map 用法等）
- **输出约定**: 人类可读输出走 stderr；`--headless` 时 stdout 仅输出 JSON 运行摘要（见 [cmd/promptopt/run.go](cmd/promptopt/run.go)）
- **退出码契约**: `0` 成功；`1` 评估失败或用法错误；`2` 预算耗尽（优先于 `1`）
- **命令面**: `run --web` 提供实时 SSE 看板，run 结束后看板驻留（可在前沿看板采纳候选）直至 Ctrl-C；`serve` 浏览历史 run 并提供产物干预端点（`POST /runs/{id}/adopt` 写 adopted.json），仍不暴露实时端点

## 示例项目

参考 [examples/json_extraction/](examples/json_extraction/) 了解最小工作流：

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
