# M7 团队协作与可视化平台进展记录

> 更新时间：2026-03-22

## 本轮已落地能力

### Web UI 基础框架

- 新增 `src/promptopt/web/app.py`
- 新增 `src/promptopt/web/static/index.html`
- 新增 `promptopt web` 命令，支持启动 PromptOpt Web UI
- 使用 FastAPI + 轻量静态前端实现，不引入前端构建链

### Run 对比与 Slice 可视化

- `GET /api/runs`
- `GET /api/runs/{run_id}`
- `GET /api/runs/{run_id}/diagnostics`
- `GET /api/compare`
- 前端页面可查看 runs 列表、单 run 详情、baseline diff、slice regression 与 prompt diff

### Prompt Registry

- 新增 `PromptRegistryEntryModel`
- 支持 `draft / review / approved / deployed` 状态机
- 提供 API：
  - `POST /api/registry`
  - `POST /api/registry/{candidate_id}/review`
  - `POST /api/registry/{candidate_id}/approve`
  - `POST /api/registry/{candidate_id}/deploy`
- 同一 `registry_key` 只保留一个 `deployed` 项，其余自动回落为 `approved`

## 本轮新增/更新文件

- `pyproject.toml`
- `src/promptopt/storage/models.py`
- `src/promptopt/storage/__init__.py`
- `src/promptopt/web/__init__.py`
- `src/promptopt/web/app.py`
- `src/promptopt/web/static/index.html`
- `src/promptopt/cli/main.py`
- `tests/test_web_api.py`
- `docs/m7-web-platform-progress.md`
- `README.md`
- `ROADMAP.md`

## 已验证命令

- `uv run pytest -q`
- `uv run ruff check src/ tests/`
- `uv run mypy src/`

## 当前状态评估

### 已完成

- `#38` Web UI 基础框架：已完成基础版
- `#39` run 对比视图：已完成基础版
- `#40` slice 可视化：已完成基础版
- `#41` Prompt Registry：已完成基础版
- `#42` 候选审批流：已完成基础版

### 后续可继续打磨

- 前端目前为轻量静态实现，后续可升级为 React/Vue 前端
- 审批流当前无鉴权/用户体系，后续可接入真正的团队权限模型
- slice 图表当前为轻量可视化，后续可升级为更丰富的图表组件

## 结果

M1–M7 路线图中的核心任务已全部落地，并通过测试、lint 与类型检查验证。
