"""Tests for diagnostics analysis and CLI integration."""

from __future__ import annotations

import json
from pathlib import Path

from typer.testing import CliRunner

from promptopt.cli.main import app
from promptopt.diagnostics import DiagnosticsAnalyzer, FailureCategory
from promptopt.storage.database import Database, reset_db
from promptopt.storage.models import CandidateModel, RunModel, SampleResultModel

RUNNER = CliRunner()


def test_diagnostics_analyzer_builds_report_and_exports_failures(tmp_path: Path) -> None:
    db = Database(str(tmp_path / "diagnostics.db"))
    db.create_tables()
    run_id = _seed_diagnostics_run(db)
    analyzer = DiagnosticsAnalyzer()

    with db.session() as session:
        run = session.get(RunModel, run_id)
        assert run is not None
        sample_results = (
            session.query(SampleResultModel)
            .filter(SampleResultModel.run_id == run_id)
            .order_by(SampleResultModel.sample_id.asc())
            .all()
        )
        report = analyzer.analyze_run(run, sample_results, top_k=2)

    assert report.failed_samples == 3
    assert report.category_counts[FailureCategory.FORMAT_ERROR.value] == 1
    assert report.category_counts[FailureCategory.SEMANTIC_ERROR.value] == 1
    assert report.category_counts[FailureCategory.EXECUTION_ERROR.value] == 1
    assert report.aggregate_metrics["exact_match"] == 0.25
    assert "contains_number" in report.slice_metrics
    assert len(report.top_failures) == 2
    assert report.top_failures[0].sample_id == "sample_002"
    assert any("JSON contract" in suggestion for suggestion in report.suggestions)

    export_path = tmp_path / "failures.json"
    analyzer.export_failures(report.failures, export_path)
    exported = json.loads(export_path.read_text(encoding="utf-8"))

    assert len(exported) == 3
    assert exported[0]["category"] == FailureCategory.FORMAT_ERROR.value



def test_diagnose_command_renders_report_and_exports_failures(
    tmp_path: Path,
    monkeypatch,
) -> None:
    reset_db()
    project_dir = tmp_path / "diagnostics_project"
    project_dir.mkdir(parents=True, exist_ok=True)
    db_path = project_dir / ".promptopt" / "promptopt.db"
    db_path.parent.mkdir(parents=True, exist_ok=True)

    db = Database(str(db_path))
    db.create_tables()
    run_id = _seed_diagnostics_run(db)

    (project_dir / ".promptopt.yaml").write_text(
        f"""models:
  target: openai/gpt-3.5-turbo

storage:
  db_path: {db_path}
""",
        encoding="utf-8",
    )
    monkeypatch.chdir(project_dir)

    export_path = project_dir / "diagnose_failures.json"
    result = RUNNER.invoke(
        app,
        [
            "diagnose",
            run_id,
            "--top-k",
            "2",
            "--export-failures",
            str(export_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "失败类别分布" in result.output
    assert "Top 失败样本" in result.output
    assert export_path.exists()

    exported = json.loads(export_path.read_text(encoding="utf-8"))
    assert len(exported) == 3


def test_diagnostics_analyzer_builds_baseline_diff_report(tmp_path: Path) -> None:
    db = Database(str(tmp_path / "diagnostics_diff.db"))
    db.create_tables()
    baseline_run_id, candidate_run_id = _seed_diff_runs(db)
    analyzer = DiagnosticsAnalyzer()

    with db.session() as session:
        baseline_run = session.get(RunModel, baseline_run_id)
        candidate_run = session.get(RunModel, candidate_run_id)
        assert baseline_run is not None
        assert candidate_run is not None

        baseline_sample_results = (
            session.query(SampleResultModel)
            .filter(SampleResultModel.run_id == baseline_run_id)
            .order_by(SampleResultModel.sample_id.asc())
            .all()
        )
        candidate_sample_results = (
            session.query(SampleResultModel)
            .filter(SampleResultModel.run_id == candidate_run_id)
            .order_by(SampleResultModel.sample_id.asc())
            .all()
        )
        report = analyzer.compare_runs(
            baseline_run,
            baseline_sample_results,
            candidate_run,
            candidate_sample_results,
            top_k=3,
        )

    assert report.matched_samples == 4
    assert report.accuracy_delta == 0.0
    assert len(report.regressions) == 1
    assert report.regressions[0].sample_id == "sample_003"
    assert len(report.improvements) == 1
    assert report.improvements[0].sample_id == "sample_002"
    assert report.still_failed == 1
    assert report.still_correct == 1
    assert report.aggregate_metric_deltas["json_validity"] == 0.25


def test_diagnose_command_supports_baseline_diff(tmp_path: Path, monkeypatch) -> None:
    reset_db()
    project_dir = tmp_path / "diagnostics_diff_project"
    project_dir.mkdir(parents=True, exist_ok=True)
    db_path = project_dir / ".promptopt" / "promptopt.db"
    db_path.parent.mkdir(parents=True, exist_ok=True)

    db = Database(str(db_path))
    db.create_tables()
    baseline_run_id, candidate_run_id = _seed_diff_runs(db)

    (project_dir / ".promptopt.yaml").write_text(
        f"""models:
  target: openai/gpt-3.5-turbo

storage:
  db_path: {db_path}
""",
        encoding="utf-8",
    )
    monkeypatch.chdir(project_dir)

    result = RUNNER.invoke(
        app,
        [
            "diagnose",
            candidate_run_id,
            "--baseline-run",
            baseline_run_id,
            "--top-k",
            "3",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "Baseline Diff" in result.output
    assert "退化样本" in result.output
    assert "提升样本" in result.output


def test_diagnose_command_rejects_incompatible_runs(tmp_path: Path, monkeypatch) -> None:
    reset_db()
    project_dir = tmp_path / "diagnostics_guard_project"
    project_dir.mkdir(parents=True, exist_ok=True)
    db_path = project_dir / ".promptopt" / "promptopt.db"
    db_path.parent.mkdir(parents=True, exist_ok=True)

    db = Database(str(db_path))
    db.create_tables()
    baseline_run_id, candidate_run_id = _seed_diff_runs(db)

    with db.session() as session:
        candidate_run = session.get(RunModel, candidate_run_id)
        assert candidate_run is not None
        candidate_run.split = "test"

    (project_dir / ".promptopt.yaml").write_text(
        f"""models:
  target: openai/gpt-3.5-turbo

storage:
  db_path: {db_path}
""",
        encoding="utf-8",
    )
    monkeypatch.chdir(project_dir)

    result = RUNNER.invoke(
        app,
        [
            "diagnose",
            candidate_run_id,
            "--baseline-run",
            baseline_run_id,
        ],
    )

    assert result.exit_code == 1
    assert "无法比较 runs" in result.output



def _seed_diagnostics_run(db: Database) -> str:
    run_id = "run_diag_001"
    candidate_id = "baseline_001"

    def expected_json(payload: dict[str, object] | dict[str, float]) -> str:
        return json.dumps(payload, ensure_ascii=False, sort_keys=True)

    with db.session() as session:
        session.add(
            CandidateModel(
                id=candidate_id,
                name="baseline",
                prompt="{input}",
                strategy="baseline",
            )
        )
        session.add(
            RunModel(
                id=run_id,
                task_id="medical_json_extraction",
                candidate_id=candidate_id,
                model_name="fake/test-model",
                status="completed",
                total_samples=4,
                correct_count=1,
                accuracy=0.25,
                aggregate_metrics_json=expected_json(
                    {"exact_match": 0.25, "json_validity": 0.75}
                ),
            )
        )
        session.add_all(
            [
                SampleResultModel(
                    run_id=run_id,
                    sample_id="sample_001",
                    input_text="患者男性，58岁，咳嗽 3 天。",
                    expected_output=expected_json({"疾病": "感冒", "症状": ["咳嗽"]}),
                    actual_output=expected_json({"疾病": "感冒", "症状": ["咳嗽"]}),
                    is_correct=True,
                    metrics_json=expected_json({"exact_match": 1.0, "json_validity": 1.0}),
                ),
                SampleResultModel(
                    run_id=run_id,
                    sample_id="sample_002",
                    input_text="患者女性，42岁，血糖升高 3 年。",
                    expected_output=expected_json({"疾病": "糖尿病", "症状": ["血糖升高"]}),
                    actual_output="not-json",
                    is_correct=False,
                    metrics_json=expected_json({"exact_match": 0.0, "json_validity": 0.0}),
                ),
                SampleResultModel(
                    run_id=run_id,
                    sample_id="sample_003",
                    input_text="患者否认胸痛，但诉心慌 2 天。",
                    expected_output=expected_json({"疾病": "心律失常", "症状": ["心慌"]}),
                    actual_output=expected_json({"疾病": "胃炎", "症状": ["腹痛"]}),
                    is_correct=False,
                    metrics_json=expected_json({"exact_match": 0.0, "json_validity": 1.0}),
                ),
                SampleResultModel(
                    run_id=run_id,
                    sample_id="sample_004",
                    input_text="患者男性，60岁，夜间气促加重。",
                    expected_output=expected_json({"疾病": "心衰", "症状": ["气促"]}),
                    actual_output="",
                    is_correct=False,
                    metrics_json=expected_json({}),
                    error="timeout while calling model",
                ),
            ]
        )
    return run_id


def _seed_diff_runs(db: Database) -> tuple[str, str]:
    baseline_run_id = "run_diff_baseline"
    candidate_run_id = "run_diff_candidate"
    baseline_candidate_id = "baseline_001"
    candidate_id = "rewrite_001"

    def expected_json(payload: dict[str, object] | dict[str, float]) -> str:
        return json.dumps(payload, ensure_ascii=False, sort_keys=True)

    with db.session() as session:
        for candidate_id_value, candidate_name in (
            (baseline_candidate_id, "baseline"),
            (candidate_id, "rewrite_v1"),
        ):
            session.add(
                CandidateModel(
                    id=candidate_id_value,
                    name=candidate_name,
                    prompt="{input}",
                    strategy="baseline" if candidate_id_value == baseline_candidate_id else "rewrite",
                )
            )

        session.add_all(
            [
                RunModel(
                    id=baseline_run_id,
                    task_id="medical_json_extraction",
                    candidate_id=baseline_candidate_id,
                    model_name="fake/test-model",
                    split="dev",
                    status="completed",
                    total_samples=4,
                    correct_count=2,
                    accuracy=0.5,
                    aggregate_metrics_json=expected_json(
                        {"exact_match": 0.5, "json_validity": 0.5}
                    ),
                ),
                RunModel(
                    id=candidate_run_id,
                    task_id="medical_json_extraction",
                    candidate_id=candidate_id,
                    model_name="fake/test-model",
                    split="dev",
                    status="completed",
                    total_samples=4,
                    correct_count=2,
                    accuracy=0.5,
                    aggregate_metrics_json=expected_json(
                        {"exact_match": 0.5, "json_validity": 0.75}
                    ),
                ),
            ]
        )

        baseline_samples = [
            SampleResultModel(
                run_id=baseline_run_id,
                sample_id="sample_001",
                input_text="患者男性，58岁，咳嗽 3 天。",
                expected_output=expected_json({"疾病": "感冒", "症状": ["咳嗽"]}),
                actual_output=expected_json({"疾病": "感冒", "症状": ["咳嗽"]}),
                is_correct=True,
                metrics_json=expected_json({"exact_match": 1.0, "json_validity": 1.0}),
            ),
            SampleResultModel(
                run_id=baseline_run_id,
                sample_id="sample_002",
                input_text="患者女性，42岁，血糖升高 3 年。",
                expected_output=expected_json({"疾病": "糖尿病", "症状": ["血糖升高"]}),
                actual_output="not-json",
                is_correct=False,
                metrics_json=expected_json({"exact_match": 0.0, "json_validity": 0.0}),
            ),
            SampleResultModel(
                run_id=baseline_run_id,
                sample_id="sample_003",
                input_text="患者否认胸痛，但诉心慌 2 天。",
                expected_output=expected_json({"疾病": "心律失常", "症状": ["心慌"]}),
                actual_output=expected_json({"疾病": "心律失常", "症状": ["心慌"]}),
                is_correct=True,
                metrics_json=expected_json({"exact_match": 1.0, "json_validity": 1.0}),
            ),
            SampleResultModel(
                run_id=baseline_run_id,
                sample_id="sample_004",
                input_text="患者男性，60岁，夜间气促加重。",
                expected_output=expected_json({"疾病": "心衰", "症状": ["气促"]}),
                actual_output="timeout",
                is_correct=False,
                metrics_json=expected_json({"exact_match": 0.0, "json_validity": 0.0}),
                error="timeout while calling model",
            ),
        ]
        candidate_samples = [
            SampleResultModel(
                run_id=candidate_run_id,
                sample_id="sample_001",
                input_text="患者男性，58岁，咳嗽 3 天。",
                expected_output=expected_json({"疾病": "感冒", "症状": ["咳嗽"]}),
                actual_output=expected_json({"疾病": "感冒", "症状": ["咳嗽"]}),
                is_correct=True,
                metrics_json=expected_json({"exact_match": 1.0, "json_validity": 1.0}),
            ),
            SampleResultModel(
                run_id=candidate_run_id,
                sample_id="sample_002",
                input_text="患者女性，42岁，血糖升高 3 年。",
                expected_output=expected_json({"疾病": "糖尿病", "症状": ["血糖升高"]}),
                actual_output=expected_json({"疾病": "糖尿病", "症状": ["血糖升高"]}),
                is_correct=True,
                metrics_json=expected_json({"exact_match": 1.0, "json_validity": 1.0}),
            ),
            SampleResultModel(
                run_id=candidate_run_id,
                sample_id="sample_003",
                input_text="患者否认胸痛，但诉心慌 2 天。",
                expected_output=expected_json({"疾病": "心律失常", "症状": ["心慌"]}),
                actual_output=expected_json({"疾病": "胃炎", "症状": ["腹痛"]}),
                is_correct=False,
                metrics_json=expected_json({"exact_match": 0.0, "json_validity": 1.0}),
            ),
            SampleResultModel(
                run_id=candidate_run_id,
                sample_id="sample_004",
                input_text="患者男性，60岁，夜间气促加重。",
                expected_output=expected_json({"疾病": "心衰", "症状": ["气促"]}),
                actual_output=expected_json({"疾病": "心衰", "症状": ["气促"]}),
                is_correct=False,
                metrics_json=expected_json({"exact_match": 0.0, "json_validity": 1.0}),
                error="timeout while calling model",
            ),
        ]
        session.add_all(baseline_samples + candidate_samples)

    return baseline_run_id, candidate_run_id
