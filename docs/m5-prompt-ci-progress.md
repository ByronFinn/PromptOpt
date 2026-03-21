# M5 Prompt CI 进展记录

> 更新时间：2026-03-22

## 本轮已落地能力

### 非交互式运行模式

以下命令已支持 `--quiet` / `--output-json`：

- `promptopt diagnose`
- `promptopt search`
- `promptopt select`
- `promptopt verify`

### 报告生成

- `diagnose` 支持 `--report-file` / `--report-format markdown|html`
- `verify` 支持 `--report-file` / `--report-format markdown|html`
- 新增 `src/promptopt/cli/reporting.py` 统一处理 JSON / Markdown / HTML 输出

### Git diff 集成

- `search` 支持 `--changed-only`
- `search` 支持 `--git-base-ref <ref>`
- 可仅评估 Git diff 中变化过的候选 YAML

### CI 退出码规范

- `verify` 成功时返回 `0`
- `verify` 遇到运行/上下文恢复错误时返回 `1`
- `verify` 发生 regression / constraints gate failure 时返回 `2`

### GitHub Actions 示例

- 新增 `.github/workflows/promptopt.yml`
- 演示 `search -> select -> verify -> 上传报告` 的最小 Prompt CI 工作流

## 本轮新增/更新文件

- `src/promptopt/cli/main.py`
- `src/promptopt/cli/reporting.py`
- `tests/test_verify.py`
- `tests/test_search_select.py`
- `.github/workflows/promptopt.yml`
- `docs/m5-prompt-ci-progress.md`
- `README.md`
- `ROADMAP.md`

## 已验证命令

- `uv run pytest -q`
- `uv run ruff check src/ tests/`
- `uv run mypy src/`

## 当前状态评估

### 已完成

- `#28` 非交互式运行模式：已完成核心命令
- `#29` markdown/html 报告生成：已完成 diagnose/verify 基础版
- `#30` Git diff 集成：已完成 search 基础版
- `#31` CI 退出码规范：已完成 verify 基础版
- `#32` GitHub Actions 示例：已完成

### 仍待推进

- 可继续把 `eval` / `optimize` 也接入 `--output-json`
- 报告模板仍可继续美化，尤其是 HTML 样式
- Git diff 目前聚焦 candidate 文件，后续可继续增强 PR 上下文集成

## 下一步建议

进入 `M6`，优先实现：

1. Evaluator / Optimizer / Model Provider 插件入口
2. 额外 examples（classification / QA）
3. 插件开发文档
