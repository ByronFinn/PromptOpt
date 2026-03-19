# PromptOpt 开发指南

## 基本原则

1. **第一性原理**: 所有结论必须基于严密的证据或可信的信源，不编造、不臆测
2. **使用 uv 管理环境**: 必须使用 `uv` 而非 `pip` 进行依赖管理
3. **遵守 Python 代码规范**: 编写代码时必须严格遵守 [PYTHON_RULES.md](PYTHON_RULES.md) 中的所有规则，目标 Python 3.14

## 构建与测试命令

```bash
# 安装依赖
uv pip install -e ".[dev]"

# 运行测试
uv run pytest

# 类型检查
uv run mypy src/

# 代码格式化 / Lint
uv run ruff check src/ tests/

# 构建包
uv build
```

## 核心架构

```
src/promptopt/
├── cli/          # CLI 入口 (typer)
├── core/         # 核心模型 (Task, Dataset, Candidate, Run)
├── evaluators/   # 评估器 (exact_match, f1, json_validator)
├── optimizers/   # 优化策略 (contract, fewshot)
├── models/       # LLM 适配器 (litellm)
├── storage/      # 数据库模型
└── diagnostics/  # 失败分析
```

### 关键模型

- **Task**: 任务定义，包含 `prompt_template`（含 `{input}` 占位符）
- **Dataset**: 数据集配置
- **Candidate**: 候选 Prompt，包含 `CandidateMetadata` 记录优化策略
- **Run / RunResult / EvalResult**: 运行与评估结果

### 评估指标

支持 `exact_match`、`f1`、`json_validator`，可在 `Task.evaluation_metrics` 中指定。

### 优化策略

- `rewrite`: 指令重写
- `fewshot`: Few-shot 示例优化
- `contract`: JSON 输出约束强化

## 开发约定

### 代码风格

- 使用 **Ruff** (line-length: 88, target-version: py314)
- 使用 **Mypy** strict 模式
- Pydantic 模型使用 `model_dump()` 时需处理 `datetime` 序列化
- **所有代码必须遵守 [PYTHON_RULES.md](PYTHON_RULES.md) 规则**，包括但不限于：
  - 使用现代类型注解 (`list[int]`, `dict[str, ...]`, `X | None`)
  - 使用 `@dataclass(slots=True)` 作为数据容器
  - 避免 `Any`，除非不可避免
  - 避免 legacy 类型 (`List`, `Dict`, `Optional`)

### 模型定义示例

```python
class CandidateMetadata(BaseModel):
    strategy: Literal["rewrite", "fewshot", "contract", "baseline"]
    parent_id: str | None = None
    teacher_model: str | None = None
    generation_params: dict[str, object] = Field(default_factory=dict)
```

### CLI 开发

使用 **Typer** 构建 CLI，参考 [cli/main.py](src/promptopt/cli/main.py)：

- 命令使用 `@app.command()` 装饰器
- 使用 `rich.console.Console` 输出格式化的表格和消息
- 选项使用 `typer.Option(...)`，参数使用 `typer.Argument(...)`

### 测试

- 使用 **pytest** + **pytest-asyncio**
- 测试文件放在 `tests/` 目录
- 参考 [tests/test_core.py](tests/test_core.py) 中的 Pydantic 模型测试模式

## 示例项目

参考 [examples/json_extraction/](examples/json_extraction/) 了解完整的工作流：

1. `promptopt init <name>` 初始化项目
2. `promptopt eval` 运行评估
3. `promptopt diagnose` 分析失败
4. `promptopt optimize` 生成优化候选

## 常见陷阱

1. **datetime 序列化**: `Candidate.model_dump()` 已处理 datetime 序列化，继承时需注意
2. **pydantic ValidationError**: 使用 `pytest.raises(ValidationError)` 捕获验证错误
3. **异步测试**: 确保 `pytest.ini_options` 中 `asyncio_mode = "auto"`
