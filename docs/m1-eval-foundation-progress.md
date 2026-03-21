# M1 评估底座进展记录

> 更新时间：2026-03-22

## 本轮完成内容

### 已闭环的 M1 Issues

- `#4` 评估引擎核心：新增 `EvaluationEngine`，支持项目配置发现、模型解析、样本级评估、聚合指标计算与结果持久化。
- `#5` `promptopt eval` 命令：CLI 已接入真实评估链路，可解析 `task/candidate/dataset` 并输出 run 摘要。
- `#6` Run 结果持久化：`runs` 表补充了任务路径、数据集路径、模型名、聚合指标和耗时等字段。
- `#7` sample-level 结果存储：新增 `run_samples` 表，记录输入、期望、实际输出、指标、错误信息。
- `#8` mypy 错误：修复 `generate_stream` 签名兼容问题，`uv run mypy src/` 已通过。
- `#9` ruff lint：整理类型注解与导入，`uv run ruff check src/ tests/` 已通过。
- `#10` `promptopt init` 完整逻辑：初始化命令现在会生成 `.promptopt.yaml`、`task.yaml`、`baseline.yaml`、`dataset.yaml`、`samples.json`。

### 顺手修复

- 修复 `examples/json_extraction/tasks/task.yaml` 无效的问题，使 README 示例与实现重新对齐。
- 将 prompt 渲染从 `str.format()` 改为安全的 `{input}` 直接替换，避免 JSON 花括号导致模板渲染失败。
- 为结构化任务增强 `exact_match` / `f1` 评估器，使其在 `expected` 为字典、`actual` 为 JSON 字符串时能做规范化比较。
- 新增项目根目录 `.env` 占位文件，提供 `OPENAI_API_KEY` / `OPENAI_BASE_URL` 本地配置入口。

## 新增/更新的关键文件

### 核心实现

- `src/promptopt/core/evaluation.py`
- `src/promptopt/cli/main.py`
- `src/promptopt/storage/models.py`
- `src/promptopt/storage/database.py`
- `src/promptopt/evaluators/exact_match.py`
- `src/promptopt/evaluators/f1.py`
- `src/promptopt/models/litellm_adapter.py`
- `src/promptopt/core/task.py`

### 测试

- `tests/test_eval_pipeline.py`

### 示例与文档

- `examples/json_extraction/tasks/task.yaml`
- `README.md`
- `ROADMAP.md`

## 已验证命令

以下命令已在当前工作区验证通过：

- `uv run pytest -q`
- `uv run pytest tests/test_eval_pipeline.py -q`
- `uv run ruff check src/ tests/`
- `uv run mypy src/`

## 当前仍待推进的方向

### M1 收尾

- 增加 sample-level 结果导出命令/格式（当前已存储，尚未提供单独导出入口）
- 进一步明确复现实验策略（如缓存、固定生成参数、provider 侧稳定性约束）

### 下一里程碑建议

优先进入 `M2`：

1. 读取 `run_samples`，实现错误分类器
2. 计算 slice 级指标
3. 落地 `promptopt diagnose`
4. 输出失败样本与 baseline diff 报告

## 备注

本轮实现遵循“先打通主链，再补体验”的原则，优先确保：

- `eval` 不是占位命令
- run 结果可查询
- sample-level 结果可追溯
- 示例项目可以真正跑起来
