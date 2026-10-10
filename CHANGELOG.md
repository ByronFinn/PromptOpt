# 更新日志（Changelog）

本项目的所有显著变更记录于本文件。格式遵循 [Keep a Changelog 1.1.0](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

v1（Python）时代的代码归档于 tag `v0.1-python`，不在本日志追溯范围内；本日志自 Go 同仓库重写（v2）起记录。里程碑编号（V0–V6）对应 [ROADMAP.md](ROADMAP.md)。

## [Unreleased]

### Added

#### V7 证据主干、生态面与配置面

> 本节补账 V7 已落地但未及时记账的文档债（此前 `grep -c "V7" CHANGELOG.md = 0`）。

- 裁判隔离与决策级联：`--judge-provider/--judge-base-url/--judge-model/--judge-api-key/--judge-max-tokens`（env `PROMPTOPT_JUDGE_*`，run/verify 同链）为 llm_judge 提供独立裁判 LLM 配置面，未配置逐字段回落执行器；`--judge-backend decision` 把 llm_judge 样本改出 SystemOne 决策服务（score 概率加权 + confidence/低分两级联回落生成式裁判），`--judge-decision-url/--judge-decision-model` 连接面与两阈值 flag，manifest 快照五字段供 verify 复现同一裁判口径。
- 统计显著性三层收编：`--temperature/--reps` 多次采样与预算语义（rep_scores 落 trace）；verify 配对自助法 CI 三态门禁（退出码契约不动）；前沿噪声感知支配 + ε-clone。
- Provider 出站面：`--extra-body` 网关私有 JSON 字段透传、`--rps` 令牌桶限速（与 429/5xx 退避叠加）、`--timeout` 三客户端统一 per-attempt deadline（显式值入 manifest 供 verify 复现）。
- 评估指标注册表：确定性指标 `RegisterMetric` 一行接入（`llm_judge` 保留名禁注册）+ 内置中医分维度指标包 `tcm_f1_syndrome/treatment/formula/herb` 与宏平均 `tcm_f1_entity`；`--optimizer auto` 紧预算路由 p¹ 与 TightBudget 语义修正。
- `anchor` 子命令与 `--task-key`：锚点库 `anchors/<task-key>/` confirmed/staging 双层、input_hash 去重、promote 人工确认（ADR 0001 治理机制化）；verify `--anchor-lib` 加载正式库、`--promote` 逐样本 verdict 回流 staging。
- 跨 run 候选池与退化预警：`pool/<task-key>/pool.jsonl` 追加制，同集（ID + input_hash 双一致）配对自助法对照，下界超阈值 stderr 中文预警 + manifest `pool_warning`。
- `mcp` 子命令：MCP 单机 API 面——stdio JSON-RPC 三工具 optimize/verify/runs（协议取舍见 docs/mcp.md）。
- Web 看板四视图 1:1 重构（`/runs/{id}/dashboard` + 五数据端点）、`--addr`/`--port` 分立参数（显式任一即隐含起看板）、run 结束中文结论块、SSE 实时事件流。
- **配置面（[PRD-0001](docs/prd/PRD-0001-config-guidance.md)）**：YAML 配置文件层——`--config`/`PROMPTOPT_CONFIG` 四层发现序、22 环境身份键、单一解析链（run/mcp：flag > env > 文件 > 默认；verify 连接身份白名单补位、行为键不补位）、10 键显式性标记（显式 flag 含显式 0 终判压过文件）；`promptopt config` 子命令——init 九问向导（非 tty 行协议、stdin `-` 密钥前置读取、非 tty 无 `--force` 拒绝覆盖）/list 逐键来源与遮蔽可见性（人类 stderr、`--headless` JSON stdout）/get/set（仅单键校验、`-` 整读 stdin）/unset（幂等）；写侧安全闸（0600/0700、api_key 恒掩码、project 作用域幂等追加 .gitignore、宽松权限位运算告警）；run/verify 缺 base_url/model 报错升级为三/四途径指路、decision 缺参加 config 键指引（退出码 1）；`--spec-metrics` 钉死零配置合成规格的指标（仅零配置模式合法、逐项注册指标校验、显式空 = 交还 LLM 自选；config 键 `spec_metrics` 同链生效、覆盖先于样本合成、manifest 快照）；只读设置引导页 `GET /settings`（run --web 发布合并快照优先 / serve 回落「当前环境」链，逐键来源与 `config list` 同一收集器口径、api_key 恒掩码、缺参三途径引导，无配置写端点）。

#### V6 工程化与回归门禁

- `promptopt verify`：交付候选的最终验证命令。锚点验证集优先（`--anchor`，≥3 条真实样本，仅用于最终验证、永不进入优化循环），未提供锚点时按原 run spec 独立重合成同契约保留集对照，报告强制标注「结论未经真实数据验证」（ADR 0001）。
- 回归与约束判定：主指标均值退化阈值 `--max-regression`（默认 0.05）、`json_validator` 任务交付侧 JSON 合法率 100% 硬检查、`--max-avg-tokens` / `--max-avg-latency-ms` 成本与延迟上限（口径含 llm_judge 裁判开销）；违反以退出码 `3` 表达。
- `promptopt rollback`：一键回退 `adopted.json` 采纳——`--to` 指定目标、缺省回退上一不同采纳、兜底 baseline（旧工件缺 Prompt 时回退 synth spec 的 prompt_template），采纳历史追加 `adopted-history.jsonl`，`--emit` 导出 candidate.yaml。
- `promptopt replay`：按时间排序合并 events.jsonl 与各阶段 LLM 调用留痕（合成 / 评估 / 优化 / 裁判），输出完整调用与决策审计时间线及覆盖率摘要；`--headless` 输出 JSONL。
- 退出码规范：`0` 成功 / `1` 评估失败或用法错误 / `2` 预算耗尽 / `3` verify 回归或约束违反（优先级 `2 > 1 > 3`）。
- `llm_judge` 指标：经 Provider 按内置中文 rubric 打分（0~1，输出夹取边界），中文诊断写入逐样本 trace；裁判调用的 token 用量与延迟并入样本事件（`Event.Usage` / `SampleTrace.Usage` 口径改为该样本总评估开销，judge 延迟以 `judge_ms` 字段单列，`duration_ms` 保持仅计 executor 主调用）。
- GitHub Actions PR 优化对比模板 [`.github/workflows/optimize.yml`](.github/workflows/optimize.yml)：PR 上自动跑零配置优化 → `verify` 同契约合成保留集门禁（退出码 3 = PR 检查失败），缺 secrets 自动跳过，上传 run 产物并在 PR 摘要页渲染指标对照表。
- CI 新增 staticcheck 门禁（`go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...`），vet / staticcheck / modernize / test / build 五道门禁齐备（[`.github/workflows/ci.yml`](.github/workflows/ci.yml)）。
- Release 流水线：[`.github/workflows/release.yml`](.github/workflows/release.yml)（推送 `v*` tag 触发，goreleaser 钉 v2.13.0）+ [`.goreleaser.yaml`](.goreleaser.yaml)（darwin/linux × amd64/arm64 纯 Go 单二进制 tar.gz + checksums）。
- [`examples/json_extraction/anchor.yaml`](examples/json_extraction/anchor.yaml)：3 条人工编写样本的 `--anchor` 锚点验证集用法示例。
- MIT `LICENSE` 落地。

### Fixed

- reflector 空假设池视为模型的有意义回答（PRD-0001 D9①）：空数组与全 `{input}` 字面量清洗池两形态同规——恰 1 次反思调用、0 次修复调用，不再以「响应中没有可用假设」触发修复空转；gepa 侧空池落既有跳轮分支（`vista.Record(false)` + `round_done` skipped 事件「没有可验证的假设」），全程空池以 `rounds_done` 收尾交付 baseline；修复通道从「parse+语义」缩为 parse-only（垃圾 JSON 仍恰好一次修复）；预算/取消语义零改动。

### Changed

- 声明 `llm_judge` 指标的任务每次评估追加一次裁判调用（计入 executor 预算、参与软停）；仪表盘逐样本 token 显示数值会相应略增——口径修正，非计费错误。

#### V5 多范式扩展

- Optimizer 插件接口固化：注册表、能力声明、路由元数据；范式路由器按任务特征（单模块纠偏 / 复合搜索 / 多维约束 / 预算紧张）选型，`--optimizer` 显式指定或 `auto` 路由。
- 新范式实现：ProTeGi 风格（文本梯度定向纠偏）、MIPROv2 风格（指令 + Few-shot 联合搜索）、EvoPrompt 风格（`--evo-variant ga|de` 群体搜索）。
- Anthropic Provider 实现落于 `internal/provider`（CLI `--provider anthropic` 的放行待后续合入）。
- 多范式示例任务扩充（见 `examples/`）。

#### V4 Web 干预体验

- Pareto 前沿看板：候选对比、per-sample 热力表、支配关系标注、Top-1、谱系准入记录、采纳候选（原子写 `adopted.json`，幂等，SSE 广播）。
- 预算仪表（token ∥ 评估次数实时消耗与余量）、trace 浏览器（顶层 / evals / opt-calls 逐层下钻）、候选 diff（side-by-side 行级对比）、run 对比（含 Δ 列）、markdown / 自包含 HTML 报告导出。
- `run --web` 看板在 run 结束后驻留直至采纳完成（Ctrl-C 退出）；`serve` 用同一套页面浏览历史 run（含事件回放与 adopt 产物干预端点，写端点带来源防护中间件）。

#### V3 GEPA 优化引擎

- 反思-突变-前沿闭环：Reflector 读 minibatch 全量 trace（ASI）生成自然语言假设，Mutator 沿假设与祖先经验突变候选（rewrite / merge / restart），Frontier 按 per-example 分数向量做 Pareto 非支配排序准入。
- 预算双阀门：评估侧 token / 评估次数上限（`--budget-tokens` / `--budget-evals`）+ 优化侧独立阀门（`--budget-opt-tokens`）；耗尽软停、当前 minibatch 跑完再优雅退出并交付当前最优（退出码 `2`）。
- VistaGuard 稳定性防护：假设生成与重写解耦、语义标注假设并行验证、停滞随机重启（`vista_restart` 事件）、ε-greedy 假设选择。
- 候选 lineage 全程可追溯 + 中文解释性报告（轮次、分角色预算消耗、前沿成员表、取舍说明）。

#### V2 零配置合成管线

- `promptopt run "<自然语言提示词>"` 零配置模式：AI 合成任务规格 + 评测集 + 指标，p¹ 方差过滤提纯（筛掉死样本与高随机性样本），检查点默认自动放行（`--interactive` 暂停等待审核），随后 baseline 评估并进入优化循环。
- 合成集审核页：检查点处增删改合成样本、导入自有数据、批准放行；产物落 `synth/<run_id>/`。

#### V1 评估闭环

- core 模型与 YAML/JSON 工件：Task / Candidate / Dataset / RunResult / SampleTrace 及其校验加载。
- 并行评估引擎：worker 池、逐样本 trace 捕获（渲染后 prompt、响应、得分、诊断、用量）、指标 `exact_match` / `f1` / `json_validator`。
- 输出契约：`--headless` stdout 仅输出 JSON 运行摘要；退出码 `0` 成功 / `1` 评估失败或用法错误 / `2` 预算耗尽。
- Web 看板骨架页（go:embed + htmx + SSE，实时事件流）。

#### V0 地基

- Go 同仓库原地重写：module 布局（`cmd/promptopt` + `internal/`）、config（flag 默认值与 `PROMPTOPT_*` 环境变量回退）、Provider 接口 + OpenAI 兼容实现（重试、usage 统计）。
- CI（vet / modernize / test / build）与开发约定（AGENTS.md 按 Go 工作流重写）；Python v1 资产删除并归档于 tag `v0.1-python`。
