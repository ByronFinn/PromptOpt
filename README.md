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

全程：localhost 仪表盘实时可看（htmx + SSE）· 检查点可干预 · --headless 全托管
```

- **预算受控**：token 用量 ∥ 评估次数双上限，耗尽优雅终止并输出当前最优
- **可干预**：Web 仪表盘实时查看运行状态与事件流，检查点处可审核合成评测集、采纳候选
- **可托管**：`--headless` 无头模式 + JSON 输出 + 规范退出码（`0` 成功 / `1` 评估失败 / `2` 预算耗尽）

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

还没有任务 YAML？直接把自然语言提示词作为位置参数交给 `run`，Harness Builder 会自动合成任务规格、评测集与指标（p¹ 方差过滤提纯），经检查点确认后跑 baseline 评估：

```bash
export PROMPTOPT_BASE_URL=http://localhost:11434/v1   # 任意 OpenAI 兼容端点
export PROMPTOPT_MODEL=qwen2.5

promptopt run "从中医病历文本中抽取症状、证型与方剂，输出 JSON" --web
```

- 默认全托管：合成 → p¹ 过滤 → 检查点自动放行 → baseline 评估，一气呵成
- 加 `--interactive` 则在合成集检查点暂停：浏览器打开审核页增删改样本后点"批准并继续"，或直接编辑 `synth/<run_id>/checkpoint.json`
- 合成产物落 `synth/<run_id>/`（manifest / spec / samples / filter / checkpoint），评估产物落 `runs/<run_id>/`；看板会给出审核页入口

### 运行评估（配置模式）

以 [examples/json_extraction](examples/json_extraction/)（中医医疗 NER 抽取）为例：

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

产物落在 `runs/<run_id>/`：

```text
runs/<run_id>/
├── manifest.json        # 运行配置快照（复现依据）
├── events.jsonl         # 事件流，SSE 回放源
├── samples/<id>.json    # 逐样本 trace：渲染后 prompt、响应、得分、用量
└── summary.json         # 运行摘要：状态、退出码、指标均值、用量
```

### 浏览历史运行

```bash
promptopt serve    # http://127.0.0.1:17700 查看 run 摘要、样本 trace 与事件回放（只读）
```

## 架构一览

Go 单二进制，标准库优先：

```text
PromptOpt/
├── cmd/promptopt/        # CLI 入口：run / serve / version
├── internal/
│   ├── core/             # Task / Candidate / Dataset / RunResult 核心模型与 YAML 加载
│   ├── config/           # flag 默认值与 PROMPTOPT_* 环境变量解析
│   ├── provider/         # OpenAI 兼容 Provider：重试、usage 统计
│   ├── eval/             # 并行评估引擎：worker 池、exact_match / f1 / json_validator、预算阀门
│   └── web/              # 内嵌 Web 看板：run 列表 / 详情、SSE 实时事件流（go:embed 模板）
├── docs/                 # PRD / ADR / research
└── examples/             # 示例任务
```

按 [ROADMAP.md](ROADMAP.md) 推进中的模块：`harness/`（AI 合成评测集 + p¹ 方差过滤）、`engine/`（GEPA 反射进化：Reflector / Mutator / Frontier / Budget / VistaGuard）、`optimizers/`（多范式可插拔接口）、`store/`（SQLite，纯 Go 驱动）。

## 当前状态

v2 处于 V0 → V2 已落地阶段：Go 骨架、Provider、并行评估引擎、Web 看板与零配置 Harness Builder（合成 → p¹ 过滤 → 检查点 → baseline 评估，含合成集审核页）可用；GEPA 优化引擎按 V3 里程碑推进。

## v1（Python）归档

v1 为 Python 实现，已随 v2 转向停止维护，代码归档于 tag `v0.1-python`：

```bash
git checkout v0.1-python
```

## 贡献

见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## License

MIT
