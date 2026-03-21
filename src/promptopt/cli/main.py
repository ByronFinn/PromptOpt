"""CLI main entry point for PromptOpt."""

from pathlib import Path

import typer
from rich.console import Console
from rich.table import Table

from promptopt.core import (
    Candidate,
    DatasetLoader,
    EvaluationEngine,
    Split,
    Task,
    build_evaluators,
    build_model_adapter,
    discover_project_config,
)
from promptopt.storage import RunModel, get_db

app = typer.Typer(
    name="promptopt",
    help="评估驱动的 Prompt 搜索与回归测试框架",
    add_completion=False,
)

console = Console()


def _parse_split(raw_value: str) -> Split:
    normalized = raw_value.lower().strip()
    try:
        return Split(normalized)
    except ValueError as exc:
        allowed = ", ".join(split.value for split in Split)
        raise typer.BadParameter(f"split 必须是以下值之一: {allowed}") from exc


def _write_template(path: Path, content: str) -> None:
    if path.exists():
        return
    path.write_text(content, encoding="utf-8")

PROMPTOPT_CONFIG_TEMPLATE = """# PromptOpt Local Configuration

models:
    teacher: openai/gpt-4
    target: openai/gpt-3.5-turbo

evaluation:
    batch_size: 10
    max_workers: 5
    timeout: 60

storage:
    db_path: .promptopt/promptopt.db
"""

TASK_TEMPLATE = """name: text_extraction
description: 从输入文本中抽取结构化信息
dataset:
    name: sample_dataset
    path: datasets/dataset.yaml
    split_field: split
prompt_template: |
    从以下文本中抽取结构化信息，并以 JSON 输出：

    {input}
output_schema: |
    {"type": "object"}
evaluation_metrics:
    - exact_match
    - json_validator
"""

CANDIDATE_TEMPLATE = """id: baseline_001
name: baseline
description: 初始 baseline prompt

prompt: |
    从以下文本中抽取结构化信息，并以 JSON 输出：

    {input}

    输出格式：
    {
        "field": "value"
    }

metadata:
    strategy: baseline
"""

DATASET_CONFIG_TEMPLATE = """name: sample_dataset
path: datasets/samples.json
split_field: split
"""

SAMPLE_DATASET_TEMPLATE = """{
    "samples": [
        {
            "id": "sample_001",
            "input": "患者女性，38岁，咽痛伴低热3天。",
            "expected": {"field": "上呼吸道感染"},
            "split": "dev"
        },
        {
            "id": "sample_002",
            "input": "患者男性，60岁，反复胸闷2周。",
            "expected": {"field": "胸闷"},
            "split": "test"
        }
    ]
}
"""


@app.command()
def init(
    name: str = typer.Argument(..., help="项目名称"),
    output_dir: Path | None = typer.Option(None, "--output", "-o", help="输出目录"),
) -> None:
    """初始化一个新的 PromptOpt 项目."""
    output_path = output_dir or Path.cwd() / name
    output_path.mkdir(parents=True, exist_ok=True)
    
    # Create directory structure
    (output_path / "tasks").mkdir(exist_ok=True)
    (output_path / "candidates").mkdir(exist_ok=True)
    (output_path / "datasets").mkdir(exist_ok=True)
    (output_path / "runs").mkdir(exist_ok=True)
    (output_path / ".promptopt").mkdir(exist_ok=True)

    _write_template(output_path / ".promptopt.yaml", PROMPTOPT_CONFIG_TEMPLATE)
    _write_template(output_path / "tasks" / "task.yaml", TASK_TEMPLATE)
    _write_template(output_path / "candidates" / "baseline.yaml", CANDIDATE_TEMPLATE)
    _write_template(output_path / "datasets" / "dataset.yaml", DATASET_CONFIG_TEMPLATE)
    _write_template(output_path / "datasets" / "samples.json", SAMPLE_DATASET_TEMPLATE)

    console.print(f"[green]✓[/green] 项目已初始化: {output_path}")


@app.command()
def eval(
    task: Path = typer.Option(..., "--task", "-t", help="任务配置文件路径"),
    candidate: Path = typer.Option(..., "--candidate", "-c", help="候选配置路径"),
    dataset: Path = typer.Option(..., "--dataset", "-d", help="数据集配置路径"),
    split: str = typer.Option("dev", "--split", "-s", help="数据集划分"),
) -> None:
    """运行评估."""
    split_value = _parse_split(split)

    try:
        task_spec = Task.from_yaml(task)
        candidate_spec = Candidate.from_yaml(candidate)
        dataset_loader = DatasetLoader(
            path=str(dataset),
            split_field=task_spec.dataset.split_field,
        )
        project_config = discover_project_config([task, candidate, dataset])
        if project_config is None:
            raise ValueError(
                "未找到 .promptopt.yaml，请在项目根目录提供运行配置。"
            )

        engine = EvaluationEngine(
            adapter=build_model_adapter(project_config),
            evaluators=build_evaluators(task_spec.evaluation_metrics),
            db=get_db(project_config.db_path),
            timeout=project_config.timeout,
        )
        result = engine.run(
            task=task_spec,
            candidate=candidate_spec,
            dataset=dataset_loader,
            split=split_value,
            task_path=task,
            candidate_path=candidate,
            dataset_path=dataset,
        )
    except Exception as exc:
        console.print(f"[red]✗ 评估失败:[/red] {exc}")
        raise typer.Exit(code=1) from exc

    summary_table = Table(title=f"评估结果 · {result.run_id}")
    summary_table.add_column("字段", style="cyan")
    summary_table.add_column("值", style="green")
    summary_table.add_row("Task", task_spec.name)
    summary_table.add_row("Candidate", candidate_spec.name)
    summary_table.add_row("Model", engine.model_name)
    summary_table.add_row("Split", split_value.value)
    summary_table.add_row("Samples", str(result.total_samples))
    summary_table.add_row("Accuracy", f"{result.accuracy:.2%}")
    summary_table.add_row("Latency", f"{result.latency_ms:.2f} ms/sample")
    console.print(summary_table)

    if result.aggregate_metrics:
        metrics_table = Table(title="聚合指标")
        metrics_table.add_column("指标", style="magenta")
        metrics_table.add_column("值", style="yellow", justify="right")
        for metric_name, metric_value in sorted(result.aggregate_metrics.items()):
            metrics_table.add_row(metric_name, f"{metric_value:.4f}")
        console.print(metrics_table)

    error_count = sum(1 for sample in result.sample_results if sample.error)
    if error_count:
        console.print(f"[yellow]⚠[/yellow] 有 {error_count} 个样本在评估时出错。")

    console.print(f"[green]✓[/green] Run 已保存: {result.run_id}")


@app.command()
def diagnose(
    run_id: str = typer.Argument(..., help="Run ID"),
) -> None:
    """分析失败案例."""
    console.print(f"[blue]分析 Run:[/blue] {run_id}")
    
    # Placeholder - actual diagnosis logic goes here
    console.print("[yellow]诊断功能开发中...[/yellow]")


@app.command()
def optimize(
    run_id: str = typer.Argument(..., help="Run ID"),
    teacher: str = typer.Option("openai/gpt-4", "--teacher", help="Teacher 模型"),
    strategies: str = typer.Option("rewrite,fewshot", "--strategies", help="优化策略"),
    num_candidates: int = typer.Option(12, "--num-candidates", help="生成候选数量"),
) -> None:
    """生成候选 Prompt 优化."""
    console.print("[blue]优化配置:[/blue]")
    console.print(f"  Run: {run_id}")
    console.print(f"  Teacher: {teacher}")
    console.print(f"  Strategies: {strategies}")
    console.print(f"  Num Candidates: {num_candidates}")
    
    # Placeholder - actual optimization logic goes here
    console.print("[yellow]优化功能开发中...[/yellow]")


@app.command()
def search(
    candidates_dir: Path = typer.Argument(..., help="候选配置目录"),
    task: Path = typer.Option(..., "--task", "-t", help="任务配置路径"),
    dataset: Path = typer.Option(..., "--dataset", "-d", help="数据集配置路径"),
    split: str = typer.Option("dev", "--split", "-s", help="数据集划分"),
) -> None:
    """批量评估候选."""
    console.print("[blue]批量评估:[/blue]")
    console.print(f"  Candidates: {candidates_dir}")
    console.print(f"  Task: {task}")
    console.print(f"  Dataset: {dataset}")
    
    # Placeholder - actual search logic goes here
    console.print("[yellow]搜索功能开发中...[/yellow]")


@app.command()
def select(
    run_id: str = typer.Argument(..., help="Run ID"),
    primary: str = typer.Option("accuracy", "--primary", help="主要指标"),
    secondary: str | None = typer.Option(None, "--secondary", help="次要指标(逗号分隔)"),
) -> None:
    """选择最优候选."""
    console.print("[blue]选择最优候选:[/blue]")
    console.print(f"  Run: {run_id}")
    console.print(f"  Primary: {primary}")
    
    # Placeholder - actual selection logic goes here
    console.print("[yellow]选择功能开发中...[/yellow]")


@app.command()
def verify(
    run_id: str = typer.Argument(..., help="Run ID"),
    split: str = typer.Option("test", "--split", "-s", help="数据集划分"),
) -> None:
    """测试集验证."""
    console.print("[blue]测试集验证:[/blue]")
    console.print(f"  Run: {run_id}")
    console.print(f"  Split: {split}")
    
    # Placeholder - actual verification logic goes here
    console.print("[yellow]验证功能开发中...[/yellow]")


@app.command()
def list_runs() -> None:
    """列出所有 runs."""
    project_config = discover_project_config([Path.cwd()])
    db = get_db(project_config.db_path if project_config else None)
    
    table = Table(title="Runs")
    table.add_column("ID", style="cyan")
    table.add_column("Task", style="magenta")
    table.add_column("Candidate", style="green")
    table.add_column("Model", style="blue")
    table.add_column("Status", style="yellow")
    table.add_column("Accuracy", justify="right")
    
    with db.session() as session:
        runs = session.query(RunModel).order_by(RunModel.created_at.desc()).limit(20).all()
        
        for run in runs:
            table.add_row(
                run.id,
                run.task_id,
                run.candidate_id,
                run.model_name or "-",
                run.status,
                f"{run.accuracy:.2%}" if run.accuracy else "-",
            )
    
    console.print(table)


@app.command()
def version() -> None:
    """显示版本信息."""
    from promptopt import __version__
    console.print(f"PromptOpt v{__version__}")


if __name__ == "__main__":
    app()
