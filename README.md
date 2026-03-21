# PromptOpt

**一个面向结构化 LLM 任务的、评估驱动（eval-driven）的 Prompt 搜索与回归测试框架。**

---

## 项目简介

PromptOpt 用于解决一个实际存在的问题：

> **Prompt 调优目前仍然是“手工试错”，不可复现、不可对比、不可回滚。**

典型流程是：

```text
写 prompt → 跑评估 → 看错误 → 让更强模型帮改 → 再试
```

这个流程的问题：

* ❌ 没有版本管理（不知道哪个 prompt 更好）
* ❌ 不可复现（每次结果都漂）
* ❌ 只看总分，不知道错在哪
* ❌ 很容易“修 A 坏 B”
* ❌ 无法做回归测试
* ❌ 无法比较不同优化策略

---

## PromptOpt 做了什么

PromptOpt 将 prompt 调优变成一个**工程化系统**：

```text
baseline prompt
    ↓
评估（eval）
    ↓
失败分析（diagnose）
    ↓
生成多个候选 prompt（search）
    ↓
批量评估
    ↓
选择（selection）
    ↓
测试集验证（regression gate）
```

核心变化：

> ❌ 单路径试错
> ✅ 多候选搜索 + 评估驱动选择

---

## 核心能力

### 1️⃣ 评估驱动（Eval-first）

* 支持多种指标：

  * exact match
  * F1 / macro-F1
  * JSON 合法性
* 支持自定义 evaluator
* 支持 LLM-as-judge（可选）

---

### 2️⃣ 失败分析（Diagnostics）

不仅告诉你“差”，还告诉你：

> **差在哪**

支持：

* 样本级错误分析
* slice 分析（例如：

  * 否定句
  * 家族史
  * 数值型信息
    ）
* 失败模式归因

---

### 3️⃣ 多候选搜索（Search）

不是只改一个 prompt，而是生成多个候选：

* instruction rewrite
* few-shot 替换/增强
* 输出约束强化（JSON contract）

然后：

👉 **批量评估 → 选择最优**

---

### 4️⃣ Prompt 版本与血缘（Lineage）

每个 prompt 都是一个版本化对象：

* 父子关系（谁改出来的）
* 修改方式（rewrite / few-shot 等）
* 性能变化
* prompt diff

---

### 5️⃣ 回归测试（Regression Gate）

防止“越优化越坏”：

* dev / test 分离
* 必须通过 test 才能选用
* 支持约束：

  * JSON 必须合法
  * 成本限制
  * 延迟限制

---

### 6️⃣ 支持本地模型（重点）

真实使用场景：

* 用 GPT / DeepSeek 作为 **优化器（teacher）**
* 用本地模型（Qwen / LLaMA）作为 **执行器（target）**

支持：

* OpenAI-compatible API
* vLLM
* Ollama

---

## 适用场景

PromptOpt **不是万能工具**，最适合：

### ✅ 结构化任务

* JSON 抽取
* 医疗信息抽取
* 表单解析

### ✅ 分类任务

* 标签分类
* 意图识别

### ✅ 强约束生成

* schema 约束输出
* 固定格式生成

---

## 不适合的场景

* ❌ 多轮对话 agent
* ❌ RAG 全链路优化
* ❌ 自主 AI 系统
* ❌ 模型训练 / 微调

---

## 快速开始

### 1. 初始化项目

```bash
promptopt init examples/json_extraction
```

---

### 2. 跑 baseline

```bash
promptopt eval \
  --task tasks/task.yaml \
  --candidate candidates/baseline.yaml \
  --dataset datasets/dataset.yaml \
  --split dev
```

---

### 3. 失败分析

```bash
promptopt diagnose runs/run_001
```

---

### 4. 生成候选 prompt

```bash
promptopt optimize runs/run_001 \
  --teacher openai/gpt-5 \
  --strategies rewrite \
  --num-candidates 12
```

---

### 5. 批量评估

```bash
promptopt search candidates/ \
  --task tasks/task.yaml \
  --dataset datasets/dataset.yaml \
  --split dev
```

---

### 6. 选择最优

```bash
promptopt select runs/run_002 \
  --primary macro_f1 \
  --constraints json_validity=1.0 \
  --secondary cost,latency
```

---

### 7. 测试集验证

```bash
promptopt verify runs/run_002 --split test
```

---

## 项目结构

```text
promptopt/
├─ core/          # task / candidate / run / lineage
├─ evaluators/    # 评估指标
├─ optimizers/    # prompt 生成策略
├─ diagnostics/   # 失败分析
├─ models/        # 模型适配
├─ cli/           # 命令行
├─ storage/       # 实验数据
```

---

## 与现有方案的区别

| 方案              | 特点                      | 问题      |
| --------------- | ----------------------- | ------- |
| 手工调 prompt      | 简单直接                    | 不可复现    |
| 自动改 prompt      | 单路径优化                   | 容易退化    |
| DSPy / TextGrad | 偏研究/程序优化                | 不强调工程回归 |
| **PromptOpt**   | eval + search + lineage | 专注结构化任务 |

---

## 设计原则

* 先评估，再优化
* 可复现 > 魔法
* 显式记录一切（artifact-first）
* 搜索，而不是单路径
* 约束优先（成本 / 延迟 / 格式）

---

## Roadmap

> 当前进度：M1–M7 路线图核心任务已全部落地，当前项目已具备从评估、诊断、搜索、门禁、CI 到 Web/Registry 的基础闭环。

### v0.1

* task / dataset / candidate 规范 ✅
* eval runner ✅
* artifact 保存 ✅

### v0.2

* diagnostics（失败分析） ✅
* 报告生成 ✅

### v0.3

* prompt 搜索（rewrite / few-shot / contract） ✅
* selection ✅

> 当前阶段说明：`optimize`、`search`、`select` 与基础 lineage 已落地，下一步重点是 held-out test gate、回归检测和约束检查。

### v0.4

* Pareto 优化 🔨
* cost-aware 搜索

---

## 贡献

欢迎贡献：

* evaluator
* diagnostics
* optimizer 策略
* 模型适配

---

## License

MIT
