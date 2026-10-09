# MCP 单机 API 面（`promptopt mcp`）

> 来源：[roadmap V7 提案](roadmap-v7-proposal.md) §2.4（V8 候选的最小版提前落地）。单机边界声明：**一个 stdio 会话、无鉴权、无多用户**（PRD-0000 Out of Scope 不动摇）——只服务本机一个 agent 驱动本地一份 PromptOpt 安装；多用户服务化不在任何里程碑内。

## 传输与协议

`promptopt mcp [--runs-dir runs]` 在 stdin/stdout 上提供 **newline-delimited JSON-RPC 2.0**：一行一个消息，stdin EOF 或进程信号结束会话。**stdout 是协议通道，除响应帧外零字节**；所有人类可读输出走 stderr（run/verify 子路径的 headless JSON 经 out 注入写 `io.Discard`，载荷改从磁盘工件读回，见下）。

方法面（固化最小三方法 + 一通知，实现于 `internal/mcp/server.go`，纯标准库）：

| 方法 | 语义 |
|---|---|
| `initialize` | 握手；返回钉死的 `protocolVersion: "2024-11-05"`、`capabilities.tools` 与 `serverInfo` |
| `notifications/initialized` | 通知，无响应帧 |
| `tools/list` | 恰返回三个工具（optimize / verify / runs） |
| `tools/call` | `{name, arguments}` 分发；**未知方法 → `-32601`，参数不合法/未知工具 → `-32602`，handler panic → recover 转 `-32603`（会话存活）** |

**取舍记录（提案 §2.4）**：MCP spec 仍在演化，本期按「标准库优先」原则自实现最小面而非引入官方 Go SDK——协议跟随成本由此记录在案；V8 立项时若出现真实 SDK 需求再评估依赖。

## 工具清单（`internal/mcp/tools.go`）

### `optimize` — 同步执行一次 headless run

- **入参**：`task`+`candidate`+`dataset`（三件套模式）**或** `prompt`（零配置模式，二选一互斥）；`base_url`/`model`（缺省回落 `PROMPTOPT_BASE_URL`/`PROMPTOPT_MODEL`）、`api_key`、`out`（缺省 = 服务端 `--runs-dir`）、`budget_evals`/`budget_tokens`（预算上限，0 = 不限）。
- **语义**：复用 CLI 的 run 路径（同一 `parseRunFlags` 校验链与 env 回落），同步阻塞至 run 结束；run id 由工具预铸钉入，载荷从 `<out>/<run_id>/summary.json` 磁盘读回（`finishRun` 落盘的真源——**响应载荷与磁盘工件逐字一致，不做内存二次编码**）。
- **isError 语义**：run 退出码 `0/2`（成功/预算截断）是有效结论、不置 isError；`1`（评估失败）置 isError 但载荷随行；连 summary.json 都没有的失败（参数/加载错误）返回 in-band isError 文本。

### `verify` — 回归门禁验证

- **入参**：`run_id`（必填）、`runs_dir`（可选）。
- **语义**：复用 CLI verify 路径；**连接参数刻意不在工具面上**——verify 按 flag > env > manifest 链从 run 快照复现现场（与 CI 用法同一契约）。返回三态回归结论与配对自助法 CI（`verify.json` 全量载荷，磁盘真源：`<runs>/<run_id>/verify/<ts>/verify.json`）。
- **isError 语义**：退出码 `0/2/3` 都是有效结论（`3` = 回归——载荷本身就是答案，不置 isError）；`1`（评估失败）置 isError 载荷随行。

### `runs` — 列运行摘要卡片

- **入参**：`runs_dir`（可选，缺省 = 服务端 `--runs-dir`）。
- **语义**：扫 runs 目录，每个含 `manifest.json` 或 `summary.json` 的子目录投影一张卡片（run_id、created_at、task/candidate、model、status、exit_code、metric_means），按 run id（内嵌 UTC 时间戳）新到旧排序；两者皆无的目录不是 run，跳过。

## 信号上下文契约

`tools/call optimize` 在常驻 server 进程内反复触发 run 路径的 `signal.NotifyContext`。契约：**每次调用一个 sink、调用必关闭**——

- 正常与早退路径：run 路径自身 `sink.close()`（`finishRun` 内含早退分支）；
- panic 路径：cmd 侧 handler 的 `defer closeOrphanedSinks()` 在 unwind 中兜底回收遗留 sink（其 `stop()` 释放 NotifyContext），随后 server 侧 recover 把 panic 转成 `-32603`；
- 反复调用不积压信号处理器，测试以 `countLiveSinks()==0` 探针断言（`internal/mcp/server_test.go`、`cmd/promptopt/mcp_test.go`）。

## CLI 契约影响（前置重构）

`finishRun`/`finishVerify` 的 headless JSON 输出由硬编码 `os.Stdout` 改为 `out io.Writer` 注入：**5 处既有调用点全部传 `os.Stdout`，CLI 行为字节级不变**（pre/post 双二进制对照 + `TestCLIHeadlessByteContract` 钉「紧凑单行 + 换行」形状）。MCP 是该注入唯一的新消费方（传 `io.Discard`）。

## 快速试用

```bash
go build -o promptopt ./cmd/promptopt
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"runs","arguments":{}}}' \
  | ./promptopt mcp --runs-dir runs   # 响应在 stdout，诊断在 stderr
```
