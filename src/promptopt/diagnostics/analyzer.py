"""Diagnostics analyzer for failure analysis."""

from __future__ import annotations

import json
import re
from collections.abc import Sequence
from dataclasses import asdict, dataclass
from enum import StrEnum
from pathlib import Path

from promptopt.storage.models import RunModel, SampleResultModel

NEGATION_KEYWORDS = ("无", "未", "否认", "不认", "并非")
NUMBER_PATTERN = re.compile(r"\d")


class FailureCategory(StrEnum):
    """High-level failure categories for diagnosis."""

    FORMAT_ERROR = "format_error"
    SEMANTIC_ERROR = "semantic_error"
    EXECUTION_ERROR = "execution_error"
    UNKNOWN = "unknown"


@dataclass(slots=True)
class FailureCase:
    """Structured representation for a failed sample."""

    sample_id: str
    input_text: str
    expected_output: str | dict[str, object]
    actual_output: str | dict[str, object]
    metrics: dict[str, float]
    error: str | None
    category: FailureCategory
    reason: str


@dataclass(slots=True)
class DiagnosticsReport:
    """Diagnostics summary for a run."""

    run_id: str
    task_id: str
    model_name: str | None
    total_samples: int
    failed_samples: int
    accuracy: float
    aggregate_metrics: dict[str, float]
    category_counts: dict[str, int]
    slice_metrics: dict[str, dict[str, float | int]]
    failures: list[FailureCase]
    top_failures: list[FailureCase]
    suggestions: list[str]


@dataclass(slots=True)
class SampleDiff:
    """Comparison result for a matched sample across two runs."""

    sample_id: str
    input_text: str
    expected_output: str | dict[str, object]
    baseline_actual_output: str | dict[str, object]
    candidate_actual_output: str | dict[str, object]
    baseline_is_correct: bool
    candidate_is_correct: bool
    baseline_metrics: dict[str, float]
    candidate_metrics: dict[str, float]
    metric_deltas: dict[str, float]
    baseline_error: str | None
    candidate_error: str | None
    baseline_failure: FailureCase | None
    candidate_failure: FailureCase | None


@dataclass(slots=True)
class BaselineDiffReport:
    """Comparison report between a baseline run and a candidate run."""

    baseline_run_id: str
    run_id: str
    task_id: str
    split: str
    baseline_candidate_id: str
    candidate_id: str
    baseline_model_name: str | None
    model_name: str | None
    matched_samples: int
    baseline_only_samples: list[str]
    candidate_only_samples: list[str]
    conflicted_samples: list[str]
    baseline_accuracy: float
    accuracy: float
    accuracy_delta: float
    aggregate_metric_deltas: dict[str, float]
    regressions: list[SampleDiff]
    improvements: list[SampleDiff]
    still_failed: int
    still_correct: int


class DiagnosticsAnalyzer:
    """Analyze persisted sample results and build actionable reports."""

    def analyze_run(
        self,
        run: RunModel,
        sample_results: Sequence[SampleResultModel],
        *,
        top_k: int = 5,
    ) -> DiagnosticsReport:
        """Analyze a persisted run and its sample results."""
        failures: list[FailureCase] = []
        for sample_result in sample_results:
            if sample_result.is_correct and not sample_result.error:
                continue
            failures.append(self._build_failure_case(sample_result))

        category_counts: dict[str, int] = {}
        for failure in failures:
            category_name = failure.category.value
            category_counts[category_name] = category_counts.get(category_name, 0) + 1

        slice_metrics = self._compute_slice_metrics(sample_results)
        aggregate_metrics = self._parse_metrics(run.aggregate_metrics_json)
        suggestions = self._build_suggestions(
            accuracy=run.accuracy,
            category_counts=category_counts,
            slice_metrics=slice_metrics,
        )

        return DiagnosticsReport(
            run_id=run.id,
            task_id=run.task_id,
            model_name=run.model_name,
            total_samples=len(sample_results),
            failed_samples=len(failures),
            accuracy=run.accuracy,
            aggregate_metrics=aggregate_metrics,
            category_counts=category_counts,
            slice_metrics=slice_metrics,
            failures=failures,
            top_failures=failures[:top_k],
            suggestions=suggestions,
        )

    def export_failures(self, failures: Sequence[FailureCase], output_path: Path) -> None:
        """Export all failure cases to a JSON file."""
        output_path.parent.mkdir(parents=True, exist_ok=True)
        payload = [self._failure_to_dict(failure) for failure in failures]
        output_path.write_text(
            json.dumps(payload, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )

    def compare_runs(
        self,
        baseline_run: RunModel,
        baseline_sample_results: Sequence[SampleResultModel],
        candidate_run: RunModel,
        candidate_sample_results: Sequence[SampleResultModel],
        *,
        top_k: int = 5,
    ) -> BaselineDiffReport:
        """Compare two completed runs and report regressions/improvements."""
        self._validate_run_compatibility(baseline_run, candidate_run)

        baseline_by_id = {sample.sample_id: sample for sample in baseline_sample_results}
        candidate_by_id = {sample.sample_id: sample for sample in candidate_sample_results}

        baseline_only_samples = sorted(
            sample_id for sample_id in baseline_by_id if sample_id not in candidate_by_id
        )
        candidate_only_samples = sorted(
            sample_id for sample_id in candidate_by_id if sample_id not in baseline_by_id
        )

        regressions: list[SampleDiff] = []
        improvements: list[SampleDiff] = []
        still_failed = 0
        still_correct = 0
        conflicted_samples: list[str] = []

        for sample_id in sorted(set(baseline_by_id) & set(candidate_by_id)):
            baseline_sample = baseline_by_id[sample_id]
            candidate_sample = candidate_by_id[sample_id]
            if self._samples_conflict(baseline_sample, candidate_sample):
                conflicted_samples.append(sample_id)
                continue

            sample_diff = self._build_sample_diff(baseline_sample, candidate_sample)
            if sample_diff.baseline_is_correct and not sample_diff.candidate_is_correct:
                regressions.append(sample_diff)
            elif not sample_diff.baseline_is_correct and sample_diff.candidate_is_correct:
                improvements.append(sample_diff)
            elif not sample_diff.baseline_is_correct and not sample_diff.candidate_is_correct:
                still_failed += 1
            else:
                still_correct += 1

        matched_samples = (
            len(set(baseline_by_id) & set(candidate_by_id)) - len(conflicted_samples)
        )

        return BaselineDiffReport(
            baseline_run_id=baseline_run.id,
            run_id=candidate_run.id,
            task_id=candidate_run.task_id,
            split=candidate_run.split,
            baseline_candidate_id=baseline_run.candidate_id,
            candidate_id=candidate_run.candidate_id,
            baseline_model_name=baseline_run.model_name,
            model_name=candidate_run.model_name,
            matched_samples=matched_samples,
            baseline_only_samples=baseline_only_samples,
            candidate_only_samples=candidate_only_samples,
            conflicted_samples=conflicted_samples,
            baseline_accuracy=baseline_run.accuracy,
            accuracy=candidate_run.accuracy,
            accuracy_delta=candidate_run.accuracy - baseline_run.accuracy,
            aggregate_metric_deltas=self._compute_metric_deltas(
                self._parse_metrics(baseline_run.aggregate_metrics_json),
                self._parse_metrics(candidate_run.aggregate_metrics_json),
            ),
            regressions=regressions[:top_k],
            improvements=improvements[:top_k],
            still_failed=still_failed,
            still_correct=still_correct,
        )

    def _build_failure_case(self, sample_result: SampleResultModel) -> FailureCase:
        metrics = self._parse_metrics(sample_result.metrics_json)
        expected_output = self._parse_output(sample_result.expected_output)
        actual_output = self._parse_output(sample_result.actual_output)
        category = self._classify_failure(sample_result=sample_result, metrics=metrics)
        reason = self._build_failure_reason(
            sample_result=sample_result,
            category=category,
            expected_output=expected_output,
            actual_output=actual_output,
        )
        return FailureCase(
            sample_id=sample_result.sample_id,
            input_text=sample_result.input_text,
            expected_output=expected_output,
            actual_output=actual_output,
            metrics=metrics,
            error=sample_result.error,
            category=category,
            reason=reason,
        )

    def _build_sample_diff(
        self,
        baseline_sample: SampleResultModel,
        candidate_sample: SampleResultModel,
    ) -> SampleDiff:
        expected_output = self._parse_output(candidate_sample.expected_output)
        baseline_metrics = self._parse_metrics(baseline_sample.metrics_json)
        candidate_metrics = self._parse_metrics(candidate_sample.metrics_json)
        baseline_failure = self._maybe_failure_case(baseline_sample)
        candidate_failure = self._maybe_failure_case(candidate_sample)
        return SampleDiff(
            sample_id=candidate_sample.sample_id,
            input_text=candidate_sample.input_text,
            expected_output=expected_output,
            baseline_actual_output=self._parse_output(baseline_sample.actual_output),
            candidate_actual_output=self._parse_output(candidate_sample.actual_output),
            baseline_is_correct=baseline_sample.is_correct,
            candidate_is_correct=candidate_sample.is_correct,
            baseline_metrics=baseline_metrics,
            candidate_metrics=candidate_metrics,
            metric_deltas=self._compute_metric_deltas(
                baseline_metrics,
                candidate_metrics,
            ),
            baseline_error=baseline_sample.error,
            candidate_error=candidate_sample.error,
            baseline_failure=baseline_failure,
            candidate_failure=candidate_failure,
        )

    def _maybe_failure_case(
        self,
        sample_result: SampleResultModel,
    ) -> FailureCase | None:
        if sample_result.is_correct and not sample_result.error:
            return None
        return self._build_failure_case(sample_result)

    def _validate_run_compatibility(
        self,
        baseline_run: RunModel,
        candidate_run: RunModel,
    ) -> None:
        if baseline_run.status != "completed" or candidate_run.status != "completed":
            raise ValueError("只有已完成的 runs 才能进行 baseline 对比。")
        if baseline_run.task_id != candidate_run.task_id:
            raise ValueError("baseline run 与 candidate run 的 task_id 不一致，无法比较。")
        if baseline_run.split != candidate_run.split:
            raise ValueError("baseline run 与 candidate run 的 split 不一致，无法比较。")

    def _samples_conflict(
        self,
        baseline_sample: SampleResultModel,
        candidate_sample: SampleResultModel,
    ) -> bool:
        if baseline_sample.input_text != candidate_sample.input_text:
            return True
        return self._parse_output(baseline_sample.expected_output) != self._parse_output(
            candidate_sample.expected_output
        )

    def _compute_metric_deltas(
        self,
        baseline_metrics: dict[str, float],
        candidate_metrics: dict[str, float],
    ) -> dict[str, float]:
        delta_metrics: dict[str, float] = {}
        for metric_name in sorted(set(baseline_metrics) | set(candidate_metrics)):
            delta_metrics[metric_name] = (
                candidate_metrics.get(metric_name, 0.0)
                - baseline_metrics.get(metric_name, 0.0)
            )
        return delta_metrics

    def _classify_failure(
        self,
        *,
        sample_result: SampleResultModel,
        metrics: dict[str, float],
    ) -> FailureCategory:
        if sample_result.error:
            return FailureCategory.EXECUTION_ERROR
        if metrics.get("json_validity", 1.0) < 1.0:
            return FailureCategory.FORMAT_ERROR
        if self._parse_json(sample_result.actual_output) is None and self._parse_json(sample_result.expected_output) is not None:
            return FailureCategory.FORMAT_ERROR
        if not sample_result.is_correct:
            return FailureCategory.SEMANTIC_ERROR
        return FailureCategory.UNKNOWN

    def _build_failure_reason(
        self,
        *,
        sample_result: SampleResultModel,
        category: FailureCategory,
        expected_output: str | dict[str, object],
        actual_output: str | dict[str, object],
    ) -> str:
        if category is FailureCategory.EXECUTION_ERROR:
            return sample_result.error or "模型调用或评估过程发生异常。"
        if category is FailureCategory.FORMAT_ERROR:
            return "输出不是合法 JSON，或未满足预期的结构化格式。"
        if category is FailureCategory.SEMANTIC_ERROR:
            if isinstance(expected_output, dict) and isinstance(actual_output, dict):
                mismatched_fields = [
                    field_name
                    for field_name in sorted(set(expected_output) | set(actual_output))
                    if expected_output.get(field_name) != actual_output.get(field_name)
                ]
                if mismatched_fields:
                    return f"关键字段不一致：{', '.join(mismatched_fields[:3])}"
            return "输出已成形，但语义内容与期望不一致。"
        return "失败原因暂未归类。"

    def _compute_slice_metrics(
        self,
        sample_results: Sequence[SampleResultModel],
    ) -> dict[str, dict[str, float | int]]:
        raw_counts: dict[str, dict[str, int]] = {}

        for sample_result in sample_results:
            text = sample_result.input_text
            self._update_slice(raw_counts, self._length_bucket_name(text), sample_result.is_correct)
            if NUMBER_PATTERN.search(text):
                self._update_slice(raw_counts, "contains_number", sample_result.is_correct)
            if any(keyword in text for keyword in NEGATION_KEYWORDS):
                self._update_slice(raw_counts, "contains_negation", sample_result.is_correct)

        slice_metrics: dict[str, dict[str, float | int]] = {}
        for slice_name, counters in raw_counts.items():
            total = counters["total"]
            correct = counters["correct"]
            slice_metrics[slice_name] = {
                "total": total,
                "failed": total - correct,
                "accuracy": (correct / total) if total else 0.0,
            }
        return slice_metrics

    def _build_suggestions(
        self,
        *,
        accuracy: float,
        category_counts: dict[str, int],
        slice_metrics: dict[str, dict[str, float | int]],
    ) -> list[str]:
        suggestions: list[str] = []
        failed_total = sum(category_counts.values())
        if failed_total == 0:
            return ["当前 run 没有失败样本，可继续比较不同候选的优势 slice。"]

        format_failures = category_counts.get(FailureCategory.FORMAT_ERROR.value, 0)
        semantic_failures = category_counts.get(FailureCategory.SEMANTIC_ERROR.value, 0)
        execution_failures = category_counts.get(FailureCategory.EXECUTION_ERROR.value, 0)

        if format_failures / failed_total >= 0.3:
            suggestions.append("格式错误占比较高，建议强化 JSON contract、输出示例或结构化约束。")
        if semantic_failures / failed_total >= 0.3 or accuracy < 0.8:
            suggestions.append("语义错误较多，建议补充 few-shot 示例或改写主指令以减少字段误判。")
        if execution_failures > 0:
            suggestions.append("存在执行错误，建议检查模型配置、网络稳定性与超时设置。")

        weakest_slice = self._find_weakest_slice(slice_metrics)
        if weakest_slice is not None:
            suggestions.append(f"当前最弱 slice 为 `{weakest_slice}`，建议围绕该类输入定向补充样例与规则。")

        return suggestions[:3]

    def _find_weakest_slice(
        self,
        slice_metrics: dict[str, dict[str, float | int]],
    ) -> str | None:
        candidate_name: str | None = None
        candidate_accuracy = 1.0
        for slice_name, metrics in slice_metrics.items():
            total = metrics.get("total")
            accuracy = metrics.get("accuracy")
            if not isinstance(total, int) or total == 0:
                continue
            if not isinstance(accuracy, float):
                continue
            if accuracy < candidate_accuracy:
                candidate_name = slice_name
                candidate_accuracy = accuracy
        return candidate_name

    def _update_slice(
        self,
        raw_counts: dict[str, dict[str, int]],
        slice_name: str,
        is_correct: bool,
    ) -> None:
        counters = raw_counts.setdefault(slice_name, {"total": 0, "correct": 0})
        counters["total"] += 1
        if is_correct:
            counters["correct"] += 1

    def _length_bucket_name(self, text: str) -> str:
        length = len(text)
        if length < 25:
            return "length_short"
        if length < 80:
            return "length_medium"
        return "length_long"

    def _parse_metrics(self, raw_metrics: str) -> dict[str, float]:
        parsed = self._parse_json(raw_metrics)
        if not isinstance(parsed, dict):
            return {}

        metrics: dict[str, float] = {}
        for key, value in parsed.items():
            if isinstance(value, bool):
                continue
            if isinstance(value, (int, float)):
                metrics[str(key)] = float(value)
        return metrics

    def _parse_output(self, raw_output: str) -> str | dict[str, object]:
        parsed = self._parse_json(raw_output)
        if isinstance(parsed, dict):
            return parsed
        return raw_output

    def _parse_json(self, payload: str) -> object | None:
        stripped = payload.strip()
        if not stripped:
            return None
        try:
            loaded: object = json.loads(stripped)
            return loaded
        except json.JSONDecodeError:
            return None

    def _failure_to_dict(self, failure: FailureCase) -> dict[str, object]:
        payload = asdict(failure)
        payload["category"] = failure.category.value
        return payload
