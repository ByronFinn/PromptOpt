# PromptOpt 插件（范式）开发指南

> V5 状态：本文档描述的骨架与三个范式子包（`protegi` / `miprov2` / `evoprompt`）均已合入并注册。
> `--optimizer` 的可选值以 `--optimizer bogus` 的报错清单为准，当前为
> `auto, gepa, protegi, miprov2, evoprompt`；auto 默认路由（请求 protegi）
> 不再回退，`textgrad` 降级链同样落 protegi。

PromptOpt 的优化器范式以"插件"形态接入：一个范式 = 一个实现 `engine.Optimizer` 的子包 +
在 `internal/optimizers/builtin` 注册一行。注册后 `--optimizer <name>` 立即生效，
compare 看板的「范式」行经注册表解析标签，**不改 engine / eval / cmd / web 任何核心代码**。

## 1. 接口与共享助手

范式实现 `internal/engine` 的 Optimizer 接口（ADR-0002：接口留在 engine）：

```go
type Optimizer interface {
    Optimize(ctx context.Context, req Request) (Result, error)
}
```

`Request` 携带任务、参数、保留样本集、baseline 逐样本记录、Provider、共享预算与事件流；
范式专属开关经 `Opts map[string]string`（键约定 `"<paradigm>.<key>"`）注入，
用 `req.Opt("evoprompt.variant")` 读取。`Result` 的 `Best` **永不为空**——见下文 Loop 契约。

共享助手（`internal/engine`，全部公开，GEPA 自身即运行在这套脚手架上）：

| 助手 | 作用 |
|---|---|
| `NewLoop(req) (*Loop, error)` | 校验请求 + 播种 baseline（frontier/lineage 各一行），Best 永不为空的保证来源 |
| `Loop.Advisor() *Advisor` | 优化侧 LLM 调用管道（重试爬坡、opt-calls 落盘、optimizer 角色计量、token 阀门） |
| `Loop.Evaluate(ctx, cand, samples, round)` | 在 `evals/<seq>-<id>/` 起一个评估单元：事件转发（带 candidate/round 戳）+ 记录收割 |
| `Loop.Admit(child, op, parents, hyps, round, res, records)` | 行投影 + frontier 准入 + `frontier_updated` + lineage 追加（返回是否准入） |
| `Loop.MarkIncomplete(...)` | 预算中断子代：partial 行入 lineage（`Incomplete`），不入前沿 |
| `Loop.Finish(reason, rounds)` | 组装 Result + 写 report/frontier 产物 |
| `Loop.Emit / Stopped / NotifyValve / Record / BatchRecords` | 事件发布、预算阀门探测与通知、记录存取 |
| `NewAdvisor(req)`（或经 Loop 获取） | Advisor 直接构造 |
| `Advisor.Call(ctx, stage, prompt)` | 单次优化侧调用（ErrOptBudget 哨兵错误表示阀门触发） |
| `Defend[T](ctx, adv, stage, repairMarker, raw, check)` | 三级 JSON 防御：本地提取 → 一次 LLM 修复 → 带原文摘录报错 |
| `ProduceCandidate(ctx, adv, stage, prompt)` | 候选产出脚手架：Call + Defend + `CandidateQualityGate`（非空且含 `{input}`）一次修复 |
| `CleanInputLiteral / TruncateRunes` | 文本清洗工具 |
| `Reflector / Mutator / VistaGuard / Frontier / Lineage` | GEPA 同款反思、突变、ε-greedy 选择、帕累托前沿、谱系 |

范式主循环的推荐骨架（GEPA 参照 `internal/engine/gepa.go`）：

```text
loop, err := engine.NewLoop(req)
for 轮次 {
    if ctx.Err() != nil            → loop.Finish(engine.ReasonAborted, rounds)
    if loop.Stopped()              → loop.NotifyValve(); loop.Finish(engine.ReasonBudgetStopped, rounds)
    loop.Emit("round_start", Detail{round, paradigm: "xxx", stage: "..."})
    范式决策（Advisor.Call / ProduceCandidate / 自有提示词）
    res, records, err := loop.Evaluate(ctx, child, req.Samples, round)
    if res.Undispatched > 0        → loop.MarkIncomplete(...); Finish(budget_stopped)
    admitted, err := loop.Admit(child, op, parents, hyps, round, res, records)
}
loop.Finish(reason, rounds)
```

## 2. 注册表与能力声明

`internal/optimizers`（包根，**不 import 任何范式子包**，防环）：

```go
type Factory func() engine.Optimizer
type Capabilities struct {
    Name, Label, Summary, Paper string
    SuitsPipeline, SuitsDirectional, SuitsJointFewShot bool
    SuitsMultiConstraint, SuitsTightBudget bool
}
type Descriptor struct { Capabilities; Factory Factory }
```

`Registry`：`Register`（重名 / 空 name / nil factory 直接 panic——启动期 fail-fast）、
`Get(name)`、`Names()`（排序）、`Build(name)`（未知名报错并列出可选，兜底防线）。

`Capabilities` 字段含义：`Name` 是 `--optimizer` 取值；`Label` 是看板显示名（中文）；
`Summary`/`Paper` 是目录信息；`Suits*` 是面向 auto 路由的能力**目录**（文档性质），
路由决策本身是下面的固定规则表，不读 Suits 字段。

## 3. auto 路由规则

`--optimizer auto` 时，cmd 在合成管线产出保留集后调用
`optimizers.Route(builtin.Registry(), routeFeatures(task, kept, budgetEvals))`。
派生规则（写死于 `cmd/promptopt/run.go` 的 `routeFeatures`，单元可测）：

| 特征 | 派生来源 | V6 升级路径 |
|---|---|---|
| `Pipeline` | 恒 `false`（V5 无任务级流水线声明来源） | task.yaml 结构化声明 |
| `JointFewShot` | 保留集 `split=="train"` ≥2 且主指标 ∈ {exact_match, f1, json_validator} | 声明 few-shot 配置 |
| `TightBudget` | `--budget-evals > 0 且 < 2×len(kept)`（设了预算 ≠ 极紧） | 声明预算画像 |
| `MultiConstraint` | `len(Task.Metrics) > 1` | 无需升级 |

规则按优先级排序——**付不起的范式优先于数据形态，数据形态优先于多指标权衡**：

1. `Pipeline` → 请求 `textgrad`（降级到 `protegi`）
2. `TightBudget` → 请求 `p1`（降级到 `gepa`）
3. `JointFewShot` → `miprov2`
4. `MultiConstraint` → `gepa`
5. 默认 → `protegi`

降级链 `degradeTo{textgrad→protegi, p1→gepa}` 表示"机制等价实现"；
`Decision{Paradigm, Requested, Reason, Degraded}` 全部落入 manifest
（`optimizer` / `optimizer_requested` / `optimizer_route_reason`）。
**未注册范式再降级到 gepa**（auto 永不因注册状态失败，原因记录在 Reason）；显式
`--optimizer <name>` 则在 flag 解析期就校验注册名，非法名列出可选并退出 1。
已知边界：一个走完合成的零配置 run，探针 + baseline 至少消耗 `2×samples + kept`
次评估，恒 ≥ `2×kept`，因此 `TightBudget` 只会在预算耗尽（跳过优化循环）的 run 上
于 manifest 中留下 p1→gepa 的路由记录。

## 4. 第三方（in-repo）接入步骤

1. 建子包 `internal/optimizers/<name>/`，实现 `New() engine.Optimizer`（仅 import
   `engine/core/eval/provider/config` + 标准库，**不 import optimizers 根**，防环）。
2. 在 `internal/optimizers/builtin/builtin.go` 加一行 `reg.Register(...)`。
3. 完成——`--optimizer <name>`、auto 路由、compare 范式行、`Build` 校验全部生效。

仓库外（外部 module）受 Go `internal/` 边界限制无法导入这些包；V5 的"插件面"是
in-repo 的，公共 API 属远期规划（诚实声明）。

## 5. 测试规范

- **LLM 全部 mock**：包级测试用 httptest 假 server 按提示词 marker 路由
  （参照 `internal/engine/gepa_test.go` 的 `startGoldenLLM`）；cmd e2e 用
  `startZeroConfigLLM` / `startScriptedZeroConfigLLM`（`cmd/promptopt/run_test.go`）。
- 优化侧调用断言：`opt-calls/` 落盘数量 ≥ 预期调用数。
- 必测边界：`kept=1`（单样本退化，无除零/空批）、预算停
  （`reason=budget_stopped` 且 `Best` 非空）、score-clone 拒绝。
- `--race` 下事件会从 worker goroutine 到达：消费方必须加锁
  （`eventLog`、`RecordCollector` 先例）。

## 6. 事件契约

- 必发：每轮 `round_start` / `round_done`（终端进度依赖它们）；准入发
  `frontier_updated`。
- 范式专属事件**允许且鼓励**（如 `gradient_done` / `propose_done` / `joint_done` /
  `population_updated`）：带 `Detail.paradigm` 与 `Detail.stage`，live 看板的
  default 分支会渲染成 `[paradigm] type：stage` 一行；终端 stderr 不打印
  （进度打印器只认 `round_*`）。
- 引擎层永不发 `run_done`；内层评估单元的 `run_start/run_done` 由 `Loop.forward`
  过滤，勿绕过 `Loop.Evaluate` 自行起评估。

## 7. 预算与产物契约

- 分角色预算：executor 用量武装软停；optimizer 用量只喂 token 阀门
  （`OptBudgetTokens`，合成也计入）。`Advisor` 的调用全部自动计量，勿绕过。
- Best 永不为空：`NewLoop` 播种 baseline 行，任何终止路径（含预算停、abort）都
  交付当前最优；预算中断的半评估子代只入 lineage（`Incomplete`），不入前沿。
- 产物：`lineage.json`（增量原子写）、`frontier.json`、`report.md` 由
  `Loop.Finish` 统一落盘；`evals/<seq>-<id>/` 由 `Loop.Evaluate` 落盘。
