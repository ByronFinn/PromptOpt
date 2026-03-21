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
from promptopt.diagnostics import (
    BaselineDiffReport,
    DiagnosticsAnalyzer,
    DiagnosticsReport,
)
from promptopt.storage import RunModel, SampleResultModel, get_db

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


def _truncate_text(text: str, *, limit: int) -> str:
    if len(text) <= limit:
        return text
    return f"{text[: limit - 1]}…"


def _render_diagnostics_report(report: DiagnosticsReport) -> None:
    summary_table = Table(title=f"失败分析 · {report.run_id}")
    summary_table.add_column("字段", style="cyan")
    summary_table.add_column("值", style="green")
    summary_table.add_row("Task", report.task_id)
    summary_table.add_row("Model", report.model_name or "-")
    summary_table.add_row("Samples", str(report.total_samples))
    summary_table.add_row("Failures", str(report.failed_samples))
    summary_table.add_row("Accuracy", f"{report.accuracy:.2%}")
    console.print(summary_table)

    if report.aggregate_metrics:
        metrics_table = Table(title="聚合指标")
        metrics_table.add_column("指标", style="magenta")
        metrics_table.add_column("值", style="yellow", justify="right")
        for metric_name, metric_value in sorted(report.aggregate_metrics.items()):
            metrics_table.add_row(metric_name, f"{metric_value:.4f}")
        console.print(metrics_table)

    if report.category_counts:
        category_table = Table(title="失败类别分布")
        category_table.add_column("类别", style="red")
        category_table.add_column("数量", style="yellow", justify="right")
        for category_name, count in sorted(report.category_counts.items()):
            category_table.add_row(category_name, str(count))
        console.print(category_table)

    if report.slice_metrics:
        slice_table = Table(title="Slice 指标")
        slice_table.add_column("Slice", style="blue")
        slice_table.add_column("Total", justify="right")
        slice_table.add_column("Failed", justify="right")
        slice_table.add_column("Accuracy", justify="right")
        for slice_name, metrics in sorted(report.slice_metrics.items()):
            total = metrics.get("total", 0)
            failed = metrics.get("failed", 0)
            accuracy = metrics.get("accuracy", 0.0)
            total_display = str(total) if isinstance(total, int) else "0"
            failed_display = str(failed) if isinstance(failed, int) else "0"
            accuracy_display = f"{accuracy:.2%}" if isinstance(accuracy, float) else "0.00%"
            slice_table.add_row(slice_name, total_display, failed_display, accuracy_display)
        console.print(slice_table)

    if report.top_failures:
        failure_table = Table(title="Top 失败样本")
        failure_table.add_column("Sample", style="cyan")
        failure_table.add_column("Category", style="red")
        failure_table.add_column("Reason", style="yellow")
        failure_table.add_column("Input", style="green")
        for failure in report.top_failures:
            failure_table.add_row(
                failure.sample_id,
                failure.category.value,
                _truncate_text(failure.reason, limit=48),
                _truncate_text(failure.input_text, limit=36),
            )
        console.print(failure_table)

    if report.suggestions:
        console.print("[blue]建议:[/blue]")
        for suggestion in report.suggestions:
            console.print(f"- {suggestion}")


def _render_baseline_diff_report(report: BaselineDiffReport) -> None:
    summary_table = Table(title=f"Baseline Diff · {report.run_id}")
    summary_table.add_column("字段", style="cyan")
    summary_table.add_column("值", style="green")
    summary_table.add_row("Task", report.task_id)
    summary_table.add_row("Split", report.split)
    summary_table.add_row("Baseline Run", report.baseline_run_id)
    summary_table.add_row("Candidate Run", report.run_id)
    summary_table.add_row("Matched Samples", str(report.matched_samples))
    summary_table.add_row("Baseline Accuracy", f"{report.baseline_accuracy:.2%}")
    summary_table.add_row("Candidate Accuracy", f"{report.accuracy:.2%}")
    summary_table.add_row("Accuracy Delta", f"{report.accuracy_delta:+.2%}")
    summary_table.add_row("Still Correct", str(report.still_correct))
    summary_table.add_row("Still Failed", str(report.still_failed))
    console.print(summary_table)

    warnings: list[str] = []
    if report.baseline_only_samples:
        warnings.append(f"baseline only: {len(report.baseline_only_samples)}")
    if report.candidate_only_samples:
        warnings.append(f"candidate only: {len(report.candidate_only_samples)}")
    if report.conflicted_samples:
        warnings.append(f"conflicts: {len(report.conflicted_samples)}")
    if warnings:
        console.print(f"[yellow]⚠ 对齐警告:[/yellow] {'; '.join(warnings)}")

    if report.aggregate_metric_deltas:
        delta_table = Table(title="聚合指标 Delta")
        delta_table.add_column("指标", style="magenta")
        delta_table.add_column("Delta", style="yellow", justify="right")
        for metric_name, delta in sorted(report.aggregate_metric_deltas.items()):
            delta_table.add_row(metric_name, f"{delta:+.4f}")
        console.print(delta_table)

    if report.regressions:
        regression_table = Table(title="退化样本")
        regression_table.add_column("Sample", style="cyan")
        regression_table.add_column("当前类别", style="red")
        regression_table.add_column("原因", style="yellow")
        regression_table.add_column("Input", style="green")
        for sample_diff in report.regressions:
            category = (
                sample_diff.candidate_failure.category.value
                if sample_diff.candidate_failure is not None
                else "unknown"
            )
            reason = (
                sample_diff.candidate_failure.reason
                if sample_diff.candidate_failure is not None
                else "基线正确，但当前 run 回退。"
            )
            regression_table.add_row(
                sample_diff.sample_id,
                category,
                _truncate_text(reason, limit=48),
                _truncate_text(sample_diff.input_text, limit=36),
            )
        console.print(regression_table)

    if report.improvements:
        improvement_table = Table(title="提升样本")
        improvement_table.add_column("Sample", style="cyan")
        improvement_table.add_column("基线类别", style="blue")
        improvement_table.add_column("原因", style="yellow")
        improvement_table.add_column("Input", style="green")
        for sample_diff in report.improvements:
            category = (
                sample_diff.baseline_failure.category.value
                if sample_diff.baseline_failure is not None
                else "unknown"
            )
            reason = (
                sample_diff.baseline_failure.reason
                if sample_diff.baseline_failure is not None
                else "当前 run 修复了基线失败。"
            )
            improvement_table.add_row(
                sample_diff.sample_id,
                category,
                _truncate_text(reason, limit=48),
                _truncate_text(sample_diff.input_text, limit=36),
            )
        console.print(improvement_table)

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
    baseline_run: str | None = typer.Option(
        None,
        "--baseline-run",
        help="用于对比的 baseline run ID",
    ),
    top_k: int = typer.Option(5, "--top-k", help="展示失败样本数量"),
    export_failures: Path | None = typer.Option(
        None,
        "--export-failures",
        help="将失败样本导出为 JSON 文件",
    ),
) -> None:
    """分析失败案例."""
    project_config = discover_project_config([Path.cwd()])
    db = get_db(project_config.db_path if project_config else None)
    analyzer = DiagnosticsAnalyzer()
    report: DiagnosticsReport | BaselineDiffReport

    with db.session() as session:
        run = session.get(RunModel, run_id)
        if run is None:
            console.print(f"[red]✗ 未找到 Run:[/red] {run_id}")
            raise typer.Exit(code=1)

        sample_results = (
            session.query(SampleResultModel)
            .filter(SampleResultModel.run_id == run_id)
            .order_by(SampleResultModel.sample_id.asc())
            .all()
        )
        if baseline_run is None:
            report = analyzer.analyze_run(run, sample_results, top_k=top_k)
        else:
            baseline = session.get(RunModel, baseline_run)
            if baseline is None:
                console.print(f"[red]✗ 未找到 Baseline Run:[/red] {baseline_run}")
                raise typer.Exit(code=1)
            baseline_sample_results = (
                session.query(SampleResultModel)
                .filter(SampleResultModel.run_id == baseline_run)
                .order_by(SampleResultModel.sample_id.asc())
                .all()
            )
            try:
                report = analyzer.compare_runs(
                    baseline,
                    baseline_sample_results,
                    run,
                    sample_results,
                    top_k=top_k,
                )
            except ValueError as exc:
                console.print(f"[red]✗ 无法比较 runs:[/red] {exc}")
                raise typer.Exit(code=1) from exc

    if isinstance(report, DiagnosticsReport):
        _render_diagnostics_report(report)
    else:
        _render_baseline_diff_report(report)

    if export_failures is not None:
        if not isinstance(report, DiagnosticsReport):
            console.print("[red]✗ baseline diff 模式暂不支持 --export-failures。[/red]")
            raise typer.Exit(code=1)
        analyzer.export_failures(report.failures, export_failures)
        console.print(f"[green]✓[/green] 失败样本已导出到: {export_failures}")


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
