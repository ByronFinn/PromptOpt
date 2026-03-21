# PromptOpt Roadmap

> **评估驱动的 Prompt 工程基础设施**

基于用户愿景与项目当前状态，制定以下可执行路线图。

---

## 项目当前状态

| 模块 | 状态 | 说明 |
|------|------|------|
| CLI 框架 | ✅ 完成 | typer + rich，命令桩已搭建 |
| 核心模型 | ✅ 完成 | Task, Dataset, Candidate, Run, EvalResult |
| 存储模型 | ✅ 完成 | SQLAlchemy models |
| 评估器接口 | ✅ 完成 | Evaluator ABC + 3 个实现 |
| 模型适配器 | ✅ 完成 | LiteLLM adapter |
| 优化器基类 | ⚠️ 部分 | Rewrite placeholder |
| 诊断分析器 | ⚠️ 部分 | Stub 实现 |
| **评估引擎** | ✅ 完成 | `EvaluationEngine`、CLI `eval` 与 run/sample 持久化已接通 |
| **数据加载** | ✅ 完成 | `DatasetLoader` 支持 JSON/YAML/CSV 与 dataset config 跳转 |
| **LLM 集成** | ⚠️ 部分 | target adapter 已接入 `eval`，`optimize/search` 等链路尚未接通 |
| **CI/CD** | ❌ 未完成 | - |

---

## M0: 立项校准期 ✅ 已完成

项目定位已明确：
- **一句话定位**: 评估驱动的 Prompt 搜索与回归测试框架
- **目标用户**: 需要迭代优化 Prompt 的开发团队
- **核心价值**: 可评估、可搜索、可回归、可审计、可集成

**验收标准**: 能清楚回答 4 个问题（已达成）

---

## M1: 评估底座可用 🔨 进行中

> 当前状态：P0/P1/P2 issue 主线已落地，剩余工作集中在 sample-level 导出与更强的可复现保障。

### 目标

先把"测得准、存得全"做出来。

### 必须完成的 Issues

#### P0 - 核心阻塞

| # | Issue | 描述 | 验收标准 |
|---|-------|------|----------|
| 1 | **[实现数据集加载器](https://github.com/yourusername/promptopt/issues/1)** | 实现 `DatasetLoader` 支持 JSON/YAML/CSV 格式 | ✅ 已完成 |
| 2 | **[实现 TaskSpec 规范解析器](https://github.com/yourusername/promptopt/issues/2)** | 解析 `task.yaml` 为 `Task` 对象 | ✅ 已完成 |
| 3 | **[实现 Candidate 规范解析器](https://github.com/yourusername/promptopt/issues/3)** | 解析 `candidate.yaml` 为 `Candidate` 对象 | ✅ 已完成 |
| 4 | **[实现评估引擎核心](https://github.com/yourusername/promptopt/issues/4)** | 实现 `EvaluationEngine.run(task, candidate, dataset)` | ✅ 已完成 |
| 5 | **[实现 promptopt eval 命令](https://github.com/yourusername/promptopt/issues/5)** | 将评估引擎接入 CLI | ✅ 已完成 |

#### P1 - 存储完整性

| # | Issue | 描述 | 验收标准 |
|---|-------|------|----------|
| 6 | **[实现 Run 结果持久化](https://github.com/yourusername/promptopt/issues/6)** | 将 `RunResult` 存入 SQLite | ✅ 已完成 |
| 7 | **[实现 sample-level 结果存储](https://github.com/yourusername/promptopt/issues/7)** | 存储每个样本的评估详情 | ✅ 已完成 |

#### P2 - 基础体验

| # | Issue | 描述 | 验收标准 |
|---|-------|------|----------|
| 8 | **[修复 mypy 错误](https://github.com/yourusername/promptopt/issues/8)** | `generate_stream` 返回类型不兼容 | ✅ 已完成 |
| 9 | **[修复 ruff lint](https://github.com/yourusername/promptopt/issues/9)** | Import 排序和类型导入问题 | ✅ 已完成 |
| 10 | **[实现 `promptopt init` 完整逻辑](https://github.com/yourusername/promptopt/issues/10)** | 生成完整项目模板而非空目录 | ✅ 已完成 |

### M1 验收标准

- [x] `promptopt eval` 能对真实任务运行评估
- [ ] 评估结果可复现（二次运行一致或可解释差异）
- [x] `promptopt list-runs` 能查询历史运行记录
- [ ] Sample-level 结果可导出
- [x] 类型检查和 lint 全部通过

### M1 成果标志

> **Prompt benchmark runner** - 可对任务跑评估并存储结果

---

## M2: 失败分析系统可用 ✅ 已完成

> 当前状态：`diagnose`、失败样本导出、基础错误分类、slice metrics 与 baseline diff report 已全部落地，可直接服务后续 prompt 优化决策。

### 目标

从"知道分数"升级到"知道为什么"。

### 必须完成的 Issues

| # | Issue | 描述 | 验收标准 |
|---|-------|------|----------|
| 11 | **[实现错误分类器](https://github.com/yourusername/promptopt/issues/11)** | 将错误归类为：格式错误/语义错误/理解错误/模型能力上限 | 🔨 基础版已完成（format / semantic / execution / unknown） |
| 12 | **[实现 Slice Metrics 计算](https://github.com/yourusername/promptopt/issues/12)** | 按输入特征分组计算指标（如按长度、按领域） | 🔨 基础版已完成（长度 / 数值 / 否定 slice accuracy） |
| 13 | **[实现 `promptopt diagnose`](https://github.com/yourusername/promptopt/issues/13)** | CLI 集成诊断分析 | ✅ 已完成 |
| 14 | **[实现失败样本导出](https://github.com/yourusername/promptopt/issues/14)** | 导出失败样本供人工分析 | ✅ 已完成 |
| 15 | **[实现 baseline diff report](https://github.com/yourusername/promptopt/issues/15)** | 对比两个 candidate 的评估结果 | ✅ 已完成 |

### M2 验收标准

- [x] `promptopt diagnose` 能明确给出失败类别与基础归因
- [x] 能识别出最值得优化的 2~3 个方向
- [x] 失败样本可导出供人工审查

### M2 成果标志

> **Prompt diagnostics tool** - 能解释为什么失败

---

## M3: 候选生成与搜索闭环成立 ✅ 已完成

> 当前状态：`rewrite/fewshot/contract` 候选生成、`optimize/search/select` 主链与基础 lineage 持久化已落地，可批量生成并选择候选。

### 目标

从"分析工具"升级成"优化系统"。

### 必须完成的 Issues

| # | Issue | 描述 | 验收标准 |
|---|-------|------|----------|
| 16 | **[实现 RewriteOptimizer](https://github.com/yourusername/promptopt/issues/16)** | 基于 LLM 重写指令 | ✅ 已完成 |
| 17 | **[实现 FewshotOptimizer](https://github.com/yourusername/promptopt/issues/17)** | 生成 few-shot 示例 | ✅ 已完成基础版 |
| 18 | **[实现 ContractOptimizer](https://github.com/yourusername/promptopt/issues/18)** | 强化 JSON 输出约束 | ✅ 已完成基础版 |
| 19 | **[实现 `promptopt optimize`](https://github.com/yourusername/promptopt/issues/19)** | CLI 集成候选生成 | ✅ 已完成 |
| 20 | **[实现多候选并行评估](https://github.com/yourusername/promptopt/issues/20)** | 批量评估 N 个候选 | ✅ 已完成基础版 |
| 21 | **[实现 `promptopt select`](https://github.com/yourusername/promptopt/issues/21)** | 按指标选择最优候选 | ✅ 已完成第一版 |
| 22 | **[实现候选 lineage 追踪](https://github.com/yourusername/promptopt/issues/22)** | 记录候选的父子关系和 diff | ✅ 已完成基础版 |

### M3 验收标准

- [x] 给定 baseline，能自动生成 12+ 个候选
- [x] 批量评估后能选出满足约束的最优候选
- [x] 输出 candidate lineage 和 diff

### M3 成果标志

> **Eval-driven prompt search framework** - 可生成并比较多个候选

---

## M4: 回归门禁与上线控制 🔨 进行中

> 当前状态：M3 搜索闭环已打通，下一步优先实现 `verify` test gate，并在此基础上补 regression detection / constraints / rollback / prompt diff review。

### 目标

让项目进入"工程可用"状态，而不是实验玩具。

### 必须完成的 Issues

| # | Issue | 描述 | 验收标准 |
|---|-------|------|----------|
| 23 | **[实现 `promptopt verify`](https://github.com/yourusername/promptopt/issues/23)** | 在 held-out test 集上验证 | `promptopt verify <run_id> --split test` |
| 24 | **[实现 regression detection](https://github.com/yourusername/promptopt/issues/24)** | 检测新候选是否降低关键 slice 指标 | 关键 slice 不得退化 |
| 25 | **[实现约束检查](https://github.com/yourusername/promptopt/issues/25)** | JSON validity 100%、成本/延迟预算 | 不满足约束的候选自动拒绝 |
| 26 | **[实现回滚机制](https://github.com/yourusername/promptopt/issues/26)** | 一键回滚到历史 candidate | `promptopt rollback <candidate_id>` |
| 27 | **[实现 prompt diff review](https://github.com/yourusername/promptopt/issues/27)** | 生成 prompt 变更对比报告 | side-by-side diff 输出 |

### M4 验收标准

- [ ] 新 prompt 不得降低关键 slice
- [ ] JSON validity 必须 100%
- [ ] 成本/延迟超预算时告警
- [ ] 可一键回滚

### M4 成果标志

> **Prompt regression engineering system** - 可防止坏 prompt 上线

---

## M5: 集成到开发工作流（Prompt CI）🛠 待开始

### 目标

让它成为团队工程流的一部分。

### 必须完成的 Issues

| # | Issue | 描述 | 验收标准 |
|---|-------|------|----------|
| 28 | **[实现非交互式运行模式](https://github.com/yourusername/promptopt/issues/28)** | CLI 支持无人值守运行 | `--quiet --output-json` 输出 |
| 29 | **[实现 markdown/html 报告生成](https://github.com/yourusername/promptopt/issues/29)** | 生成人类可读的评估报告 | 包含指标、diff、建议 |
| 30 | **[实现 Git diff 集成](https://github.com/yourusername/promptopt/issues/30)** | 对比 prompt 文件变更 | 能读取 git diff 输出 |
| 31 | **[实现 CI 退出码规范](https://github.com/yourusername/promptopt/issues/31)** | 失败时返回明确退出码 | 0=成功, 1=评估失败, 2=回归检测到问题 |
| 32 | **[编写 GitHub Actions 示例](https://github.com/yourusername/promptopt/issues/32)** | 提供 CI 集成模板 | `.github/workflows/promptopt.yml` |

### M5 验收标准

- [ ] 可在 GitHub Actions 中运行
- [ ] PR 上能看到指标变化、slice 变化
- [ ] 失败时清晰告警

### M5 成果标志

> **Prompt CI/CD 基础设施** - 可接入团队开发流程

---

## M6: 插件化与生态初步成立 🛠 待开始

### 目标

让项目不只服务你自己，而能服务别人。

### 必须完成的 Issues

| # | Issue | 描述 | 验收标准 |
|---|-------|------|----------|
| 33 | **[定义 Evaluator 插件接口](https://github.com/yourusername/promptopt/issues/33)** | 第三方可新增 evaluator | 只需实现 `Evaluator` 接口并注册 |
| 34 | **[定义 Optimizer 插件接口](https://github.com/yourusername/promptopt/issues/34)** | 第三方可新增 optimizer | 只需实现 `Optimizer` 接口并注册 |
| 35 | **[定义 Model Provider 插件接口](https://github.com/yourusername/promptopt/issues/35)** | 支持新的模型 provider | adapter 模式支持热插拔 |
| 36 | **[实现 example 模板体系](https://github.com/yourusername/promptopt/issues/36)** | 提供 3+ 个完整 example | JSON extraction, classification, QA |
| 37 | **[编写插件开发文档](https://github.com/yourusername/promptopt/issues/37)** | 教第三方开发者如何扩展 | 文档包含接口说明和示例 |

### M6 验收标准

- [ ] 第三方可新增 evaluator 而不修改核心代码
- [ ] 第三方可新增 optimizer 而不修改核心代码
- [ ] 有 3+ 个可运行的 example

### M6 成果标志

> **开放式 PromptOps 框架** - 可扩展生态

---

## M7: 团队协作与可视化平台 🛠 待开始

### 目标

把命令行工具提升为团队级系统。

> **注意**: 这是第 7 阶段，不是第 1 阶段。先做这个，项目必死。

### 必须完成的 Issues

| # | Issue | 描述 | 验收标准 |
|---|-------|------|----------|
| 38 | **[实现 Web UI 基础框架](https://github.com/yourusername/promptopt/issues/38)** | 查看 runs / lineage / diff | React/Vue 前端 + FastAPI 后端 |
| 39 | **[实现 run 对比视图](https://github.com/yourusername/promptopt/issues/39)** | 并排比较两个 run 的结果 | 表格 + 可视化图表 |
| 40 | **[实现 slice 可视化](https://github.com/yourusername/promptopt/issues/40)** | 按特征分组展示指标 | 支持按长度/领域等分组 |
| 41 | **[实现 Prompt Registry](https://github.com/yourusername/promptopt/issues/41)** | 团队共享 prompt 库 | 版本控制 + 审批流 |
| 42 | **[实现候选审批流](https://github.com/yourusername/promptopt/issues/42)** | 候选上线前的审核 | 状态机：draft → review → approved → deployed |

### M7 验收标准

- [ ] 非技术人员能看懂本次改动值不值得用
- [ ] 能找到历史最佳 prompt
- [ ] 能查看上线前的验证记录

### M7 成果标志

> **团队级 PromptOps 平台** - 团队协作与可视化

---

## 里程碑时间线

```
Month   Milestone
0-1     M1: 评估底座可用 [🔨进行中]
1-2     M2: 失败分析系统可用
2-4     M3: 候选生成与搜索闭环成立
4-6     M4: 回归门禁与上线控制
6-9     M5: 集成到开发工作流（Prompt CI）
9-12    M6: 插件化与生态初步成立
12+     M7: 团队协作与可视化平台
```

---

## 优先级决策说明

### 为什么 M1 内部 P0 > P1 > P2

1. **数据集加载器** (P0#1) - 没有数据，评估跑不起来
2. **TaskSpec/Candidate 解析** (P0#2,3) - CLI 依赖配置解析
3. **评估引擎核心** (P0#4) - 核心中的核心
4. **eval 命令** (P0#5) - CLI 集成，否则无法使用

### 为什么 mypy/ruff 是 P2

- 不影响功能，但影响代码质量
- 后续重构成本会随规模增长

### 为什么 M2 在 M3 之前

没有诊断能力，optimizer 就是瞎猜。
诊断结果才是优化的依据。

---

## Issue 标签规范

| 标签 | 含义 |
|------|------|
| `P0` | 必须完成，否则其他工作无法开展 |
| `P1` | 重要，本里程碑内必须完成 |
| `P2` | 次要，可延后到下一里程碑 |
| `enhancement` | 功能增强 |
| `bug` | bug 修复 |
| `docs` | 文档 |
| `refactor` | 重构 |
| `test` | 测试 |

---

## GitHub Project 看板结构

```
M1: 评估底座可用
├── P0
│   ├── #1 数据集加载器
│   ├── #2 TaskSpec 解析器
│   ├── #3 Candidate 解析器
│   ├── #4 评估引擎核心
│   └── #5 eval 命令
├── P1
│   ├── #6 Run 结果持久化
│   └── #7 sample-level 结果存储
└── P2
    ├── #8 mypy 错误
    ├── #9 ruff lint
    └── #10 init 完整逻辑

M2: 失败分析系统可用
├── #11 错误分类器
├── #12 Slice Metrics
├── #13 diagnose 命令
├── #14 失败样本导出
└── #15 baseline diff report

M3: 候选生成与搜索闭环
├── #16 RewriteOptimizer
├── #17 FewshotOptimizer
├── #18 ContractOptimizer
├── #19 optimize 命令
├── #20 多候选并行评估
├── #21 select 命令
└── #22 lineage 追踪

... (M4-M7 待续)
```

---

## 如何使用本文档

1. **创建 Issue**: 每个 `#N` 都是一个 GitHub Issue
2. **创建 Project**: 用看板视图管理 Milestone
3. **优先级排序**: P0 > P1 > P2
4. **验收标准**: 每个 Issue 的验收标准是 PR merge 的条件
5. **定期回顾**: 每 2 周检查进度，更新状态

---

*本文档根据用户愿景与项目实际状态制定，每 2 周更新一次。*
