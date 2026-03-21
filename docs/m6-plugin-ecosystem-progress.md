# M6 插件化与生态进展记录

> 更新时间：2026-03-22

## 本轮已落地能力

### 插件注册与发现

- 新增 `src/promptopt/plugins.py`
- 支持通过 Python entry points 发现三类插件：
  - `promptopt.evaluators`
  - `promptopt.optimizers`
  - `promptopt.providers`
- 提供内置 registry + 外部 entry points 合并机制

### 插件接口

- `Evaluator` 继续作为 evaluator 插件实现面
- `Optimizer` 继续作为 optimizer 插件实现面
- 在 `src/promptopt/models/base.py` 中新增 `ModelProvider` 协议，用于 provider 插件路由与构造

### 宿主接线

- `build_evaluators()` 已改为使用 evaluator registry
- `build_model_adapter()` / `build_teacher_model_adapter()` 已改为通过 provider registry 路由
- `optimize` 策略分发已改为通过 optimizer registry 获取实现
- `CandidateMetadata.strategy` 已放宽为字符串，以支持第三方 optimizer 名称

### example / template 体系

- 新增完整 examples：
  - `examples/classification`
  - `examples/qa`
- `promptopt init` 新增 `--template`
- 当前支持模板：
  - `default`
  - `json_extraction`
  - `classification`
  - `qa`

### 插件开发文档

- 新增 `docs/plugin-development.md`
- 覆盖 evaluator / optimizer / provider 的实现、注册、命名与常见坑

## 本轮新增/更新文件

- `src/promptopt/plugins.py`
- `src/promptopt/models/base.py`
- `src/promptopt/models/__init__.py`
- `src/promptopt/core/candidate.py`
- `src/promptopt/core/evaluation.py`
- `src/promptopt/cli/main.py`
- `docs/plugin-development.md`
- `docs/m6-plugin-ecosystem-progress.md`
- `examples/classification/**`
- `examples/qa/**`
- `tests/test_examples.py`
- `tests/test_plugins.py`

## 已验证命令

- `uv run pytest -q`
- `uv run ruff check src/ tests/`
- `uv run mypy src/`

## 当前状态评估

### 已完成

- `#33` Evaluator 插件接口：已完成
- `#34` Optimizer 插件接口：已完成
- `#35` Model Provider 插件接口：已完成基础版
- `#36` example 模板体系：已完成基础版
- `#37` 插件开发文档：已完成

### 仍待推进

- provider 插件的更复杂配置分发仍可继续增强
- `init --template` 当前复用 examples 目录，后续可继续演进为独立模板 manifest
- 插件冲突覆盖策略当前仍保持“后加载覆盖”，后续可继续细化

## 下一步建议

进入 `M7`，优先实现：

1. Web UI 基础框架
2. run 对比与 slice 可视化
3. Prompt registry / 审批流最小模型
