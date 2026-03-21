# M2 失败分析系统进展记录

> 更新时间：2026-03-22

## 本轮已落地能力

### `promptopt diagnose` 初版闭环

- 基于 `run_id` 读取 `runs` 与 `run_samples`
- 输出 run 摘要、聚合指标、失败类别分布、slice 指标、Top 失败样本和建议
- 支持 `--top-k` 控制展示失败样本数量
- 支持 `--export-failures <path>` 导出失败样本 JSON
- 支持 `--baseline-run <run_id>` 生成 baseline diff 报告

### 诊断分析器初版

- 新增失败分类：
  - `format_error`
  - `semantic_error`
  - `execution_error`
  - `unknown`
- 新增 slice 分析：
  - `length_short`
  - `length_medium`
  - `length_long`
  - `contains_number`
  - `contains_negation`
- 自动生成 2~3 条优化建议

### baseline diff report

- 按 `sample_id` 对齐 baseline run 与 candidate run
- 生成 accuracy delta 与聚合指标 delta
- 输出退化样本（regressions）与提升样本（improvements）列表
- 对不兼容 run（如 split 不同）进行护栏校验

## 本轮新增/更新文件

- `src/promptopt/diagnostics/analyzer.py`
- `src/promptopt/diagnostics/__init__.py`
- `src/promptopt/cli/main.py`
- `tests/test_diagnostics.py`
- `ROADMAP.md`

## 已验证命令

- `uv run pytest tests/test_diagnostics.py -q`
- `uv run pytest -q`
- `uv run ruff check src/ tests/`
- `uv run mypy src/`

## 当前状态评估

### 已完成或基础可用

- `#13` `promptopt diagnose`：已完成
- `#14` 失败样本导出：已完成
- `#12` Slice Metrics：已完成基础版（当前以 accuracy 为主）
- `#11` 错误分类器：已完成基础版（当前为规则分类）
- `#15` baseline diff report：已完成

### 仍待推进

- 将错误分类进一步扩展到 roadmap 中的“理解错误 / 模型能力上限”层级
- 增加 slice 级 `f1` 等更多指标，而不仅是 accuracy
- 为 diagnose 输出补充更丰富的字段级 diff 和退化样本聚合

## 下一步建议

进入 `M3`，优先完成候选生成与搜索闭环，理由：

1. `eval + diagnose + baseline diff` 已能为优化提供反馈闭环
2. 当前最缺的是“自动生成候选并批量比较”的生产力能力
3. 现有 diagnostics 可以直接作为 M3 optimizer 的反馈输入
