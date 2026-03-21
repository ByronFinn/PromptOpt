# PromptOpt 插件开发指南

## 扩展点总览

PromptOpt 当前支持三类插件：

- **Evaluator**：新增评估指标
- **Optimizer**：新增 prompt 生成策略
- **Provider**：新增模型 provider / adapter 构造逻辑

## Python entry points

推荐使用以下 entry point group 注册插件：

- `promptopt.evaluators`
- `promptopt.optimizers`
- `promptopt.providers`

示例：

```toml
[project.entry-points."promptopt.evaluators"]
custom_metric = "your_package.evaluators:CustomEvaluator"

[project.entry-points."promptopt.optimizers"]
custom_optimizer = "your_package.optimizers:CustomOptimizer"

[project.entry-points."promptopt.providers"]
custom_provider = "your_package.providers:CustomProvider"
```

## Evaluator 插件

插件类应继承 `promptopt.evaluators.base.Evaluator`，并满足：

- 提供 `name`
- 实现 `evaluate(expected, actual, **kwargs)`
- 返回 `(is_correct, metrics_dict)`

注册后，可在 `task.yaml` 的 `evaluation_metrics` 中直接引用对应名字。

## Optimizer 插件

插件类应继承 `promptopt.optimizers.base.Optimizer`，并满足：

- 提供 `name`
- 实现 `optimize(current_prompt, eval_results, task_description, **kwargs)`
- 返回 `list[str]` 候选 prompt

宿主目前可能传入的 `kwargs` 包括：

- `teacher_adapter`
- `sample_results`
- `output_schema`
- `num_candidates`

注册后，可在 CLI 中通过 `--strategies <name>` 使用。

## Provider 插件

Provider 插件应实现 `promptopt.models.base.ModelProvider` 协议，核心能力包括：

- `name`
- `supports(model_name: str) -> bool`
- `build(model_name, *, api_key=None, base_url=None, **kwargs) -> ModelAdapter`

其中 `build(...)` 必须返回 `ModelAdapter` 实例。

## 最小 smoke test

建议使用 `uv` 安装和验证插件：

```bash
uv pip install -e ".[dev]"
uv run pytest -q
uv run mypy src/
```

## 命名建议

- evaluator 名称应直接作为 metric key 使用
- optimizer 名称应简洁稳定，会写入 candidate metadata
- provider 推荐使用 `provider/model` 风格模型名

## 常见坑

- optimizer 名称如果和内置策略重名，会覆盖或冲突，建议避免同名
- evaluator 第一版建议零参可实例化
- provider 插件不应只暴露 adapter class，而应暴露 provider factory/plugin
- 若插件需要额外配置，建议通过 `.promptopt.yaml` 或运行环境变量传入
