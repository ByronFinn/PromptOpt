# PromptOpt v2 Roadmap

> **输入一个提示词，输出最优提示词 —— 预算受控、可干预可托管的多范式提示词优化平台**

基于 PRD-0000（[docs/prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md](docs/prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md)，父 Issue #49）制定。本版取代旧 M0–M7 Python 路线（旧 issue 已全部关闭处置，v1 代码归档于 tag `v0.1-python`）。

---

## 产品形态

```text
$ promptopt run "从病历文本中抽取结构化不良反应信息，输出 JSON"

 ① Harness Builder   AI 合成任务规格 + 评测集 + 指标（p¹ 方差过滤提纯）
 ② GEPA 引擎         反思突变 + Pareto 前沿 + VISTA 稳定性防护，预算阀门控制
 ③ 输出              最优提示词 + lineage + 解释性报告

 全程: localhost 仪表盘实时可看（htmx+SSE）· 检查点可干预 · --headless 全托管
```

- **预算双模式**：token 用量上限 ∥ 评估次数上限，可组合；耗尽优雅终止输出当前最优。
- **干预双通道**：Web 检查点（合成集确认 / 候选采纳）+ 本地 artifact 文件直接编辑。
- **开源门面**：页面即产品脸面——V1 起每个里程碑都有可打开的可视化交付，demo GIF 随里程碑更新进 README。

---

## 理论基础（引用已逐一查证，2026-09-28）

| 机制 | 出处 | 在本平台的角色 |
|---|---|---|
| GEPA 反射进化 + Pareto 前沿 | [arXiv:2507.19457](https://arxiv.org/abs/2507.19457)（ICLR 2026 Oral） | V3 引擎核心循环 |
| p¹ 方差过滤 / 最小辨识集 | [arXiv:2604.08801](https://arxiv.org/abs/2604.08801) | V2 合成评测集提纯 |
| VISTA 假设解耦 + 重启 + ε-greedy | [arXiv:2603.18388](https://arxiv.org/abs/2603.18388)（ACL SRW 2026） | V3 稳定性防护（防坏种子退化） |
| ProTeGi 文本梯度 | [arXiv:2305.03495](https://arxiv.org/abs/2305.03495) | V5 范式（定向纠偏） |
| MIPROv2 指令+Few-shot 联合搜索 | [arXiv:2406.11695](https://arxiv.org/abs/2406.11695) / [2407.21787](https://arxiv.org/abs/2407.21787) | V5 范式（复合搜索） |
| TextGrad 计算图反向传播 | [arXiv:2406.07496](https://arxiv.org/abs/2406.07496) | 远期范式（多组件流水线） |
| EvoPrompt / Promptbreeder 进化 | [arXiv:2309.08532](https://arxiv.org/abs/2309.08532) / [2309.16797](https://arxiv.org/abs/2309.16797) | V5 范式（群体搜索） |

> BAPO、TEMPER 两条引用未能核实（arXiv:2305.12524 实为 TheoremQA），已剔除；BO 流以 TextBO [arXiv:2511.12063](https://arxiv.org/abs/2511.12063) 等已验证文献为参考。

**范式路由准则**（远期 V5 产品化）：单模块定向纠偏→ProTeGi；多组件流水线→TextGrad；指令+Few-shot 复合→MIPROv2 风格；生产多维约束→GEPA；预算极紧/测试集大→p¹。

---

## 架构总览

```
promptopt/                      # Go module（同仓库原地重写，单二进制）
├── cmd/promptopt/              # run / serve / resume / inspect
├── internal/
│   ├── core/                   # Task, Candidate, Dataset, RunResult, Trace
│   ├── provider/               # OpenAI-compat 通用 + Anthropic；重试/限流/usage
│   ├── harness/                # 合成任务规格+数据集+指标；p¹ 方差过滤
│   ├── eval/                   # 并行评估（goroutine+errgroup）、trace 捕获、指标
│   ├── engine/                 # Reflector / Mutator / Frontier / Budget / VistaGuard
│   ├── optimizers/             # Optimizer 接口（多范式可插拔，v1 仅 GEPA）
│   ├── store/                  # SQLite + artifact 文件
│   └── web/                    # net/http + htmx 模板 + SSE；go:embed
└── web/                        # 前端模板与静态资源（嵌入源）
```

工程规范：遵守 [JetBrains go-modern-guidelines](https://github.com/JetBrains/go-modern-guidelines)（按 go.mod 版本用现代习语），CI 跑 `go vet` / `staticcheck` / `modernize` / `go test ./...`。标准库优先，SQLite 用纯 Go 驱动（免 cgo）。

---

## 里程碑

### V0：地基 🛠

**目标**：Go 项目骨架立起来，Python 体面归档。

- [ ] Go module + `cmd/promptopt` + `internal/` 布局 + config 加载
- [ ] `Provider` 接口 + OpenAI-compatible 通用实现 + Anthropic 原生实现（重试/限流/usage 统计）
- [ ] SQLite（纯 Go 驱动）+ artifact 文件读写骨架
- [ ] CI：vet / staticcheck / modernize / test 全绿
- [ ] 删除 `src/` `tests/` `pyproject.toml` 等 Python 资产，打 tag `v0.1-python`
- [ ] AGENTS.md 按 Go 工作流重写
- [ ] 开源门面：README 重写（v2 定位、badge、快速开始、架构图）+ CONTRIBUTING.md + LICENSE（MIT）确认

**验收**：`go build ./...` 出单二进制；`go test ./...` 绿；Provider mock 测试覆盖重试与 usage 计量。

### V1：评估闭环 🛠

**目标**：测得准、存得全（继承旧 M1 意图，Go 重实现）。

- [ ] core 模型：Task / Candidate / Dataset / RunResult / Trace（structs + YAML/JSON artifact 序列化）
- [ ] eval 引擎：goroutine+errgroup 并行评估、全量 trace 捕获、指标（exact_match / f1 / json_validator / LLM-judge）
- [ ] 评估器返回 `(score, diagnosis)`——diagnosis 即 ASI，进反思上下文
- [ ] run / sample-level 结果持久化；`promptopt run --dataset <file>` 手工数据集最小闭环
- [ ] **Web 看板骨架页**（前置）：`go:embed` + htmx + SSE，浏览器实时看运行状态与事件流
- [ ] 录制首个 demo GIF 进 README

**验收**：手工数据集跑通评估并落盘；二次运行结果可查；trace 含输入/输出/评分/诊断；打开 localhost 即见实时事件流。

### V2：Harness Builder + Web UI v0 🛠

**目标**：兑现"输入提示词即可"。

- [x] Harness Builder：从提示词合成任务规格 + 评测集 + 指标（默认全托管直跑）
- [x] p¹ 方差过滤：筛掉死样本与高随机性样本，保留高区分度最小辨识集
- [x] 检查点机制：事件总线（SSE 推送）+ 合成集审核页（增删/替换/导入自有数据）
- [x] Web 审核页：在 V1 骨架页基础上补齐合成集审核交互

**验收**：`promptopt run "<自然语言提示词>"` 零配置跑通合成→过滤→确认→进入优化队列。

### V3：GEPA 引擎 🛠

**目标**：反思进化闭环成立（v1 算法核心）。

- [ ] Reflector：读 minibatch 全量 trace（ASI）生成自然语言诊断与假设
- [ ] Mutator：沿假设 + 祖先经验教训突变候选；前沿互补合并
- [ ] Frontier：per-example 分数矩阵 + 非支配排序
- [ ] Budget：token 用量 ∥ 评估次数双阀门，context 传递，耗尽优雅终止
- [ ] VistaGuard：假设生成与重写解耦、语义标注假设并行验证、随机重启、ε-greedy
- [ ] 候选 lineage + 解释性报告（优化轨迹、预算消耗、最终前沿）

**验收**：坏种子场景有防护且留事件记录；预算耗尽输出当前最优而非空手而归；lineage 全程可追溯。

### V4：Web UI 完整化 🛠

**目标**：干预体验成型（吸收旧 M7 的高价值部分，提前到此）。

- [ ] Pareto 前沿看板（候选对比、支配关系、per-example 热力）
- [ ] 预算仪表（token/评估次数实时消耗）
- [ ] trace 浏览器 + 候选 diff（side-by-side）+ run 对比
- [ ] 检查点干预完整化（候选采纳/拒绝、合成集修订）
- [ ] markdown / html 报告导出

**验收**：不看终端即可完成一次"输入提示词→审数据→盯优化→采纳候选"全流程。

### V5：多范式扩展 🛠

**目标**：从单引擎到平台（Optimizer 接口兑现）。

- [ ] Optimizer 插件接口固化（注册、能力声明、路由元数据）
- [ ] ProTeGi 风格（文本梯度定向纠偏）
- [ ] MIPROv2 风格（指令+Few-shot 联合搜索）
- [ ] EvoPrompt 风格（GA/DE 群体搜索）
- [ ] 范式路由器：按任务特征（单模块/复合/多维约束/预算紧张）选型
- [ ] 插件开发文档 + examples 体系（≥3 个完整示例）

**验收**：同一任务可声明使用不同范式并对比结果；第三方可实现新 Optimizer 而不改核心。

### V6：工程化与回归门禁 🛠

**目标**：进 CI、防退化（吸收旧 M4/M5）。

- [ ] `--headless` 无头模式 + JSON 输出 + 退出码规范（0=成功 / 1=评估失败 / 2=预算耗尽 / 3=回归）
- [ ] verify：锚点验证集（用户 3~5 条真实样本，ADR 0001）优先，降级为合成保留集 + 报告标注
- [ ] regression detection + 约束检查（JSON validity 100% / 成本 / 延迟）
- [ ] rollback（tag + artifact 天然支持一键回退）
- [ ] GitHub Actions 集成模板
- [ ] Release 规范：goreleaser 多平台单二进制产物 + changelog + README 的 GIF/截图随版本更新

**验收**：PR 上可自动跑优化对比；新提示词不得在关键指标/约束上退化。

---

## 明确不做（v2 范围外）

多用户/云部署/SaaS、模型微调与 RL 训练类优化（RLPrompt 流派）、多轮对话 agent 与 RAG 全链路优化、MCTS 类搜索（无核实文献）、第三方插件市场、MLflow/Opik 等外部生态集成。

---

## 决策记录（2026-09-28，详见 PRD-0000）

| # | 决策点 | 结论 |
|---|---|---|
| 1 | 评测数据来源 | AI 合成 + 可干预（p¹ 过滤 + 检查点） |
| 2 | 范式范围 | v1 = GEPA+p¹ 核心；接口多范式，实现分期 |
| 3 | 技术栈 | Go 同仓库原地重写；Python 删除 + tag 留档 |
| 4 | 交互形态 | Web UI 优先（单二进制内嵌）+ CLI 无头模式 |
| 5 | 前端 | htmx + 模板 + SSE |
| 6 | Provider | OpenAI-compatible 通用 + Anthropic 原生 |
| 7 | 旧遗留 | 45 issue 批量关闭，能力按 V0–V6 重拆 |
| 8 | 页面与开源（2026-09-28 追加） | V1 前置看板骨架页 + UI 门面化；V0 开源配套 / V6 Release 规范；前端维持 htmx |

---

## 与旧路线的承接关系

| 旧里程碑（Python） | 新去向 |
|---|---|
| M1 评估底座 | V1 |
| M2 失败分析 | V3（反思 trace）+ V4（视图） |
| M3 候选搜索 | V3（GEPA）+ V5（其余范式） |
| M4 回归门禁 | V6 |
| M5 Prompt CI | V6 |
| M6 插件化 | V5 |
| M7 Web 平台 | V2（v0）+ V4（完整化） |

---

*下一步：`/grill` 挑战 PRD-0000 → `/story` 按里程碑拆解 Issue → 实现工作流落地。*
