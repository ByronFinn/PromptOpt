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

评估产物落在 `runs/<run_id>/`：

```text
runs/<run_id>/
├── manifest.json        # 运行配置快照（复现依据）
├── events.jsonl         # 事件流，SSE 回放源
├── samples/<id>.json    # 逐样本 trace：渲染后 prompt、响应、得分、用量
└── summary.json         # 运行摘要：状态、退出码、指标均值、用量
```

### Web 仪表盘

`run --web` 随运行启动实时看板（htmx + SSE，内嵌单二进制，无前端构建链），run 结束后看板驻留——可在前沿看板采纳候选，直至 Ctrl-C；`--addr` 自定义监听地址（默认 `127.0.0.1:17700`，仅与 `--web` 搭配生效）：

```bash
promptopt run "从中医病历文本中抽取症状、证型与方剂，输出 JSON" --web
```

<!-- TODO(V5 后补 GIF)：仪表盘截图 / demo GIF 占位（前沿看板热力表 + 采纳候选流程） -->

页面清单：

| 页面 | 路由 | 能做什么 |
|---|---|---|
| 首页 | `/` | run 卡片列表：状态、退出码、指标均值、用量 |
| run 详情 | `/runs/{id}` | 逐样本 trace（prompt / 响应 / 得分 / 诊断）、**预算仪表**（token ∥ 评估次数实时消耗与余量）、SSE 事件流 |
| 前沿看板 | `/runs/{id}/frontier` | Pareto 候选对比：per-sample 热力表、支配关系标注、Top-1、谱系准入记录、**采纳候选**（原子写 `adopted.json`，幂等，SSE 广播） |
| trace 浏览器 | `/runs/{id}/trace` | 顶层 / `evals/*` / `opt-calls` 单元逐层下钻：样本详情、LLM 调用请求响应全文、与 dataset.json 联查 |
| 候选 diff | `/runs/{id}/diff?a=&b=` | 两候选提示词 side-by-side 行级对比（新增 / 删除统计） |
| run 对比 | `/compare?a=&b=` | 两次 run 的指标均值、分角色用量、Top-1 并排对照（含 Δ 列） |
| 优化报告 | `/runs/{id}/report` | 中文解释性报告站内渲染，下载 `report.md` 附件或自包含 `report.html` |
| 合成集审核 | `/synth/{id}` | 零配置模式检查点：增删改合成样本、批准放行（`--interactive` 时暂停等待） |

`promptopt serve` 用同一套页面浏览历史 run（含 events.jsonl 事件回放与 adopt 产物干预端点，但不暴露实时端点）：

```bash
promptopt serve    # http://127.0.0.1:17700
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
│   ├── harness/          # 零配置合成管线：任务规格/样本合成、p¹ 方差过滤、检查点门
│   ├── engine/           # GEPA 优化引擎：Reflector / Mutator / Pareto 前沿 / VistaGuard / lineage / 报告
│   └── web/              # 内嵌 Web 看板：run 列表/详情、前沿看板、trace 浏览器、diff/对比、报告导出、SSE 实时事件流（go:embed 模板）
├── docs/                 # PRD / ADR / research / reports
└── examples/             # 示例任务
```

按 [ROADMAP.md](ROADMAP.md) 推进中的模块：`optimizers/`（多范式可插拔接口，V5）、`store/`（SQLite，纯 Go 驱动）。

## 当前状态

v2 处于 V0 → V4 已落地阶段：Go 骨架、Provider、并行评估引擎、Web 看板、零配置 Harness Builder（合成 → p¹ 过滤 → 检查点 → baseline 评估，含合成集审核页）、GEPA 优化引擎（反思突变 + Pareto 前沿 + VistaGuard 防护 + lineage/报告）与完整化 Web 干预体验（前沿看板、预算仪表、trace 浏览器、候选 diff、run 对比、报告导出、候选采纳）均可用；下一步按 V5 里程碑扩展多范式优化器。

## v1（Python）归档

v1 为 Python 实现，已随 v2 转向停止维护，代码归档于 tag `v0.1-python`：

```bash
git checkout v0.1-python
```

## 贡献

见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## License

MIT
