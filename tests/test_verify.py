"""Tests for the verify CLI workflow."""

from __future__ import annotations

import json
from collections.abc import AsyncIterator
from pathlib import Path

from typer.testing import CliRunner

from promptopt.cli import main as cli_main
from promptopt.cli.main import app
from promptopt.models.base import ModelAdapter
from promptopt.storage.database import Database, reset_db
from promptopt.storage.models import CandidateModel, RunModel, SampleResultModel

RUNNER = CliRunner()


class FakeVerifyAdapter(ModelAdapter):
    """Deterministic target adapter for verify tests."""

    def __init__(self, model_name: str = "fake/verify-model") -> None:
        self._model_name = model_name

    async def generate(
        self,
        prompt: str,
        *,
        temperature: float = 0.0,
        max_tokens: int = 2048,
        **kwargs: object,
    ) -> str:
        del temperature, max_tokens, kwargs
        if "患者胸痛 2 小时。" in prompt:
            return json.dumps({"疾病": "心绞痛", "症状": ["胸痛"]}, ensure_ascii=False, sort_keys=True)
        return json.dumps({"疾病": "感冒", "症状": ["咳嗽"]}, ensure_ascii=False, sort_keys=True)

    def generate_stream(
        self,
        prompt: str,
        *,
        temperature: float = 0.0,
        max_tokens: int = 2048,
        **kwargs: object,
    ) -> AsyncIterator[str]:
        async def iterator() -> AsyncIterator[str]:
            yield await self.generate(
                prompt,
                temperature=temperature,
                max_tokens=max_tokens,
                **kwargs,
            )

        return iterator()

    def get_token_count(self, text: str) -> int:
        return len(text)

    @property
    def model_name(self) -> str:
        return self._model_name


class FakeRegressionAdapter(FakeVerifyAdapter):
    """Adapter that intentionally regresses on the test sample."""

    async def generate(
        self,
        prompt: str,
        *,
        temperature: float = 0.0,
        max_tokens: int = 2048,
        **kwargs: object,
    ) -> str:
        del temperature, max_tokens, kwargs
        if "患者胸痛 2 小时。" in prompt:
            return json.dumps({"疾病": "胃炎", "症状": ["腹痛"]}, ensure_ascii=False, sort_keys=True)
        return await super().generate(prompt)



def test_verify_command_runs_test_split_and_persists_new_run(
    tmp_path: Path,
    monkeypatch,
) -> None:
    reset_db()
    project_dir, db_path, source_run_id = _seed_verify_project(tmp_path / "verify_project")
    monkeypatch.chdir(project_dir)
    monkeypatch.setattr(
        cli_main,
        "build_model_adapter",
        lambda config: FakeVerifyAdapter(),
    )

    result = RUNNER.invoke(app, ["verify", source_run_id, "--split", "test"])

    assert result.exit_code == 0, result.output
    assert "测试集验证" in result.output
    assert "Verify Run 已保存" in result.output

    db = Database(str(db_path))
    db.create_tables()
    with db.session() as session:
        runs = session.query(RunModel).order_by(RunModel.created_at.asc()).all()
        assert len(runs) == 3
        verify_run = next(
            run for run in runs if run.id not in {source_run_id, "run_verify_baseline_test"}
        )
        assert verify_run.split == "test"
        assert verify_run.accuracy == 1.0
        assert verify_run.candidate_id == "baseline_001"


def test_verify_command_supports_baseline_run_and_constraints(
    tmp_path: Path,
    monkeypatch,
) -> None:
    reset_db()
    project_dir, _db_path, source_run_id = _seed_verify_project(tmp_path / "verify_compare_project")
    monkeypatch.chdir(project_dir)
    monkeypatch.setattr(cli_main, "build_model_adapter", lambda config: FakeVerifyAdapter())

    result = RUNNER.invoke(
        app,
        [
            "verify",
            source_run_id,
            "--split",
            "test",
            "--baseline-run",
            "run_verify_baseline_test",
            "--constraints",
            "json_validity=1.0",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "Baseline Diff" in result.output
    assert "Verify Run 已保存" in result.output


def test_verify_command_fails_on_regression(
    tmp_path: Path,
    monkeypatch,
) -> None:
    reset_db()
    project_dir, _db_path, source_run_id = _seed_verify_project(tmp_path / "verify_regression_project")
    monkeypatch.chdir(project_dir)
    monkeypatch.setattr(cli_main, "build_model_adapter", lambda config: FakeRegressionAdapter())

    result = RUNNER.invoke(
        app,
        [
            "verify",
            source_run_id,
            "--split",
            "test",
            "--baseline-run",
            "run_verify_baseline_test",
        ],
    )

    assert result.exit_code == 1
    assert "Verify gate 未通过" in result.output
    assert "Regression failure" in result.output



def _seed_verify_project(project_dir: Path) -> tuple[Path, Path, str]:
    project_dir.mkdir(parents=True, exist_ok=True)
    (project_dir / "tasks").mkdir(parents=True, exist_ok=True)
    (project_dir / "candidates").mkdir(parents=True, exist_ok=True)
    (project_dir / "datasets").mkdir(parents=True, exist_ok=True)
    (project_dir / ".promptopt").mkdir(parents=True, exist_ok=True)
    db_path = project_dir / ".promptopt" / "promptopt.db"

    (project_dir / ".promptopt.yaml").write_text(
        f"""models:
  target: openai/gpt-3.5-turbo
  teacher: openai/gpt-4

storage:
  db_path: {db_path}
""",
        encoding="utf-8",
    )

    (project_dir / "tasks" / "task.yaml").write_text(
        """name: medical_json_extraction

description: 从医疗文本中抽取疾病和症状

dataset:
  name: medical_demo
  path: datasets/dataset.yaml
  split_field: split
prompt_template: |
  从以下文本中抽取结构化信息：

  {input}
evaluation_metrics:
  - exact_match
  - json_validator
""",
        encoding="utf-8",
    )
    (project_dir / "candidates" / "baseline.yaml").write_text(
        """id: baseline_001
name: baseline
prompt: |
  从以下医疗文本中抽取信息，并以 JSON 格式输出：

  {input}
metadata:
  strategy: baseline
""",
        encoding="utf-8",
    )
    (project_dir / "datasets" / "samples.json").write_text(
        json.dumps(
            {
                "samples": [
                    {
                        "id": "sample_001",
                        "input": "患者咳嗽 3 天。",
                        "expected": {"疾病": "感冒", "症状": ["咳嗽"]},
                        "split": "dev",
                    },
                    {
                        "id": "sample_002",
                        "input": "患者胸痛 2 小时。",
                        "expected": {"疾病": "心绞痛", "症状": ["胸痛"]},
                        "split": "test",
                    },
                ]
            },
            ensure_ascii=False,
            indent=2,
        ),
        encoding="utf-8",
    )
    (project_dir / "datasets" / "dataset.yaml").write_text(
        """name: medical_demo
path: datasets/samples.json
split_field: split
""",
        encoding="utf-8",
    )

    source_run_id = "run_verify_source"
    baseline_test_run_id = "run_verify_baseline_test"
    db = Database(str(db_path))
    db.create_tables()
    with db.session() as session:
        session.add(
            CandidateModel(
                id="baseline_001",
                name="baseline",
                prompt="从以下医疗文本中抽取信息，并以 JSON 格式输出：\n\n{input}",
                strategy="baseline",
            )
        )
        session.add(
            RunModel(
                id=source_run_id,
                task_id="medical_json_extraction",
                task_path="tasks/task.yaml",
                candidate_id="baseline_001",
                candidate_path="candidates/baseline.yaml",
                dataset_path="datasets/dataset.yaml",
                model_name="fake/dev-model",
                split="dev",
                status="completed",
                total_samples=1,
                correct_count=1,
                accuracy=1.0,
                aggregate_metrics_json=json.dumps({"exact_match": 1.0, "json_validity": 1.0}, ensure_ascii=False, sort_keys=True),
            )
        )
        session.add(
            RunModel(
                id=baseline_test_run_id,
                task_id="medical_json_extraction",
                task_path="tasks/task.yaml",
                candidate_id="baseline_001",
                candidate_path="candidates/baseline.yaml",
                dataset_path="datasets/dataset.yaml",
                model_name="fake/test-model",
                split="test",
                status="completed",
                total_samples=1,
                correct_count=1,
                accuracy=1.0,
                aggregate_metrics_json=json.dumps({"exact_match": 1.0, "json_validity": 1.0}, ensure_ascii=False, sort_keys=True),
            )
        )
        session.add_all(
            [
                SampleResultModel(
                    run_id=source_run_id,
                    sample_id="sample_001",
                    input_text="患者咳嗽 3 天。",
                    expected_output=json.dumps({"疾病": "感冒", "症状": ["咳嗽"]}, ensure_ascii=False, sort_keys=True),
                    actual_output=json.dumps({"疾病": "感冒", "症状": ["咳嗽"]}, ensure_ascii=False, sort_keys=True),
                    is_correct=True,
                    metrics_json=json.dumps({"exact_match": 1.0, "json_validity": 1.0}, ensure_ascii=False, sort_keys=True),
                ),
                SampleResultModel(
                    run_id=baseline_test_run_id,
                    sample_id="sample_002",
                    input_text="患者胸痛 2 小时。",
                    expected_output=json.dumps({"疾病": "心绞痛", "症状": ["胸痛"]}, ensure_ascii=False, sort_keys=True),
                    actual_output=json.dumps({"疾病": "心绞痛", "症状": ["胸痛"]}, ensure_ascii=False, sort_keys=True),
                    is_correct=True,
                    metrics_json=json.dumps({"exact_match": 1.0, "json_validity": 1.0}, ensure_ascii=False, sort_keys=True),
                ),
            ]
        )

    return project_dir, db_path, source_run_id
