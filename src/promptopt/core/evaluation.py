"""Evaluation engine and project configuration helpers."""

from __future__ import annotations

import asyncio
import difflib
import json
import time
import uuid
from collections.abc import Sequence
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path

import yaml
from sqlalchemy.orm import Session

from promptopt.core.candidate import Candidate
from promptopt.core.dataset import DatasetLoader, Sample
from promptopt.core.run import EvalResult, RunResult
from promptopt.core.task import Split, Task
from promptopt.evaluators import Evaluator
from promptopt.models import ModelAdapter
from promptopt.plugins import get_evaluator_registry, resolve_provider
from promptopt.storage.database import Database, get_db
from promptopt.storage.models import (
    CandidateModel,
    LineageModel,
    RunModel,
    SampleResultModel,
)


@dataclass(slots=True)
class ProjectConfig:
    """Runtime configuration discovered from ``.promptopt.yaml``."""

    target_model: str
    target_base_url: str | None = None
    teacher_model: str | None = None
    teacher_base_url: str | None = None
    local_model_base_urls: dict[str, str] = field(default_factory=dict)
    constraints: dict[str, float] = field(default_factory=dict)
    db_path: str | None = None
    batch_size: int = 1
    max_workers: int = 1
    timeout: int | None = None
    config_path: Path | None = None

    @classmethod
    def from_file(cls, path: Path) -> ProjectConfig:
        """Load project configuration from a YAML file."""
        with open(path, encoding="utf-8") as file:
            loaded: object = yaml.safe_load(file)

        if loaded is None:
            loaded = {}
        if not isinstance(loaded, dict):
            raise ValueError(".promptopt.yaml 必须在根节点提供映射配置。")

        models_raw = loaded.get("models", {})
        if not isinstance(models_raw, dict):
            raise ValueError(".promptopt.yaml 中的 models 必须是映射。")

        target_model_raw = models_raw.get("target")
        if not isinstance(target_model_raw, str) or not target_model_raw.strip():
            raise ValueError(".promptopt.yaml 必须提供 models.target。")
        target_model = normalize_model_name(target_model_raw.strip())

        teacher_model_raw = models_raw.get("teacher")
        teacher_model = None
        if isinstance(teacher_model_raw, str) and teacher_model_raw.strip():
            teacher_model = normalize_model_name(teacher_model_raw.strip())

        local_model_base_urls = _collect_local_model_base_urls(models_raw)
        target_base_url = local_model_base_urls.get(target_model)
        teacher_base_url = (
            local_model_base_urls.get(teacher_model) if teacher_model is not None else None
        )

        storage_raw = loaded.get("storage", {})
        db_path = None
        if isinstance(storage_raw, dict):
            db_path_raw = storage_raw.get("db_path")
            if isinstance(db_path_raw, str) and db_path_raw.strip():
                db_path = _resolve_config_path(path.parent, db_path_raw.strip())

        constraints_raw = loaded.get("constraints", {})
        constraints = _collect_numeric_constraints(constraints_raw)

        evaluation_raw = loaded.get("evaluation", {})
        batch_size = 1
        max_workers = 1
        timeout = None
        if isinstance(evaluation_raw, dict):
            batch_size = _coerce_int(evaluation_raw.get("batch_size"), default=1)
            max_workers = _coerce_int(evaluation_raw.get("max_workers"), default=1)
            timeout_value = evaluation_raw.get("timeout")
            timeout = _coerce_int(timeout_value, default=0) or None

        return cls(
            target_model=target_model,
            target_base_url=target_base_url,
            teacher_model=teacher_model,
            teacher_base_url=teacher_base_url,
            local_model_base_urls=local_model_base_urls,
            constraints=constraints,
            db_path=db_path,
            batch_size=batch_size,
            max_workers=max_workers,
            timeout=timeout,
            config_path=path,
        )


def discover_project_config(start_paths: Sequence[Path] | None = None) -> ProjectConfig | None:
    """Discover the nearest ``.promptopt.yaml`` from the provided paths."""
    candidate_dirs: list[Path] = []
    seen: set[Path] = set()

    search_roots = list(start_paths or [])
    search_roots.append(Path.cwd())

    for raw_path in search_roots:
        resolved = raw_path.expanduser().resolve()
        base_dir = resolved.parent if resolved.is_file() else resolved
        for directory in (base_dir, *base_dir.parents):
            if directory not in seen:
                seen.add(directory)
                candidate_dirs.append(directory)

    for directory in candidate_dirs:
        config_path = directory / ".promptopt.yaml"
        if config_path.exists():
            return ProjectConfig.from_file(config_path)

    return None


class EvaluationEngine:
    """Run prompt evaluation against a dataset and persist the results."""

    def __init__(
        self,
        adapter: ModelAdapter,
        evaluators: Sequence[Evaluator],
        db: Database | None = None,
        timeout: int | None = None,
    ) -> None:
        self._adapter = adapter
        self._evaluators = list(evaluators)
        self._db = db
        self._timeout = timeout

    @property
    def model_name(self) -> str:
        """Return the active target model name."""
        return self._adapter.model_name

    def run(
        self,
        task: Task,
        candidate: Candidate,
        dataset: Sequence[Sample] | DatasetLoader,
        *,
        split: Split,
        task_path: Path | None = None,
        candidate_path: Path | None = None,
        dataset_path: Path | None = None,
    ) -> RunResult:
        """Execute the evaluation and persist the final artifacts."""
        samples = list(dataset.load(split=split) if isinstance(dataset, DatasetLoader) else dataset)
        started_at = datetime.now()
        run_id = _generate_run_id()
        result = RunResult(
            candidate_id=candidate.id,
            run_id=run_id,
            total_samples=len(samples),
            started_at=started_at,
        )

        db = self._db or get_db()
        self._create_run_record(
            db=db,
            run_id=run_id,
            task=task,
            candidate=candidate,
            split=split,
            started_at=started_at,
            task_path=task_path,
            candidate_path=candidate_path,
            dataset_path=dataset_path,
        )

        try:
            sample_results, aggregate_metrics, correct_count, average_latency = asyncio.run(
                self._evaluate_samples(candidate=candidate, samples=samples)
            )
        except Exception as exc:
            completed_at = datetime.now()
            result.completed_at = completed_at
            result.duration_seconds = (completed_at - started_at).total_seconds()
            self._mark_run_failed(db=db, run_id=run_id, error=str(exc), completed_at=completed_at)
            raise

        completed_at = datetime.now()
        result.correct_count = correct_count
        result.aggregate_metrics = aggregate_metrics
        result.sample_results = sample_results
        result.completed_at = completed_at
        result.duration_seconds = (completed_at - started_at).total_seconds()
        result.latency_ms = average_latency

        self._persist_completed_run(
            db=db,
            result=result,
            task=task,
            split=split,
            completed_at=completed_at,
            task_path=task_path,
            dataset_path=dataset_path,
        )
        return result

    async def _evaluate_samples(
        self,
        *,
        candidate: Candidate,
        samples: Sequence[Sample],
    ) -> tuple[list[EvalResult], dict[str, float], int, float]:
        metric_totals: dict[str, float] = {}
        sample_results: list[EvalResult] = []
        correct_count = 0
        latency_sum = 0.0

        for sample in samples:
            prompt = render_prompt(candidate.prompt, sample.input)
            started = time.perf_counter()

            try:
                actual_output = await self._generate(prompt)
                elapsed_ms = (time.perf_counter() - started) * 1000
                latency_sum += elapsed_ms

                evaluator_flags: list[bool] = []
                metrics: dict[str, float] = {}
                for evaluator in self._evaluators:
                    is_correct, evaluator_metrics = evaluator.evaluate(sample.expected, actual_output)
                    evaluator_flags.append(is_correct)
                    metrics.update(evaluator_metrics)

                is_sample_correct = all(evaluator_flags) if evaluator_flags else False
                if is_sample_correct:
                    correct_count += 1

                for metric_name, metric_value in metrics.items():
                    metric_totals[metric_name] = metric_totals.get(metric_name, 0.0) + metric_value

                sample_results.append(
                    EvalResult(
                        sample_id=sample.id,
                        input_text=sample.input,
                        expected_output=sample.expected,
                        actual_output=actual_output,
                        is_correct=is_sample_correct,
                        metrics=metrics,
                    )
                )
            except Exception as exc:
                latency_sum += (time.perf_counter() - started) * 1000
                sample_results.append(
                    EvalResult(
                        sample_id=sample.id,
                        input_text=sample.input,
                        expected_output=sample.expected,
                        actual_output="",
                        error=str(exc),
                    )
                )

        sample_count = len(sample_results)
        aggregate_metrics = {
            name: total / sample_count for name, total in metric_totals.items()
        } if sample_count else {}
        average_latency = latency_sum / sample_count if sample_count else 0.0
        return sample_results, aggregate_metrics, correct_count, average_latency

    async def _generate(self, prompt: str) -> str:
        if self._timeout is None:
            return await self._adapter.generate(prompt)
        return await asyncio.wait_for(self._adapter.generate(prompt), timeout=self._timeout)

    def _create_run_record(
        self,
        *,
        db: Database,
        run_id: str,
        task: Task,
        candidate: Candidate,
        split: Split,
        started_at: datetime,
        task_path: Path | None,
        candidate_path: Path | None,
        dataset_path: Path | None,
    ) -> None:
        with db.session() as session:
            candidate_model = session.get(CandidateModel, candidate.id)
            if candidate_model is None:
                candidate_model = CandidateModel(
                    id=candidate.id,
                    name=candidate.name,
                    prompt=candidate.prompt,
                    description=candidate.description,
                    strategy=candidate.metadata.strategy,
                    parent_id=candidate.metadata.parent_id,
                    teacher_model=candidate.metadata.teacher_model,
                    created_at=candidate.created_at,
                )
                session.add(candidate_model)
            else:
                candidate_model.name = candidate.name
                candidate_model.prompt = candidate.prompt
                candidate_model.description = candidate.description
                candidate_model.strategy = candidate.metadata.strategy
                candidate_model.parent_id = candidate.metadata.parent_id
                candidate_model.teacher_model = candidate.metadata.teacher_model

            self._upsert_lineage_record(session=session, candidate=candidate)

            session.add(
                RunModel(
                    id=run_id,
                    task_id=task.name,
                    task_path=str(task_path) if task_path else None,
                    candidate_id=candidate.id,
                    candidate_path=str(candidate_path) if candidate_path else None,
                    dataset_path=str(dataset_path) if dataset_path else None,
                    model_name=self._adapter.model_name,
                    split=split.value,
                    status="running",
                    total_samples=0,
                    correct_count=0,
                    accuracy=0.0,
                    aggregate_metrics_json="{}",
                    duration_seconds=0.0,
                    cost=0.0,
                    latency_ms=0.0,
                    created_at=started_at,
                )
            )

    def _persist_completed_run(
        self,
        *,
        db: Database,
        result: RunResult,
        task: Task,
        split: Split,
        completed_at: datetime,
        task_path: Path | None,
        dataset_path: Path | None,
    ) -> None:
        with db.session() as session:
            run = session.get(RunModel, result.run_id)
            if run is None:
                raise ValueError(f"Run record not found: {result.run_id}")

            run.task_id = task.name
            run.task_path = str(task_path) if task_path else run.task_path
            run.dataset_path = str(dataset_path) if dataset_path else run.dataset_path
            run.model_name = self._adapter.model_name
            run.split = split.value
            run.status = "completed"
            run.total_samples = result.total_samples
            run.correct_count = result.correct_count
            run.accuracy = result.accuracy
            run.aggregate_metrics_json = _serialize_json(result.aggregate_metrics)
            run.duration_seconds = result.duration_seconds
            run.cost = result.cost
            run.latency_ms = result.latency_ms
            run.error = None
            run.completed_at = completed_at

            for sample_result in result.sample_results:
                session.add(
                    SampleResultModel(
                        run_id=result.run_id,
                        sample_id=sample_result.sample_id,
                        input_text=sample_result.input_text,
                        expected_output=_serialize_output(sample_result.expected_output),
                        actual_output=_serialize_output(sample_result.actual_output),
                        is_correct=sample_result.is_correct,
                        metrics_json=_serialize_json(sample_result.metrics),
                        error=sample_result.error,
                    )
                )

    def _upsert_lineage_record(self, *, session: Session, candidate: Candidate) -> None:
        lineage = session.get(LineageModel, candidate.id)
        parent_id = candidate.metadata.parent_id
        change_type = candidate.metadata.strategy

        ancestors: list[str] = []
        prompt_diff: str | None = None

        if parent_id is not None:
            parent_lineage = session.get(LineageModel, parent_id)
            if parent_lineage is not None:
                try:
                    parsed_ancestors: object = json.loads(parent_lineage.ancestors)
                except json.JSONDecodeError:
                    parsed_ancestors = []
                if isinstance(parsed_ancestors, list):
                    ancestors.extend(
                        ancestor for ancestor in parsed_ancestors if isinstance(ancestor, str)
                    )
            if parent_id not in ancestors:
                ancestors.append(parent_id)

            parent_candidate = session.get(CandidateModel, parent_id)
            if parent_candidate is not None:
                diff_lines = difflib.unified_diff(
                    parent_candidate.prompt.splitlines(),
                    candidate.prompt.splitlines(),
                    fromfile=parent_id,
                    tofile=candidate.id,
                    lineterm="",
                )
                prompt_diff = "\n".join(diff_lines)

        if lineage is None:
            session.add(
                LineageModel(
                    candidate_id=candidate.id,
                    ancestors=json.dumps(ancestors, ensure_ascii=False),
                    parent_id=parent_id,
                    change_type=change_type,
                    diff=prompt_diff,
                )
            )
            return

        lineage.ancestors = json.dumps(ancestors, ensure_ascii=False)
        lineage.parent_id = parent_id
        lineage.change_type = change_type
        lineage.diff = prompt_diff

    def _mark_run_failed(
        self,
        *,
        db: Database,
        run_id: str,
        error: str,
        completed_at: datetime,
    ) -> None:
        with db.session() as session:
            run = session.get(RunModel, run_id)
            if run is None:
                return
            run.status = "failed"
            run.error = error
            run.completed_at = completed_at
            run.duration_seconds = (completed_at - run.created_at).total_seconds()


def build_evaluators(metric_names: Sequence[str]) -> list[Evaluator]:
    """Build evaluator instances from metric names."""
    registry = get_evaluator_registry()
    aliases = {
        "exact_match": "exact_match",
        "f1": "f1",
        "macro_f1": "f1",
        "json_validator": "json_validity",
        "json_validity": "json_validity",
    }

    evaluators: list[Evaluator] = []
    seen: set[str] = set()
    for metric_name in metric_names:
        canonical_name = aliases.get(metric_name, metric_name)
        if canonical_name not in registry:
            raise ValueError(f"不支持的评估指标: {metric_name}")
        if canonical_name in seen:
            continue
        evaluator_cls = registry[canonical_name]
        evaluator = evaluator_cls()
        if not isinstance(evaluator, Evaluator):
            raise ValueError(f"插件 `{canonical_name}` 未返回有效的 Evaluator 实例。")
        evaluators.append(evaluator)
        seen.add(canonical_name)
    return evaluators


def build_model_adapter(
    config: ProjectConfig,
    *,
    model_name: str | None = None,
    base_url: str | None = None,
) -> ModelAdapter:
    """Build a model adapter from project config."""
    resolved_model = config.target_model if model_name is None else normalize_model_name(model_name)
    resolved_base_url = (
        config.target_base_url if model_name is None else config.local_model_base_urls.get(resolved_model)
    )
    if base_url is not None:
        resolved_base_url = base_url
    provider = resolve_provider(resolved_model)
    return provider.build(
        resolved_model,
        base_url=resolved_base_url,
    )


def build_teacher_model_adapter(
    config: ProjectConfig,
    *,
    teacher_model: str | None = None,
) -> ModelAdapter:
    """Build the teacher model adapter used by optimize/search workflows."""
    resolved_teacher = config.teacher_model if teacher_model is None else normalize_model_name(teacher_model)
    if resolved_teacher is None:
        raise ValueError("未配置 teacher 模型，请在 .promptopt.yaml 中设置 models.teacher 或显式传入 --teacher。")

    resolved_base_url = config.local_model_base_urls.get(resolved_teacher)
    if teacher_model is None:
        resolved_base_url = config.teacher_base_url
    return build_model_adapter(
        config,
        model_name=resolved_teacher,
        base_url=resolved_base_url,
    )


def normalize_model_name(raw_model: str) -> str:
    """Normalize provider:model strings to LiteLLM provider/model strings."""
    if "://" in raw_model or "/" in raw_model:
        return raw_model
    if ":" in raw_model:
        provider, model = raw_model.split(":", 1)
        return f"{provider}/{model}"
    return raw_model


def render_prompt(template: str, input_text: str) -> str:
    """Safely render the ``{input}`` placeholder without breaking JSON braces."""
    return template.replace("{input}", input_text)


def _collect_local_model_base_urls(models_raw: dict[str, object]) -> dict[str, str]:
    local_models = models_raw.get("local")
    if not isinstance(local_models, list):
        return {}

    base_urls: dict[str, str] = {}

    for item in local_models:
        if not isinstance(item, dict):
            continue
        name_raw = item.get("name")
        if not isinstance(name_raw, str):
            continue
        base_url_raw = item.get("base_url")
        if isinstance(base_url_raw, str) and base_url_raw.strip():
            base_urls[normalize_model_name(name_raw)] = base_url_raw.strip()
    return base_urls


def _collect_numeric_constraints(raw_constraints: object) -> dict[str, float]:
    if not isinstance(raw_constraints, dict):
        return {}

    constraints: dict[str, float] = {}
    for key, value in raw_constraints.items():
        if not isinstance(key, str):
            continue
        if isinstance(value, bool):
            continue
        if isinstance(value, (int, float)):
            constraints[key] = float(value)
    return constraints


def _resolve_config_path(base_dir: Path, raw_path: str) -> str:
    candidate = Path(raw_path).expanduser()
    if not candidate.is_absolute():
        candidate = (base_dir / candidate).resolve()
    return str(candidate)


def _serialize_json(payload: dict[str, float] | dict[str, object]) -> str:
    return json.dumps(payload, ensure_ascii=False, sort_keys=True, default=str)


def _serialize_output(payload: str | dict[str, object]) -> str:
    if isinstance(payload, str):
        return payload
    return _serialize_json(payload)


def _generate_run_id() -> str:
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    return f"run_{timestamp}_{uuid.uuid4().hex[:8]}"


def _coerce_int(value: object, *, default: int) -> int:
    if isinstance(value, bool):
        return default
    if isinstance(value, int):
        return value
    if isinstance(value, str) and value.isdigit():
        return int(value)
    return default
