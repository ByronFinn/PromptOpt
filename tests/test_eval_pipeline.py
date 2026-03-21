"""Integration tests for the evaluation pipeline and CLI."""

from __future__ import annotations

import json
from collections.abc import AsyncIterator, Mapping
from pathlib import Path

from typer.testing import CliRunner

from promptopt.cli import main as cli_main
from promptopt.cli.main import app
from promptopt.core import (
    Candidate,
    Dataset,
    DatasetLoader,
    EvaluationEngine,
    Split,
    Task,
    build_evaluators,
)
from promptopt.models.base import ModelAdapter
from promptopt.storage import SampleResultModel
from promptopt.storage.database import Database, reset_db
from promptopt.storage.models import CandidateModel, RunModel


class FakeAdapter(ModelAdapter):
    """Deterministic adapter used in tests."""

    def __init__(self, responses: Mapping[str, str], model_name: str = "fake/test-model") -> None:
        self._responses = dict(responses)
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
        for needle, response in self._responses.items():
            if needle in prompt:
                return response
        return "{}"

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


RUNNER = CliRunner()


def test_task_format_prompt_preserves_json_braces() -> None:
    task = Task(
        name="json_task",
        description="test",
        dataset=Dataset(name="demo", path="dataset.json"),
        prompt_template='输出 JSON: {"answer": "示例"}\n\n{input}',
    )

    rendered = task.format_prompt("hello")

    assert '{"answer": "示例"}' in rendered
    assert rendered.endswith("hello")


def test_evaluation_engine_persists_run_and_samples(tmp_path: Path) -> None:
    dataset_path = _write_demo_dataset(tmp_path / "dataset.json")
    db = Database(str(tmp_path / "promptopt.db"))
    db.create_tables()
    adapter = FakeAdapter(
        responses={
            "样本一": json.dumps(
                {"疾病": "感冒", "症状": ["咳嗽"]},
                ensure_ascii=False,
                sort_keys=True,
            ),
            "样本二": json.dumps(
                {"疾病": "胃炎", "症状": ["腹痛"]},
                ensure_ascii=False,
                sort_keys=True,
            ),
        }
    )
    engine = EvaluationEngine(
        adapter=adapter,
        evaluators=build_evaluators(["exact_match", "json_validator"]),
        db=db,
    )
    task = Task(
        name="medical_json_extraction",
        description="extract",
        dataset=Dataset(name="medical_demo", path=str(dataset_path)),
        prompt_template="不会被实际执行使用：{input}",
        evaluation_metrics=["exact_match", "json_validator"],
    )
    candidate = Candidate(
        id="baseline_001",
        name="baseline",
        prompt='请输出 JSON：\n{"疾病": "疾病名", "症状": []}\n\n{input}',
    )

    result = engine.run(
        task=task,
        candidate=candidate,
        dataset=DatasetLoader(path=str(dataset_path)),
        split=Split.DEV,
        task_path=tmp_path / "task.yaml",
        candidate_path=tmp_path / "candidate.yaml",
        dataset_path=dataset_path,
    )

    assert result.total_samples == 2
    assert result.correct_count == 1
    assert result.accuracy == 0.5
    assert result.aggregate_metrics["exact_match"] == 0.5
    assert result.aggregate_metrics["json_validity"] == 1.0
    assert result.completed_at is not None

    with db.session() as session:
        stored_run = session.get(RunModel, result.run_id)
        assert stored_run is not None
        assert stored_run.status == "completed"
        assert stored_run.model_name == adapter.model_name

        stored_candidate = session.get(CandidateModel, candidate.id)
        assert stored_candidate is not None
        assert stored_candidate.prompt == candidate.prompt

        stored_samples = (
            session.query(SampleResultModel)
            .filter(SampleResultModel.run_id == result.run_id)
            .order_by(SampleResultModel.sample_id.asc())
            .all()
        )
        assert len(stored_samples) == 2
        assert stored_samples[0].sample_id == "sample_001"
        assert stored_samples[0].is_correct is True
        assert stored_samples[1].sample_id == "sample_002"
        assert stored_samples[1].is_correct is False


def test_eval_command_runs_with_project_config(tmp_path: Path, monkeypatch) -> None:
    reset_db()
    project_dir = tmp_path / "demo_project"
    task_path, candidate_path, dataset_config_path = _write_demo_project(project_dir)
    fake_adapter = FakeAdapter(
        responses={
            "样本一": json.dumps(
                {"疾病": "感冒", "症状": ["咳嗽"]},
                ensure_ascii=False,
                sort_keys=True,
            ),
            "样本二": json.dumps(
                {"疾病": "心绞痛", "症状": ["胸痛"]},
                ensure_ascii=False,
                sort_keys=True,
            ),
        }
    )
    monkeypatch.setattr(cli_main, "build_model_adapter", lambda config: fake_adapter)

    result = RUNNER.invoke(
        app,
        [
            "eval",
            "--task",
            str(task_path),
            "--candidate",
            str(candidate_path),
            "--dataset",
            str(dataset_config_path),
            "--split",
            "dev",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "Run 已保存" in result.output
    assert "评估结果" in result.output

    db = Database(str(project_dir / ".promptopt" / "promptopt.db"))
    db.create_tables()
    with db.session() as session:
        runs = session.query(RunModel).all()
        assert len(runs) == 1
        assert runs[0].status == "completed"
        assert runs[0].accuracy == 1.0


def test_init_creates_project_templates(tmp_path: Path) -> None:
    project_path = tmp_path / "scaffolded"

    result = RUNNER.invoke(app, ["init", "demo", "--output", str(project_path)])

    assert result.exit_code == 0, result.output
    assert (project_path / ".promptopt.yaml").exists()
    assert (project_path / "tasks" / "task.yaml").exists()
    assert (project_path / "candidates" / "baseline.yaml").exists()
    assert (project_path / "datasets" / "dataset.yaml").exists()
    assert (project_path / "datasets" / "samples.json").exists()


def _write_demo_dataset(path: Path) -> Path:
    payload = {
        "samples": [
            {
                "id": "sample_001",
                "input": "样本一：患者反复咳嗽 3 天。",
                "expected": {"疾病": "感冒", "症状": ["咳嗽"]},
                "split": "dev",
            },
            {
                "id": "sample_002",
                "input": "样本二：患者胸痛 2 小时。",
                "expected": {"疾病": "心绞痛", "症状": ["胸痛"]},
                "split": "dev",
            },
        ]
    }
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
    return path


def _write_demo_project(project_dir: Path) -> tuple[Path, Path, Path]:
    (project_dir / "tasks").mkdir(parents=True, exist_ok=True)
    (project_dir / "candidates").mkdir(parents=True, exist_ok=True)
    (project_dir / "datasets").mkdir(parents=True, exist_ok=True)
    (project_dir / ".promptopt").mkdir(parents=True, exist_ok=True)

    task_path = project_dir / "tasks" / "task.yaml"
    task_path.write_text(
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

    candidate_path = project_dir / "candidates" / "baseline.yaml"
    candidate_path.write_text(
        """id: baseline_001
name: baseline
prompt: |
  从以下医疗文本中抽取信息，并以 JSON 格式输出：

  {input}

  输出格式：
  {
    "疾病": "疾病名称",
    "症状": []
  }
metadata:
  strategy: baseline
""",
        encoding="utf-8",
    )

    dataset_json_path = _write_demo_dataset(project_dir / "datasets" / "samples.json")
    dataset_config_path = project_dir / "datasets" / "dataset.yaml"
    dataset_config_path.write_text(
        f"""name: medical_demo
path: {dataset_json_path.relative_to(project_dir)}
split_field: split
""",
        encoding="utf-8",
    )

    (project_dir / ".promptopt.yaml").write_text(
        f"""models:
  teacher: openai/gpt-4
  target: openai/gpt-3.5-turbo

evaluation:
  timeout: 30

storage:
  db_path: {str(project_dir / '.promptopt' / 'promptopt.db')}
""",
        encoding="utf-8",
    )
    return task_path, candidate_path, dataset_config_path
