# M4 回归门禁与上线控制进展记录

> 更新时间：2026-03-22

## 本轮已落地能力

### `promptopt verify` held-out test gate

- 支持通过历史 `run_id` 回捞 `task/candidate/dataset` 上下文
- 支持在 `test` 等 held-out split 上重新评估并持久化新的 verify run
- 输出验证摘要与聚合指标

### regression detection

- `verify` 支持 `--baseline-run <run_id>`
- 复用 `DiagnosticsAnalyzer.compare_runs()` 比较 baseline run 与 verify run
- 新增 slice regression 检测：若关键 slice accuracy 下降，则 verify gate 失败

### constraints gate

- `.promptopt.yaml` 中的 `constraints` 会被解析到 `ProjectConfig`
- `select` 支持 `--constraints`
- `verify` 支持 `--constraints`
- 当前支持：
  - 指标下限（如 `json_validity=1.0`）
  - 上限约束（如 `max_latency=5000`、`max_cost=10.0`）

### prompt diff review

- `diagnose --baseline-run` 会输出 candidate prompt diff
- 单 run `diagnose` 在有 lineage diff 时也会展示 prompt diff

### rollback（弱回滚）

- 新增 `promptopt rollback <candidate_id>`
- 可从历史 `CandidateModel` 导出新的 rollback YAML 工件
- 不改数据库 schema，先实现“恢复历史 prompt 工件”的弱回滚

## 本轮新增/更新文件

- `src/promptopt/cli/main.py`
- `src/promptopt/core/evaluation.py`
- `src/promptopt/diagnostics/analyzer.py`
- `tests/test_verify.py`
- `tests/test_diagnostics.py`
- `tests/test_search_select.py`
- `docs/m4-regression-gate-progress.md`
- `README.md`
- `ROADMAP.md`

## 已验证命令

- `uv run pytest -q`
- `uv run ruff check src/ tests/`
- `uv run mypy src/`

## 当前状态评估

### 已完成

- `#23` `promptopt verify`：已完成
- `#24` regression detection：已完成基础版
- `#25` constraints gate：已完成基础版
- `#26` rollback：已完成弱回滚版本
- `#27` prompt diff review：已完成基础版

### 仍待推进

- cost 约束当前依赖 `RunModel.cost`，后续可进一步接入真实 usage/cost 采集
- regression detection 当前以 slice accuracy 为主，后续可扩展到更多指标
- rollback 当前是工件回滚，不是“active deployment state rollback”

## 下一步建议

进入 `M5`，优先实现：

1. `verify` / `search` / `select` 的 `--quiet --output-json`
2. `diagnose` / `verify` 的 markdown/html 报告
3. CI 退出码规范与 GitHub Actions 示例
