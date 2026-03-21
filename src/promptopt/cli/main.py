"""CLI main entry point for PromptOpt."""

import difflib
import json
import subprocess
from collections.abc import Callable
from pathlib import Path
from typing import Literal

import typer
import yaml
from rich.console import Console
from rich.table import Table

from promptopt.cli.reporting import to_json_text, to_jsonable, write_report_file
from promptopt.core import (
    Candidate,
    CandidateMetadata,
    DatasetLoader,
    EvaluationEngine,
    ProjectConfig,
    Split,
    Task,
    build_evaluators,
    build_model_adapter,
    build_teacher_model_adapter,
    discover_project_config,
)
from promptopt.diagnostics import (
    BaselineDiffReport,
    DiagnosticsAnalyzer,
    DiagnosticsReport,
)
from promptopt.optimizers import ContractOptimizer, FewShotOptimizer, RewriteOptimizer
from promptopt.storage import RunModel, SampleResultModel, get_db
from promptopt.storage.models import CandidateModel, LineageModel

app = typer.Typer(
    name="promptopt",
    help="评估驱动的 Prompt 搜索与回归测试框架",
    add_completion=False,
)

console = Console()

type OptimizerStrategy = Literal["rewrite", "fewshot", "contract"]


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


def _render_prompt_diff(diff_text: str, *, title: str = "Prompt Diff") -> None:
    if not diff_text.strip():
        return
    console.print(f"[blue]{title}:[/blue]")
    console.print(diff_text)


def _resolve_artifact_path(raw_path: str | None, project_root: Path) -> Path:
    if raw_path is None or not raw_path.strip():
        raise ValueError("Run 缺少必要的 artifact 路径，无法恢复上下文。")

    candidate = Path(raw_path).expanduser()
    if candidate.is_absolute():
        return candidate
    return (project_root / candidate).resolve()


def _write_candidate_yaml(candidate: Candidate, output_path: Path) -> None:
    output_path.parent.mkdir(parents=True, exist_ok=True)
    payload = candidate.model_dump(mode="json")
    output_path.write_text(
        yaml.safe_dump(payload, allow_unicode=True, sort_keys=False),
        encoding="utf-8",
    )


def _build_candidate_file_path(
    parent_dir: Path,
    candidate_id: str,
) -> Path:
    output_path = parent_dir / f"{candidate_id}.yaml"
    if not output_path.exists():
        return output_path

    suffix = 2
    while True:
        output_path = parent_dir / f"{candidate_id}_{suffix:02d}.yaml"
        if not output_path.exists():
            return output_path
        suffix += 1


def _build_prompt_diff_text(
    from_label: str,
    from_prompt: str,
    to_label: str,
    to_prompt: str,
) -> str:
    diff_lines = difflib.unified_diff(
        from_prompt.splitlines(),
        to_prompt.splitlines(),
        fromfile=from_label,
        tofile=to_label,
        lineterm="",
    )
    return "\n".join(diff_lines)


def _normalize_strategies(raw_strategies: str) -> list[str]:
    return [item.strip() for item in raw_strategies.split(",") if item.strip()]


def _coerce_optimizer_strategies(raw_strategies: list[str]) -> list[OptimizerStrategy]:
    strategies: list[OptimizerStrategy] = []
    for item in raw_strategies:
        if item == "rewrite":
            strategies.append("rewrite")
        elif item == "fewshot":
            strategies.append("fewshot")
        elif item == "contract":
            strategies.append("contract")
    return strategies


def _build_optimize_eval_payload(report: DiagnosticsReport) -> dict[str, object]:
    return {
        "run_id": report.run_id,
        "task_id": report.task_id,
        "accuracy": report.accuracy,
        "aggregate_metrics": report.aggregate_metrics,
        "slice_metrics": report.slice_metrics,
        "suggestions": report.suggestions,
        "top_failures": report.top_failures,
    }


def _build_diagnostics_payload(
    report: DiagnosticsReport,
    *,
    prompt_diff_text: str | None,
) -> dict[str, object]:
    return {
        "kind": "diagnostics",
        "report": to_jsonable(report),
        "prompt_diff": prompt_diff_text,
    }


def _build_baseline_diff_payload(
    report: BaselineDiffReport,
    *,
    prompt_diff_text: str | None,
) -> dict[str, object]:
    return {
        "kind": "baseline_diff",
        "report": to_jsonable(report),
        "prompt_diff": prompt_diff_text,
    }


def _build_search_payload(results: list[dict[str, object]]) -> dict[str, object]:
    return {
        "kind": "search",
        "results": to_jsonable(results),
    }


def _build_select_payload(
    *,
    seed_run_id: str,
    selected_run_id: str,
    selected_candidate_id: str,
    primary: str,
    secondary_metrics: list[str],
    constraints: dict[str, float],
    rows: list[dict[str, object]],
) -> dict[str, object]:
    return {
        "kind": "select",
        "seed_run_id": seed_run_id,
        "selected_run_id": selected_run_id,
        "selected_candidate_id": selected_candidate_id,
        "primary": primary,
        "secondary": secondary_metrics,
        "constraints": constraints,
        "candidates": rows,
    }


def _build_verify_payload(
    *,
    source_run_id: str,
    verify_run_id: str,
    split: str,
    result: dict[str, object],
    constraints: dict[str, float],
    gate_failures: list[str],
    regression_failures: list[str],
    baseline_diff: dict[str, object] | None,
) -> dict[str, object]:
    return {
        "kind": "verify",
        "source_run_id": source_run_id,
        "verify_run_id": verify_run_id,
        "split": split,
        "result": result,
        "constraints": constraints,
        "constraint_failures": gate_failures,
        "regression_failures": regression_failures,
        "baseline_diff": baseline_diff,
        "exit_code": 2 if gate_failures or regression_failures else 0,
    }


def _emit_structured_output(
    *,
    title: str,
    payload: dict[str, object],
    quiet: bool,
    output_json: bool,
    report_file: Path | None,
    report_format: str,
    rich_renderer: Callable[[], None] | None = None,
) -> None:
    if report_file is not None:
        write_report_file(
            title=title,
            payload=payload,
            destination=report_file,
            report_format=report_format,
        )
    if output_json:
        typer.echo(to_json_text(payload))
        return
    if quiet:
        return
    if rich_renderer is not None:
        rich_renderer()


def _resolve_project_config_or_exit(candidate_paths: list[Path]) -> ProjectConfig:
    project_config = discover_project_config(candidate_paths)
    if project_config is None:
        console.print("[red]✗ 未找到 .promptopt.yaml，请在项目根目录提供运行配置。[/red]")
        raise typer.Exit(code=1)
    return project_config


def _load_run_and_samples_or_exit(
    run_id: str,
) -> tuple[RunModel, list[SampleResultModel], ProjectConfig]:
    initial_config = discover_project_config([Path.cwd()])
    db = get_db(initial_config.db_path if initial_config else None)

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
        run_task_path = run.task_path
        run_candidate_path = run.candidate_path

    project_config = _resolve_project_config_or_exit(
        [
            Path.cwd(),
            Path(run_task_path) if run_task_path is not None else Path.cwd(),
            Path(run_candidate_path) if run_candidate_path is not None else Path.cwd(),
        ]
    )
    return run, sample_results, project_config


def _load_task_candidate_context_from_run(
    run: RunModel,
    project_config: ProjectConfig,
) -> tuple[Path, Path, Task, Candidate]:
    config_path = project_config.config_path
    if config_path is None:
        raise ValueError("未找到 .promptopt.yaml 路径。")

    project_root = config_path.parent
    task_path = _resolve_artifact_path(run.task_path, project_root)
    candidate_path = _resolve_artifact_path(run.candidate_path, project_root)
    task_spec = Task.from_yaml(task_path)
    parent_candidate = Candidate.from_yaml(candidate_path)
    return task_path, candidate_path, task_spec, parent_candidate


def _instantiate_optimizer(
    strategy: OptimizerStrategy,
) -> RewriteOptimizer | FewShotOptimizer | ContractOptimizer:
    if strategy == "rewrite":
        return RewriteOptimizer()
    if strategy == "fewshot":
        return FewShotOptimizer()
    if strategy == "contract":
        return ContractOptimizer()
    raise ValueError(f"不支持的优化策略: {strategy}")


def _build_generation_kwargs(
    *,
    strategy: OptimizerStrategy,
    teacher_adapter: object,
    num_candidates: int,
    sample_results: list[SampleResultModel],
    task_spec: Task,
) -> dict[str, object]:
    kwargs: dict[str, object] = {
        "num_candidates": num_candidates,
    }
    if strategy == "rewrite":
        kwargs.update(
            {
                "teacher_adapter": teacher_adapter,
                "temperature": 0.7,
                "max_tokens": 2048,
            }
        )
    elif strategy == "fewshot":
        kwargs.update(
            {
                "sample_results": sample_results,
                "max_examples": min(3, max(1, num_candidates)),
            }
        )
    elif strategy == "contract":
        kwargs.update({"output_schema": task_spec.output_schema})
    return kwargs


def _parse_secondary_metrics(raw_secondary: str | None) -> list[str]:
    if raw_secondary is None:
        return []
    return [item.strip() for item in raw_secondary.split(",") if item.strip()]


def _parse_constraints(raw_constraints: str | None) -> dict[str, float]:
    if raw_constraints is None or not raw_constraints.strip():
        return {}

    constraints: dict[str, float] = {}
    for chunk in raw_constraints.split(","):
        item = chunk.strip()
        if not item:
            continue
        if "=" not in item:
            raise typer.BadParameter(f"无效约束格式: {item}，应为 key=value")
        key, raw_value = item.split("=", 1)
        constraint_name = key.strip()
        value_text = raw_value.strip()
        try:
            constraints[constraint_name] = float(value_text)
        except ValueError as exc:
            raise typer.BadParameter(f"约束值必须是数值: {item}") from exc
    return constraints


def _merge_constraints(project_config: ProjectConfig, raw_constraints: str | None) -> dict[str, float]:
    constraints = dict(project_config.constraints)
    constraints.update(_parse_constraints(raw_constraints))
    return constraints


def _metric_value_for_run(run: RunModel, metric_name: str) -> float:
    if metric_name == "accuracy":
        return float(run.accuracy)
    if metric_name in {"latency", "latency_ms"}:
        return float(run.latency_ms)
    if metric_name == "cost":
        return float(run.cost)
    try:
        parsed_metrics: object = json.loads(run.aggregate_metrics_json)
    except json.JSONDecodeError:
        return 0.0
    if not isinstance(parsed_metrics, dict):
        return 0.0
    value = parsed_metrics.get(metric_name)
    if isinstance(value, bool):
        return 0.0
    if isinstance(value, (int, float)):
        return float(value)
    return 0.0


def _collect_candidate_files(candidates_dir: Path) -> list[Path]:
    return sorted(
        path for path in candidates_dir.glob("*.y*ml") if path.is_file()
    )


def _filter_candidate_files_by_git_diff(
    candidate_files: list[Path],
    *,
    candidates_dir: Path,
    git_base_ref: str,
) -> list[Path]:
    command = [
        "git",
        "diff",
        "--name-only",
        f"{git_base_ref}...HEAD",
        "--",
        str(candidates_dir),
    ]
    completed = subprocess.run(
        command,
        check=False,
        capture_output=True,
        text=True,
        cwd=Path.cwd(),
    )
    if completed.returncode != 0:
        raise ValueError(completed.stderr.strip() or "git diff 执行失败")

    changed_paths = {
        (Path.cwd() / line.strip()).resolve()
        for line in completed.stdout.splitlines()
        if line.strip()
    }
    return [path for path in candidate_files if path.resolve() in changed_paths]


def _constraints_failures(run: RunModel, constraints: dict[str, float]) -> list[str]:
    failures: list[str] = []
    for key, threshold in constraints.items():
        if key.startswith("max_"):
            metric_name = key.removeprefix("max_")
            metric_value = _metric_value_for_run(run, metric_name)
            if metric_value > threshold:
                failures.append(f"{metric_name}={metric_value:.4f} > {threshold:.4f}")
            continue

        metric_value = _metric_value_for_run(run, key)
        if metric_value < threshold:
            failures.append(f"{key}={metric_value:.4f} < {threshold:.4f}")
    return failures


def _select_compatible_runs(seed_run: RunModel, all_runs: list[RunModel]) -> list[RunModel]:
    compatible_runs: list[RunModel] = []
    for run in all_runs:
        if run.status != "completed":
            continue
        if run.task_id != seed_run.task_id or run.split != seed_run.split:
            continue
        if seed_run.dataset_path is not None and run.dataset_path != seed_run.dataset_path:
            continue
        if seed_run.model_name is not None and run.model_name != seed_run.model_name:
            continue
        compatible_runs.append(run)
    return compatible_runs

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
    quiet: bool = typer.Option(False, "--quiet", help="禁止控制台富文本输出"),
    output_json: bool = typer.Option(False, "--output-json", help="输出 JSON 到 stdout"),
    report_file: Path | None = typer.Option(None, "--report-file", help="导出 markdown/html 报告文件"),
    report_format: str = typer.Option("markdown", "--report-format", help="报告格式：markdown 或 html"),
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
    prompt_diff_text: str | None = None

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
            lineage = session.get(LineageModel, run.candidate_id)
            if lineage is not None and lineage.diff:
                prompt_diff_text = lineage.diff
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
                baseline_candidate = session.get(CandidateModel, baseline.candidate_id)
                candidate_model = session.get(CandidateModel, run.candidate_id)
                if baseline_candidate is not None and candidate_model is not None:
                    prompt_diff_text = _build_prompt_diff_text(
                        baseline.candidate_id,
                        baseline_candidate.prompt,
                        run.candidate_id,
                        candidate_model.prompt,
                    )
            except ValueError as exc:
                console.print(f"[red]✗ 无法比较 runs:[/red] {exc}")
                raise typer.Exit(code=1) from exc

    if isinstance(report, DiagnosticsReport):
        payload = _build_diagnostics_payload(report, prompt_diff_text=prompt_diff_text)

        def render() -> None:
            _render_diagnostics_report(report)
            if prompt_diff_text is not None:
                _render_prompt_diff(prompt_diff_text)

    else:
        payload = _build_baseline_diff_payload(report, prompt_diff_text=prompt_diff_text)

        def render() -> None:
            _render_baseline_diff_report(report)
            if prompt_diff_text is not None:
                _render_prompt_diff(prompt_diff_text)

    _emit_structured_output(
        title=f"diagnose_{run_id}",
        payload=payload,
        quiet=quiet,
        output_json=output_json,
        report_file=report_file,
        report_format=report_format,
        rich_renderer=render,
    )

    if export_failures is not None:
        if not isinstance(report, DiagnosticsReport):
            console.print("[red]✗ baseline diff 模式暂不支持 --export-failures。[/red]")
            raise typer.Exit(code=1)
        analyzer.export_failures(report.failures, export_failures)
        console.print(f"[green]✓[/green] 失败样本已导出到: {export_failures}")


@app.command()
def optimize(
    run_id: str = typer.Argument(..., help="Run ID"),
    teacher: str | None = typer.Option(None, "--teacher", help="Teacher 模型，默认读取 .promptopt.yaml"),
    strategies: str = typer.Option("rewrite", "--strategies", help="优化策略"),
    num_candidates: int = typer.Option(12, "--num-candidates", help="生成候选数量"),
) -> None:
    """生成候选 Prompt 优化."""
    raw_strategy_list = _normalize_strategies(strategies)
    strategy_list = _coerce_optimizer_strategies(raw_strategy_list)
    unsupported_strategies = [
        item for item in raw_strategy_list if item not in {"rewrite", "fewshot", "contract"}
    ]
    if not strategy_list:
        console.print("[red]✗ 请至少提供一个优化策略。[/red]")
        raise typer.Exit(code=1)
    if unsupported_strategies:
        console.print(
            "[red]✗ 存在不支持的优化策略: "
            f"{', '.join(unsupported_strategies)}[/red]"
        )
        raise typer.Exit(code=1)
    analyzer = DiagnosticsAnalyzer()

    run, sample_results, project_config = _load_run_and_samples_or_exit(run_id)
    report = analyzer.analyze_run(run, sample_results, top_k=5)
    try:
        _task_path, candidate_path, task_spec, parent_candidate = _load_task_candidate_context_from_run(
            run,
            project_config,
        )
    except (FileNotFoundError, ValueError) as exc:
        console.print(f"[red]✗ 无法恢复优化上下文:[/red] {exc}")
        raise typer.Exit(code=1) from exc
    try:
        teacher_adapter = build_teacher_model_adapter(
            project_config,
            teacher_model=teacher,
        )
    except ValueError as exc:
        console.print(f"[red]✗ 无法解析 Teacher 模型:[/red] {exc}")
        raise typer.Exit(code=1) from exc

    eval_payload = _build_optimize_eval_payload(report)
    per_strategy_target = max(1, (num_candidates + len(strategy_list) - 1) // len(strategy_list))
    generated_candidates: list[tuple[OptimizerStrategy, Candidate]] = []
    seen_prompts: set[str] = set()

    for strategy in strategy_list:
        optimizer = _instantiate_optimizer(strategy)
        generated_prompts = optimizer.optimize(
            current_prompt=parent_candidate.prompt,
            eval_results=eval_payload,
            task_description=task_spec.description,
            **_build_generation_kwargs(
                strategy=strategy,
                teacher_adapter=teacher_adapter,
                num_candidates=per_strategy_target,
                sample_results=sample_results,
                task_spec=task_spec,
            ),
        )
        for prompt_text in generated_prompts:
            normalized_prompt = prompt_text.strip()
            if not normalized_prompt or normalized_prompt in seen_prompts:
                continue
            seen_prompts.add(normalized_prompt)
            index = len([item for item in generated_candidates if item[0] == strategy]) + 1
            candidate_id = f"{parent_candidate.id}_{strategy}_{index:02d}"
            generated_candidate = Candidate(
                id=candidate_id,
                name=f"{parent_candidate.name}_{strategy}_{index:02d}",
                description=f"{strategy} candidate generated from run {run_id}",
                prompt=normalized_prompt,
                metadata=CandidateMetadata(
                    strategy=strategy,
                    parent_id=parent_candidate.id,
                    teacher_model=teacher_adapter.model_name if strategy == "rewrite" else None,
                    generation_params={
                        "source_run_id": run_id,
                        "strategy": strategy,
                        "num_candidates": num_candidates,
                    },
                ),
            )
            generated_candidates.append((strategy, generated_candidate))
            if len(generated_candidates) >= num_candidates:
                break
        if len(generated_candidates) >= num_candidates:
            break

    if not generated_candidates:
        console.print("[red]✗ 未生成任何候选 prompt。[/red]")
        raise typer.Exit(code=1)

    output_table = Table(title=f"优化结果 · {run_id}")
    output_table.add_column("Candidate ID", style="cyan")
    output_table.add_column("Strategy", style="magenta")
    output_table.add_column("Name", style="green")
    output_table.add_column("File", style="yellow")

    generated_files: list[Path] = []
    for strategy, generated_candidate in generated_candidates:
        output_path = _build_candidate_file_path(candidate_path.parent, generated_candidate.id)
        _write_candidate_yaml(generated_candidate, output_path)
        generated_files.append(output_path)
        output_table.add_row(generated_candidate.id, strategy, generated_candidate.name, str(output_path))

    console.print("[blue]优化配置:[/blue]")
    console.print(f"  Run: {run_id}")
    console.print(f"  Teacher: {teacher_adapter.model_name}")
    console.print(f"  Strategies: {', '.join(strategy_list)}")
    console.print(f"  Num Candidates: {len(generated_files)}")
    console.print(output_table)


@app.command()
def search(
    candidates_dir: Path = typer.Argument(..., help="候选配置目录"),
    task: Path = typer.Option(..., "--task", "-t", help="任务配置路径"),
    dataset: Path = typer.Option(..., "--dataset", "-d", help="数据集配置路径"),
    split: str = typer.Option("dev", "--split", "-s", help="数据集划分"),
    changed_only: bool = typer.Option(False, "--changed-only", help="仅评估 Git diff 里的候选文件"),
    git_base_ref: str = typer.Option("main", "--git-base-ref", help="Git diff 对比基准分支/引用"),
    quiet: bool = typer.Option(False, "--quiet", help="禁止控制台富文本输出"),
    output_json: bool = typer.Option(False, "--output-json", help="输出 JSON 到 stdout"),
) -> None:
    """批量评估候选."""
    split_value = _parse_split(split)
    candidate_files = _collect_candidate_files(candidates_dir)
    if changed_only:
        try:
            candidate_files = _filter_candidate_files_by_git_diff(
                candidate_files,
                candidates_dir=candidates_dir,
                git_base_ref=git_base_ref,
            )
        except ValueError as exc:
            console.print(f"[red]✗ 无法读取 Git diff:[/red] {exc}")
            raise typer.Exit(code=1) from exc
    if not candidate_files:
        console.print(f"[red]✗ 未在目录中找到候选 YAML:[/red] {candidates_dir}")
        raise typer.Exit(code=1)

    project_config = _resolve_project_config_or_exit([candidates_dir, task, dataset])
    task_spec = Task.from_yaml(task)
    dataset_loader = DatasetLoader(path=str(dataset), split_field=task_spec.dataset.split_field)
    engine = EvaluationEngine(
        adapter=build_model_adapter(project_config),
        evaluators=build_evaluators(task_spec.evaluation_metrics),
        db=get_db(project_config.db_path),
        timeout=project_config.timeout,
    )

    result_table = Table(title=f"批量评估 · {candidates_dir}")
    result_table.add_column("Run ID", style="cyan")
    result_table.add_column("Candidate", style="green")
    result_table.add_column("Accuracy", justify="right")
    result_table.add_column("Metrics", style="yellow")
    payload_rows: list[dict[str, object]] = []

    for candidate_file in candidate_files:
        candidate_spec = Candidate.from_yaml(candidate_file)
        result = engine.run(
            task=task_spec,
            candidate=candidate_spec,
            dataset=dataset_loader,
            split=split_value,
            task_path=task,
            candidate_path=candidate_file,
            dataset_path=dataset,
        )
        metrics_display = ", ".join(
            f"{name}={value:.3f}" for name, value in sorted(result.aggregate_metrics.items())
        )
        result_table.add_row(
            result.run_id,
            candidate_spec.id,
            f"{result.accuracy:.2%}",
            metrics_display or "-",
        )
        payload_rows.append(
            {
                "run_id": result.run_id,
                "candidate_id": candidate_spec.id,
                "candidate_file": str(candidate_file),
                "accuracy": result.accuracy,
                "metrics": result.aggregate_metrics,
            }
        )

    def render() -> None:
        console.print("[blue]批量评估:[/blue]")
        console.print(f"  Candidates: {candidates_dir}")
        console.print(f"  Task: {task}")
        console.print(f"  Dataset: {dataset}")
        console.print(result_table)

    _emit_structured_output(
        title=f"search_{candidates_dir.name}",
        payload=_build_search_payload(payload_rows),
        quiet=quiet,
        output_json=output_json,
        report_file=None,
        report_format="markdown",
        rich_renderer=render,
    )


@app.command()
def select(
    run_id: str = typer.Argument(..., help="Run ID"),
    primary: str = typer.Option("accuracy", "--primary", help="主要指标"),
    secondary: str | None = typer.Option(None, "--secondary", help="次要指标(逗号分隔)"),
    constraints: str | None = typer.Option(None, "--constraints", help="约束条件，如 json_validity=1.0,max_latency=5000"),
    quiet: bool = typer.Option(False, "--quiet", help="禁止控制台富文本输出"),
    output_json: bool = typer.Option(False, "--output-json", help="输出 JSON 到 stdout"),
) -> None:
    """选择最优候选."""
    project_config = discover_project_config([Path.cwd()])
    db = get_db(project_config.db_path if project_config else None)
    secondary_metrics = _parse_secondary_metrics(secondary)
    effective_constraints = _merge_constraints(project_config, constraints) if project_config else _parse_constraints(constraints)

    with db.session() as session:
        seed_run = session.get(RunModel, run_id)
        if seed_run is None:
            console.print(f"[red]✗ 未找到 Run:[/red] {run_id}")
            raise typer.Exit(code=1)

        all_runs = session.query(RunModel).order_by(RunModel.created_at.desc()).all()
        compatible_runs = _select_compatible_runs(seed_run, all_runs)
        if not compatible_runs:
            console.print("[red]✗ 未找到可比较的已完成 runs。[/red]")
            raise typer.Exit(code=1)

        constrained_runs = [
            run for run in compatible_runs if not _constraints_failures(run, effective_constraints)
        ]
        if not constrained_runs:
            console.print("[red]✗ 所有候选都未满足约束条件。[/red]")
            raise typer.Exit(code=1)

        selected_run = max(
            constrained_runs,
            key=lambda run: tuple(
                [_metric_value_for_run(run, primary)]
                + [_metric_value_for_run(run, metric_name) for metric_name in secondary_metrics]
            ),
        )
        selected_run_id = selected_run.id
        selected_candidate_id = selected_run.candidate_id

        table = Table(title=f"候选选择 · {seed_run.task_id}")
        table.add_column("Run ID", style="cyan")
        table.add_column("Candidate", style="green")
        table.add_column(primary, justify="right")
        if secondary_metrics:
            for metric_name in secondary_metrics:
                table.add_column(metric_name, justify="right")

        payload_rows: list[dict[str, object]] = []
        for run in constrained_runs:
            row = [run.id, run.candidate_id, f"{_metric_value_for_run(run, primary):.4f}"]
            payload_row: dict[str, object] = {
                "run_id": run.id,
                "candidate_id": run.candidate_id,
                primary: _metric_value_for_run(run, primary),
            }
            for metric_name in secondary_metrics:
                metric_value = _metric_value_for_run(run, metric_name)
                row.append(f"{metric_value:.4f}")
                payload_row[metric_name] = metric_value
            table.add_row(*row)
            payload_rows.append(payload_row)

    def render() -> None:
        console.print("[blue]选择最优候选:[/blue]")
        console.print(f"  Seed Run: {run_id}")
        console.print(f"  Primary: {primary}")
        if secondary_metrics:
            console.print(f"  Secondary: {', '.join(secondary_metrics)}")
        if effective_constraints:
            console.print(
                f"  Constraints: {', '.join(f'{key}={value}' for key, value in effective_constraints.items())}"
            )
        console.print(f"[green]✓[/green] 选中候选: {selected_candidate_id} ({selected_run_id})")
        console.print(table)

    _emit_structured_output(
        title=f"select_{run_id}",
        payload=_build_select_payload(
            seed_run_id=run_id,
            selected_run_id=selected_run_id,
            selected_candidate_id=selected_candidate_id,
            primary=primary,
            secondary_metrics=secondary_metrics,
            constraints=effective_constraints,
            rows=payload_rows,
        ),
        quiet=quiet,
        output_json=output_json,
        report_file=None,
        report_format="markdown",
        rich_renderer=render,
    )


@app.command()
def verify(
    run_id: str = typer.Argument(..., help="Run ID"),
    split: str = typer.Option("test", "--split", "-s", help="数据集划分"),
    baseline_run: str | None = typer.Option(None, "--baseline-run", help="用于回归检测的 baseline run ID"),
    constraints: str | None = typer.Option(None, "--constraints", help="约束条件，如 json_validity=1.0,max_latency=5000"),
    quiet: bool = typer.Option(False, "--quiet", help="禁止控制台富文本输出"),
    output_json: bool = typer.Option(False, "--output-json", help="输出 JSON 到 stdout"),
    report_file: Path | None = typer.Option(None, "--report-file", help="导出 markdown/html 报告文件"),
    report_format: str = typer.Option("markdown", "--report-format", help="报告格式：markdown 或 html"),
) -> None:
    """测试集验证."""
    split_value = _parse_split(split)
    run, _sample_results, project_config = _load_run_and_samples_or_exit(run_id)
    analyzer = DiagnosticsAnalyzer()
    effective_constraints = _merge_constraints(project_config, constraints)

    try:
        task_path, candidate_path, task_spec, candidate_spec = _load_task_candidate_context_from_run(
            run,
            project_config,
        )
        config_path = project_config.config_path
        if config_path is None:
            raise ValueError("未找到 .promptopt.yaml 路径。")
        dataset_path = _resolve_artifact_path(run.dataset_path, config_path.parent)
    except (FileNotFoundError, ValueError) as exc:
        console.print(f"[red]✗ 无法恢复验证上下文:[/red] {exc}")
        raise typer.Exit(code=1) from exc

    dataset_loader = DatasetLoader(path=str(dataset_path), split_field=task_spec.dataset.split_field)
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
        task_path=task_path,
        candidate_path=candidate_path,
        dataset_path=dataset_path,
    )

    gate_failures = _constraints_failures(
        RunModel(
            id=result.run_id,
            task_id=task_spec.name,
            candidate_id=candidate_spec.id,
            split=split_value.value,
            status="completed",
            accuracy=result.accuracy,
            aggregate_metrics_json=json.dumps(result.aggregate_metrics, ensure_ascii=False, sort_keys=True),
            cost=result.cost,
            latency_ms=result.latency_ms,
        ),
        effective_constraints,
    )
    regression_messages: list[str] = []
    baseline_payload: dict[str, object] | None = None

    if baseline_run is not None:
        db = get_db(project_config.db_path)
        with db.session() as session:
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
            verify_run = session.get(RunModel, result.run_id)
            if verify_run is None:
                raise ValueError(f"Verify run not found: {result.run_id}")
            verify_sample_results = (
                session.query(SampleResultModel)
                .filter(SampleResultModel.run_id == result.run_id)
                .order_by(SampleResultModel.sample_id.asc())
                .all()
            )

        diff_report = analyzer.compare_runs(
            baseline,
            baseline_sample_results,
            verify_run,
            verify_sample_results,
            top_k=max(len(verify_sample_results), 1),
        )
        regressed_slices = analyzer.detect_slice_regressions(
            baseline_sample_results,
            verify_sample_results,
        )
        regression_count = diff_report.still_failed + len(diff_report.regressions)
        if diff_report.regressions:
            regression_messages.append(
                f"出现 {len(diff_report.regressions)} 个退化样本"
            )
        if regressed_slices:
            regression_messages.append(
                "关键 slice 退化: "
                + ", ".join(
                    f"{slice_name}({baseline_accuracy:.2%}->{candidate_accuracy:.2%})"
                    for slice_name, (baseline_accuracy, candidate_accuracy) in regressed_slices.items()
                )
            )
        if regression_count:
            console.print(f"[yellow]⚠[/yellow] Regression summary: {regression_count} 个失败/退化样本")
        baseline_payload = _build_baseline_diff_payload(diff_report, prompt_diff_text=None)

    summary_table = Table(title=f"验证结果 · {result.run_id}")
    summary_table.add_column("字段", style="cyan")
    summary_table.add_column("值", style="green")
    summary_table.add_row("Task", task_spec.name)
    summary_table.add_row("Candidate", candidate_spec.id)
    summary_table.add_row("Model", engine.model_name)
    summary_table.add_row("Split", split_value.value)
    summary_table.add_row("Samples", str(result.total_samples))
    summary_table.add_row("Accuracy", f"{result.accuracy:.2%}")
    summary_table.add_row("Latency", f"{result.latency_ms:.2f} ms/sample")

    metrics_table = Table(title="验证指标")
    metrics_table.add_column("指标", style="magenta")
    metrics_table.add_column("值", style="yellow", justify="right")
    for metric_name, metric_value in sorted(result.aggregate_metrics.items()):
        metrics_table.add_row(metric_name, f"{metric_value:.4f}")

    payload = _build_verify_payload(
        source_run_id=run_id,
        verify_run_id=result.run_id,
        split=split_value.value,
        result={
            "task": task_spec.name,
            "candidate": candidate_spec.id,
            "model": engine.model_name,
            "samples": result.total_samples,
            "accuracy": result.accuracy,
            "latency_ms": result.latency_ms,
            "aggregate_metrics": result.aggregate_metrics,
        },
        constraints=effective_constraints,
        gate_failures=gate_failures,
        regression_failures=regression_messages,
        baseline_diff=baseline_payload,
    )

    def render() -> None:
        console.print("[blue]测试集验证:[/blue]")
        console.print(f"  Source Run: {run_id}")
        console.print(f"  Verify Split: {split_value.value}")
        if effective_constraints:
            console.print(
                f"  Constraints: {', '.join(f'{key}={value}' for key, value in effective_constraints.items())}"
            )
        console.print(summary_table)
        if result.aggregate_metrics:
            console.print(metrics_table)
        if baseline_payload is not None and baseline_run is not None:
            _render_baseline_diff_report(diff_report)
        if gate_failures or regression_messages:
            console.print("[red]✗ Verify gate 未通过:[/red]")
            for failure in gate_failures:
                console.print(f"- Constraint failure: {failure}")
            for message in regression_messages:
                console.print(f"- Regression failure: {message}")
        else:
            console.print(f"[green]✓[/green] Verify Run 已保存: {result.run_id}")

    _emit_structured_output(
        title=f"verify_{result.run_id}",
        payload=payload,
        quiet=quiet,
        output_json=output_json,
        report_file=report_file,
        report_format=report_format,
        rich_renderer=render,
    )

    if gate_failures or regression_messages:
        raise typer.Exit(code=2)


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
def rollback(
    candidate_id: str = typer.Argument(..., help="历史 Candidate ID"),
    output: Path | None = typer.Option(None, "--output", "-o", help="回滚候选输出路径或目录"),
) -> None:
    """导出历史 candidate，生成弱回滚 YAML 工件。"""
    project_config = discover_project_config([Path.cwd()])
    db = get_db(project_config.db_path if project_config else None)

    with db.session() as session:
        candidate_model = session.get(CandidateModel, candidate_id)
        if candidate_model is None:
            console.print(f"[red]✗ 未找到 Candidate:[/red] {candidate_id}")
            raise typer.Exit(code=1)

    rollback_candidate = Candidate(
        id=f"{candidate_id}_rollback",
        name=f"{candidate_model.name}_rollback",
        description=f"Rollback candidate restored from {candidate_id}",
        prompt=candidate_model.prompt,
        metadata=CandidateMetadata(
            strategy="baseline",
            parent_id=candidate_id,
            generation_params={"rollback_source": candidate_id},
        ),
    )

    if output is None:
        output_path = _build_candidate_file_path(Path.cwd() / "candidates", rollback_candidate.id)
    elif output.suffix.lower() in {".yaml", ".yml"}:
        output_path = output
    else:
        output_path = _build_candidate_file_path(output, rollback_candidate.id)

    _write_candidate_yaml(rollback_candidate, output_path)
    console.print(f"[green]✓[/green] 已导出回滚候选: {output_path}")


@app.command()
def version() -> None:
    """显示版本信息."""
    from promptopt import __version__
    console.print(f"PromptOpt v{__version__}")


if __name__ == "__main__":
    app()
