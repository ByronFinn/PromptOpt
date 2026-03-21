"""Tests for the optimize CLI workflow."""

from __future__ import annotations

import json
from collections.abc import AsyncIterator
from pathlib import Path

import yaml
from typer.testing import CliRunner

from promptopt.cli import main as cli_main
from promptopt.cli.main import app
from promptopt.models.base import ModelAdapter
from promptopt.storage.database import Database, reset_db
from promptopt.storage.models import CandidateModel, RunModel, SampleResultModel

RUNNER = CliRunner()


class FakeTeacherAdapter(ModelAdapter):
    """Teacher adapter that returns deterministic rewrite candidates."""

    def __init__(self, response: str, model_name: str = "fake/teacher-model") -> None:
        self._response = response
        self._model_name = model_name

    async def generate(
        self,
        prompt: str,
        *,
        temperature: float = 0.0,
        max_tokens: int = 2048,
        **kwargs: object,
    ) -> str:
        del prompt, temperature, max_tokens, kwargs
        return self._response

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


def test_optimize_command_generates_rewrite_candidates(tmp_path: Path, monkeypatch) -> None:
    reset_db()
    project_dir = tmp_path / "optimize_project"
    db_path, run_id, candidate_dir = _seed_optimize_project(project_dir)
    teacher_calls: list[str | None] = []

    def fake_builder(config: object, *, teacher_model: str | None = None) -> FakeTeacherAdapter:
        del config
        teacher_calls.append(teacher_model)
        return FakeTeacherAdapter(
            response=json.dumps(
                {
                    "candidates": [
                        "请严格按照任务要求执行，并保留 {input} 占位符。\n\n{input}",
                        "你是一名结构化抽取专家。请直接输出最终结果。\n\n{input}",
                    ]
                },
                ensure_ascii=False,
            ),
            model_name="fake/config-teacher",
        )

    monkeypatch.setattr(cli_main, "build_teacher_model_adapter", fake_builder)
    monkeypatch.chdir(project_dir)

    result = RUNNER.invoke(app, ["optimize", run_id, "--strategies", "rewrite", "--num-candidates", "2"])

    assert result.exit_code == 0, result.output
    assert "优化结果" in result.output
    assert teacher_calls == [None]

    generated_files = sorted(candidate_dir.glob("baseline_001_rewrite_*.yaml"))
    assert len(generated_files) == 2

    first_candidate = yaml.safe_load(generated_files[0].read_text(encoding="utf-8"))
    assert first_candidate["metadata"]["strategy"] == "rewrite"
    assert first_candidate["metadata"]["parent_id"] == "baseline_001"
    assert first_candidate["metadata"]["teacher_model"] == "fake/config-teacher"
    assert first_candidate["metadata"]["generation_params"]["source_run_id"] == run_id

    db = Database(str(db_path))
    db.create_tables()
    with db.session() as session:
        stored_run = session.get(RunModel, run_id)
        assert stored_run is not None



def test_optimize_command_teacher_option_overrides_config(
    tmp_path: Path,
    monkeypatch,
) -> None:
    reset_db()
    project_dir = tmp_path / "optimize_teacher_override"
    _db_path, run_id, _candidate_dir = _seed_optimize_project(project_dir)
    teacher_calls: list[str | None] = []

    def fake_builder(config: object, *, teacher_model: str | None = None) -> FakeTeacherAdapter:
        del config
        teacher_calls.append(teacher_model)
        return FakeTeacherAdapter(
            response=json.dumps({"candidates": ["改写版本 A\n\n{input}"]}, ensure_ascii=False),
            model_name="fake/override-teacher",
        )

    monkeypatch.setattr(cli_main, "build_teacher_model_adapter", fake_builder)
    monkeypatch.chdir(project_dir)

    result = RUNNER.invoke(
        app,
        [
            "optimize",
            run_id,
            "--teacher",
            "openai/custom-teacher",
            "--strategies",
            "rewrite",
            "--num-candidates",
            "1",
        ],
    )

    assert result.exit_code == 0, result.output
    assert teacher_calls == ["openai/custom-teacher"]


def test_optimize_command_supports_multiple_strategies(
    tmp_path: Path,
    monkeypatch,
) -> None:
    reset_db()
    project_dir = tmp_path / "optimize_multi_strategy"
    _db_path, run_id, candidate_dir = _seed_optimize_project(project_dir)

    monkeypatch.setattr(
        cli_main,
        "build_teacher_model_adapter",
        lambda config, *, teacher_model=None: FakeTeacherAdapter(
            response=json.dumps(
                {"candidates": ["rewrite 候选版本\n\n{input}"]},
                ensure_ascii=False,
            ),
            model_name="fake/multi-teacher",
        ),
    )
    monkeypatch.chdir(project_dir)

    result = RUNNER.invoke(
        app,
        [
            "optimize",
            run_id,
            "--strategies",
            "rewrite,fewshot,contract",
            "--num-candidates",
            "6",
        ],
    )

    assert result.exit_code == 0, result.output
    generated_files = sorted(candidate_dir.glob("baseline_001_*.yaml"))
    generated_names = [path.name for path in generated_files]
    assert any("rewrite" in name for name in generated_names)
    assert any("fewshot" in name for name in generated_names)
    assert any("contract" in name for name in generated_names)



def test_optimize_command_rejects_unsupported_strategies(
    tmp_path: Path,
    monkeypatch,
) -> None:
    reset_db()
    project_dir = tmp_path / "optimize_guard_project"
    _db_path, run_id, _candidate_dir = _seed_optimize_project(project_dir)
    monkeypatch.chdir(project_dir)

    result = RUNNER.invoke(
        app,
        [
            "optimize",
            run_id,
            "--strategies",
            "rewrite,unknown",
        ],
    )

    assert result.exit_code == 1
    assert "不支持的优化策略" in result.output



def _seed_optimize_project(project_dir: Path) -> tuple[Path, str, Path]:
    project_dir.mkdir(parents=True, exist_ok=True)
    task_dir = project_dir / "tasks"
    candidate_dir = project_dir / "candidates"
    task_dir.mkdir(parents=True, exist_ok=True)
    candidate_dir.mkdir(parents=True, exist_ok=True)
    db_path = project_dir / ".promptopt" / "promptopt.db"
    db_path.parent.mkdir(parents=True, exist_ok=True)

    task_path = task_dir / "task.yaml"
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

    candidate_path = candidate_dir / "baseline.yaml"
    candidate_path.write_text(
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

    (project_dir / ".promptopt.yaml").write_text(
        f"""models:
  teacher: openai/gpt-4
  target: openai/gpt-3.5-turbo

storage:
  db_path: {db_path}
""",
        encoding="utf-8",
    )

    run_id = "run_optimize_001"

    def expected_json(payload: dict[str, object] | dict[str, float]) -> str:
        return json.dumps(payload, ensure_ascii=False, sort_keys=True)

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
                id=run_id,
                task_id="medical_json_extraction",
                task_path="tasks/task.yaml",
                candidate_id="baseline_001",
                candidate_path="candidates/baseline.yaml",
                model_name="fake/target-model",
                split="dev",
                status="completed",
                total_samples=2,
                correct_count=1,
                accuracy=0.5,
                aggregate_metrics_json=expected_json(
                    {"exact_match": 0.5, "json_validity": 0.5}
                ),
            )
        )
        session.add_all(
            [
                SampleResultModel(
                    run_id=run_id,
                    sample_id="sample_001",
                    input_text="患者咳嗽 3 天。",
                    expected_output=expected_json({"疾病": "感冒", "症状": ["咳嗽"]}),
                    actual_output=expected_json({"疾病": "感冒", "症状": ["咳嗽"]}),
                    is_correct=True,
                    metrics_json=expected_json(
                        {"exact_match": 1.0, "json_validity": 1.0}
                    ),
                ),
                SampleResultModel(
                    run_id=run_id,
                    sample_id="sample_002",
                    input_text="患者血糖升高 3 年。",
                    expected_output=expected_json(
                        {"疾病": "糖尿病", "症状": ["血糖升高"]}
                    ),
                    actual_output="not-json",
                    is_correct=False,
                    metrics_json=expected_json(
                        {"exact_match": 0.0, "json_validity": 0.0}
                    ),
                ),
            ]
        )

    return db_path, run_id, candidate_dir
