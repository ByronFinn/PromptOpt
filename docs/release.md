# 发布与门禁 Runbook（V6）

本文是 PromptOpt 的发布工程与 PR 门禁 runbook：CI 门禁组成、PR 优化对比模板的语义与 secrets 约定、`verify` 的两种验证模式与退出码、以及 Release 流水线的钉版原因。里程碑背景见 [ROADMAP.md](../ROADMAP.md) V6，锚点策略的决策记录见 [ADR 0001](adr/0001-synthetic-eval-and-real-anchor.md)。

## 1. CI 门禁（五道）

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) 在 push/PR 到 main 时执行，全绿才可合入：

| 步骤 | 命令 | 说明 |
|---|---|---|
| Vet | `go vet ./...` | 标准静态检查 |
| Staticcheck | `go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...` | 版本钉死保证告警集稳定；`go run` 形式无 `$GOPATH/bin` PATH 假设 |
| Modernize | `modernize -test ./...`（@latest） | go-modern-guidelines 现代习语检查 |
| Test | `go test -race ./...` | 全量测试含竞态检测 |
| Build | `go build ./...` | 单二进制可构建 |

升级 staticcheck 版本时新告警须当场清零后再改版本号；modernize 维持 @latest 现状。

## 2. PR 优化对比（optimize.yml）

[`.github/workflows/optimize.yml`](../.github/workflows/optimize.yml) 在 `examples/**` 或工作流文件本身变更的 PR 上自动运行（也可手动 `workflow_dispatch`），流程为：构建 CLI → 零配置优化（`run --headless`）→ `verify` 门禁 → 上传产物 + PR 摘要页指标对照表。**验收语义：新提示词不得在关键指标/约束上退化。**

### 2.1 secrets 约定

模板需要三个仓库级 secrets（Job 通过同名环境变量消费）：

| secret | 环境变量 | 含义 | 缺省行为 |
|---|---|---|---|
| `PROMPTOPT_BASE_URL` | `PROMPTOPT_BASE_URL` | OpenAI 兼容 API base URL | 缺任一 secret 时整个 job 跳过（notice 提示，不算失败、不烧 API 费） |
| `PROMPTOPT_MODEL` | `PROMPTOPT_MODEL` | 模型名 | 同上 |
| `PROMPTOPT_API_KEY` | `PROMPTOPT_API_KEY` | API key（本地网关可设 `1`） | 同上 |

secrets 不能直接用于 `if` 条件，模板先落到 job env 再由 Guard 步骤间接判断。连接参数不通过 flag 传给 `verify`：`verify` 按 **flag > 环境变量（`PROMPTOPT_*`）> run manifest** 回退，job 级 env 自动生效。

成本护栏：默认 paths 过滤（仅 `examples/**`）、缺 secrets 自动跳过、预算钉死（`--budget-tokens` / `--budget-opt-tokens`）、同 PR 并发取消、30 分钟超时。

### 2.2 run 步骤的退出码矩阵

| run 退出码 | 含义 | 模板行为 |
|---|---|---|
| `0` | 正常完成 | 继续 verify |
| `2` | 预算耗尽（优雅终止，交付当前最优） | 继续 verify（`--headless` 下 JSON 摘要仍打印到 stdout，`run_id` 可取） |
| `1`（或其他） | 评估失败 / 用法错误 | 立即失败 job |

**边界**：预算饿死 baseline 的 run 不产 `frontier.json`（run 在 baseline 未完成时跳过优化循环），`verify` 会因找不到前沿以明确错误退出 1。此时按报错调大预算——预算须覆盖 **baseline 评估 + 至少一轮优化**（合成与探针也计入评估侧预算）。

## 3. verify 的两种验证模式

`promptopt verify <run_id>` 对交付候选（`adopted.json` 人工采纳优先，否则前沿 Top-1）与 baseline（前沿 baseline 成员，兜底 synth spec 的 `prompt_template`）做同集对照评估，两种验证集模式：

### 3.1 锚点模式（`--anchor <dataset.yaml>`）

用户提供 **3~5 条真实样本** 作为锚点验证集（<3 条为用法错误；>5 条警告统计效力有限）。锚点仅用于最终验证，**永不进入优化循环**，保持对合成分布的独立性（ADR 0001）——这是唯一有真实数据背书的模式。

示例：[examples/json_extraction/anchor.yaml](../examples/json_extraction/anchor.yaml)（3 条人工编写样本，`split: test`，可直接 `core.LoadDataset` 加载）：

```bash
promptopt verify <run_id> --runs-dir runs \
  --anchor examples/json_extraction/anchor.yaml
```

**适用条件**：任务契约固定/自控的场景——手写三件套的任务，或零配置 run 在检查点固化了 spec。零配置 run 的任务契约（`prompt_template` / `metrics` / 输出 schema）由 LLM 每次 run 现场决定（`internal/harness/synthesize.go` 的 spec 合成提示词），静态锚点的 `expected` 与之脱钩，主指标 delta 是噪声。

### 3.2 合成保留集模式（缺省降级路径）

不带 `--anchor` 时，`verify` 重读原 run 的 spec，按同任务契约独立重合成保留集（复用 harness 合成器，无 p¹ 探针、无过滤；合成用量单独记账，不进任何一侧预算）。这是 **GA（optimize.yml）的默认门禁**：同 spec 保留集是同契约对照，delta 噪声小于静态脱锚但非零。

代价与边界（ADR 0001 已声明）：同族偏差不设防——保留集与优化集同出 LLM 合成分布，该门禁是「同契约合成对照级」的不退化证明，**不是真实数据背书**。因此报告与 PR 摘要强制标注「结论未经真实数据验证」（`verify.json` 的 `caveat` 字段）；真实数据背书需本地提供 `--anchor`。`json_validator` 合法率 ==100% 的硬检查不受契约脱钩影响，仍是硬门禁。

## 4. verify 退出码表

| 退出码 | 含义 | PR 门禁语义 |
|---|---|---|
| `0` | 通过：无回归、无约束违反 | 检查通过 |
| `1` | 用法错误或评估失败（工件缺失、候选评估出错） | 失败 |
| `2` | 任一侧有因预算未派发的样本——**证据不完整，回归结论不可得** | 失败（可加预算重跑） |
| `3` | 回归或约束违反：主指标均值退化超 `--max-regression`（默认 0.05）、`json_validator` 任务交付侧合法率 <100%、`--max-avg-tokens` / `--max-avg-latency-ms` 超限 | 失败（这就是「新提示词退化」） |

优先级 `2 > 1 > 3`（与全局退出码规范一致）。

### 4.1 预算与评估顺序语义

`verify` 的 `--budget-tokens` / `--budget-evals` 是**逐侧上限**（baseline 与 delivered 各自一份独立预算），不是共享池——同时消除评估顺序效应与双侧用量互污。评估顺序固定**先 baseline 后 delivered**。`--max-avg-tokens` / `--max-avg-latency-ms` 的口径为交付侧逐样本事件累加（含 llm_judge 裁判开销与延迟）。

工件落 `runs/<id>/verify/<UTC 时间戳>/`（`verify.json` / `verify.md` / 双侧 trace / 锚点数据集或保留集目录）；`--headless` 时 JSON 报告打到 stdout。

## 5. Release 流程

1. 合入 main（CI 五道门禁全绿）。
2. 更新 [CHANGELOG.md](../CHANGELOG.md)（人工维护为权威，Keep a Changelog 格式：新条目先入 `Unreleased`，发版时落版本段）。
3. 打 tag 并推送：`git tag vX.Y.Z && git push origin vX.Y.Z`。
4. [`.github/workflows/release.yml`](../.github/workflows/release.yml) 自动触发 goreleaser，产物为 darwin/linux × amd64/arm64 纯 Go（`CGO_ENABLED=0`）单二进制 tar.gz + `checksums.txt`；`ldflags` 注入 `main.version` / `main.commit`（`promptopt version` 可见）。

### 5.1 goreleaser 钉 v2.13.0 的原因与解钉条件

goreleaser `@latest` 要求 go ≥ 1.27.1，与 go.mod 的 Go 1.26 / CI 工具链不兼容（实测 `go install` 失败于工具链约束）；v2.13.0 在 go1.26.5 下安装并 `goreleaser check` 通过。**解钉条件**：go.mod 升至 1.27+ 后重评钉版（改 `.github/workflows/release.yml` 的 `version:` 与 `.goreleaser.yaml` 注释即可，配置本身无版本耦合键）。

本地校验 goreleaser 配置：`goreleaser check`（沙盒无 `.git` 时需先 `git init` + `git remote add origin <repo>`，check 依赖仓库与 remote 存在）。changelog 由 goreleaser 禁用（`changelog.disable: true`），Release 正文引用仓库根的 CHANGELOG.md，不自动生成 git log。

## 6. README badge 与许可

`LICENSE` 为 MIT（版权人 ByronFinn），README 的 License badge 指向该文件。
