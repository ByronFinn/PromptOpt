# M3 候选生成与搜索进展记录

> 更新时间：2026-03-22

## 本轮已落地能力

### `promptopt optimize` 主链

- 支持通过 `run_id` 回捞 `task.yaml` / `candidate.yaml` 上下文
- 读取 `diagnose` 结果作为 teacher 的优化反馈输入
- 解析 `.promptopt.yaml` 中的 `models.teacher`
- 支持显式 `--teacher` 覆盖配置文件中的 teacher
- 支持 `rewrite` / `fewshot` / `contract` 三种策略
- 可生成多个候选并写入 `candidates/` 目录 YAML 文件

### Optimizer 第一版

- `RewriteOptimizer` 使用 teacher 模型生成候选 prompt
- 要求 teacher 返回 JSON 格式候选数组
- 当 teacher 返回不可解析结果时，自动回退到内建 rewrite 模板，确保命令可产出候选
- `FewShotOptimizer` 会从已评估正确样本中自动挑选示例并拼入 prompt
- `ContractOptimizer` 会基于 schema 与诊断建议强化输出约束
- 候选 metadata 会记录：
  - `strategy`
  - `parent_id`
  - `teacher_model`
  - `generation_params`

### `promptopt search` / `promptopt select`

- `search` 支持批量评估候选目录中的 YAML 文件
- `select` 支持基于主指标与次指标从兼容 runs 中选出最优候选
- 批量评估结果会持久化到数据库，可与 `list-runs` / `diagnose` 复用

### lineage 持久化

- 评估候选时自动写入 `LineageModel`
- 自动记录：`ancestors`、`parent_id`、`change_type` 与 prompt unified diff

## 本轮新增/更新文件

- `src/promptopt/cli/main.py`
- `src/promptopt/core/evaluation.py`
- `src/promptopt/core/__init__.py`
- `src/promptopt/optimizers/base.py`
- `src/promptopt/optimizers/fewshot.py`
- `src/promptopt/optimizers/contract.py`
- `tests/test_optimize.py`
- `tests/test_search_select.py`
- `README.md`
- `examples/json_extraction/README.md`
- `ROADMAP.md`

## 已验证命令

- `uv run pytest tests/test_optimize.py -q`
- `uv run pytest -q`
- `uv run ruff check src/ tests/`
- `uv run mypy src/`

## 当前状态评估

### 已完成

- `#16` RewriteOptimizer：已完成第一版
- `#17` FewshotOptimizer：已完成基础版
- `#18` ContractOptimizer：已完成基础版
- `#19` `promptopt optimize`：已完成
- `#20` 多候选并行评估：已完成基础版
- `#21` `promptopt select`：已完成第一版
- `#22` 候选 lineage 追踪：已完成基础版

### 仍待推进

- few-shot 示例选择策略仍可继续优化
- contract/schema 约束仍可进一步增强
- search 当前仍是串行评估，后续可升级为真正并行
- select 当前仍是基于规则的第一版选择器

## 下一步建议

优先继续完成：

1. `verify` held-out test gate
2. regression detection
3. constraints / rollback / prompt diff review

这样可以把当前已打通的 M3 搜索闭环升级为真正的工程门禁系统。
