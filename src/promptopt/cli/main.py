"""CLI main entry point for PromptOpt."""

from pathlib import Path

import typer
from rich.console import Console
from rich.table import Table

from promptopt.storage import get_db

app = typer.Typer(
    name="promptopt",
    help="评估驱动的 Prompt 搜索与回归测试框架",
    add_completion=False,
)

console = Console()


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
    
    console.print(f"[green]✓[/green] 项目已初始化: {output_path}")


@app.command()
def eval(
    task: Path = typer.Option(..., "--task", "-t", help="任务配置文件路径"),
    candidate: Path = typer.Option(..., "--candidate", "-c", help="候选配置路径"),
    dataset: Path = typer.Option(..., "--dataset", "-d", help="数据集配置路径"),
    split: str = typer.Option("dev", "--split", "-s", help="数据集划分"),
) -> None:
    """运行评估."""
    console.print("[blue]评估配置:[/blue]")
    console.print(f"  Task: {task}")
    console.print(f"  Candidate: {candidate}")
    console.print(f"  Dataset: {dataset}")
    console.print(f"  Split: {split}")
    
    # Placeholder - actual evaluation logic goes here
    console.print("[yellow]评估功能开发中...[/yellow]")


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
    teacher: str = typer.Option("openai:gpt-4", "--teacher", help="Teacher 模型"),
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
    db = get_db()
    
    table = Table(title="Runs")
    table.add_column("ID", style="cyan")
    table.add_column("Task", style="magenta")
    table.add_column("Candidate", style="green")
    table.add_column("Status", style="yellow")
    table.add_column("Accuracy", justify="right")
    
    with db.session() as session:
        from promptopt.storage.models import RunModel
        runs = session.query(RunModel).order_by(RunModel.created_at.desc()).limit(20).all()
        
        for run in runs:
            table.add_row(
                run.id,
                run.task_id,
                run.candidate_id,
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
