"""Tests for search, select, and lineage persistence."""

from __future__ import annotations

import json
from collections.abc import AsyncIterator
from pathlib import Path

from typer.testing import CliRunner

from promptopt.cli import main as cli_main
from promptopt.cli.main import app
from promptopt.models.base import ModelAdapter
from promptopt.storage.database import Database, reset_db
from promptopt.storage.models import CandidateModel, LineageModel, RunModel

RUNNER = CliRunner()


class FakeTargetAdapter(ModelAdapter):
    """Deterministic target adapter for search/select tests."""

    def __init__(self, model_name: str = "fake/target-model") -> None:
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
        if "结构化抽取专家" in prompt:
            if "患者咳嗽 3 天。" in prompt:
                return json.dumps({"疾病": "感冒", "症状": ["咳嗽"]}, ensure_ascii=False, sort_keys=True)
            return json.dumps({"疾病": "糖尿病", "症状": ["血糖升高"]}, ensure_ascii=False, sort_keys=True)
        if "患者咳嗽 3 天。" in prompt:
            return json.dumps({"疾病": "感冒", "症状": ["咳嗽"]}, ensure_ascii=False, sort_keys=True)
        return "not-json"

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



def test_search_command_runs_all_candidates_and_select_picks_best(
    tmp_path: Path,
    monkeypatch,
) -> None:
    reset_db()
    project_dir, db_path = _seed_search_project(tmp_path / "search_project")
    monkeypatch.chdir(project_dir)
    monkeypatch.setattr(
        cli_main,
        "build_model_adapter",
        lambda config: FakeTargetAdapter(),
    )

    search_result = RUNNER.invoke(
        app,
        [
            "search",
            str(project_dir / "candidates"),
            "--task",
            str(project_dir / "tasks" / "task.yaml"),
            "--dataset",
            str(project_dir / "datasets" / "dataset.yaml"),
            "--split",
            "dev",
        ],
    )

    assert search_result.exit_code == 0, search_result.output
    assert "批量评估" in search_result.output

    db = Database(str(db_path))
    db.create_tables()
    with db.session() as session:
        runs = session.query(RunModel).order_by(RunModel.created_at.asc()).all()
        assert len(runs) == 2
        baseline_run = next(run for run in runs if run.candidate_id == "baseline_001")
        rewrite_run = next(run for run in runs if run.candidate_id == "rewrite_001")
        assert baseline_run.accuracy == 0.5
        assert rewrite_run.accuracy == 1.0

        lineage = session.get(LineageModel, "rewrite_001")
        assert lineage is not None
        assert lineage.parent_id == "baseline_001"
        assert "baseline_001" in lineage.ancestors

    select_result = RUNNER.invoke(
        app,
        [
            "select",
            baseline_run.id,
            "--primary",
            "accuracy",
            "--secondary",
            "json_validity",
            "--constraints",
            "json_validity=1.0",
        ],
    )

    assert select_result.exit_code == 0, select_result.output
    assert "选中候选: rewrite_001" in select_result.output
    assert "Constraints:" in select_result.output


def test_search_and_select_support_output_json(tmp_path: Path, monkeypatch) -> None:
    reset_db()
    project_dir, db_path = _seed_search_project(tmp_path / "search_json_project")
    monkeypatch.chdir(project_dir)
    monkeypatch.setattr(cli_main, "build_model_adapter", lambda config: FakeTargetAdapter())

    search_result = RUNNER.invoke(
        app,
        [
            "search",
            str(project_dir / "candidates"),
            "--task",
            str(project_dir / "tasks" / "task.yaml"),
            "--dataset",
            str(project_dir / "datasets" / "dataset.yaml"),
            "--split",
            "dev",
            "--quiet",
            "--output-json",
        ],
    )

    assert search_result.exit_code == 0, search_result.output
    search_payload = json.loads(search_result.output)
    assert search_payload["kind"] == "search"
    assert len(search_payload["results"]) == 2

    db = Database(str(db_path))
    db.create_tables()
    with db.session() as session:
        baseline_run = next(run for run in session.query(RunModel).all() if run.candidate_id == "baseline_001")

    select_result = RUNNER.invoke(
        app,
        [
            "select",
            baseline_run.id,
            "--primary",
            "accuracy",
            "--quiet",
            "--output-json",
        ],
    )

    assert select_result.exit_code == 0, select_result.output
    select_payload = json.loads(select_result.output)
    assert select_payload["kind"] == "select"
    assert select_payload["selected_candidate_id"] == "rewrite_001"


def test_search_changed_only_filters_candidates(tmp_path: Path, monkeypatch) -> None:
    reset_db()
    project_dir, _db_path = _seed_search_project(tmp_path / "search_changed_project")
    monkeypatch.chdir(project_dir)
    monkeypatch.setattr(cli_main, "build_model_adapter", lambda config: FakeTargetAdapter())
    monkeypatch.setattr(
        cli_main,
        "_filter_candidate_files_by_git_diff",
        lambda candidate_files, *, candidates_dir, git_base_ref: [
            path for path in candidate_files if path.name == "rewrite.yaml"
        ],
    )

    result = RUNNER.invoke(
        app,
        [
            "search",
            str(project_dir / "candidates"),
            "--task",
            str(project_dir / "tasks" / "task.yaml"),
            "--dataset",
            str(project_dir / "datasets" / "dataset.yaml"),
            "--changed-only",
            "--quiet",
            "--output-json",
        ],
    )

    assert result.exit_code == 0, result.output
    payload = json.loads(result.output)
    assert len(payload["results"]) == 1
    assert payload["results"][0]["candidate_id"] == "rewrite_001"


def test_rollback_command_exports_candidate_yaml(tmp_path: Path, monkeypatch) -> None:
    reset_db()
    project_dir, db_path = _seed_search_project(tmp_path / "rollback_project")
    monkeypatch.chdir(project_dir)

    db = Database(str(db_path))
    db.create_tables()
    with db.session() as session:
        session.add(
            CandidateModel(
                id="legacy_001",
                name="legacy_prompt",
                prompt="请输出 JSON\n\n{input}",
                strategy="baseline",
            )
        )

    output_path = project_dir / "rollback" / "legacy_rollback.yaml"
    result = RUNNER.invoke(app, ["rollback", "legacy_001", "--output", str(output_path)])

    assert result.exit_code == 0, result.output
    assert output_path.exists()
    exported = output_path.read_text(encoding="utf-8")
    assert "legacy_001_rollback" in exported
    assert "rollback_source" in exported



def _seed_search_project(project_dir: Path) -> tuple[Path, Path]:
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
                        "input": "患者血糖升高 3 年。",
                        "expected": {"疾病": "糖尿病", "症状": ["血糖升高"]},
                        "split": "dev",
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
    (project_dir / "candidates" / "rewrite.yaml").write_text(
        """id: rewrite_001
name: rewrite_v1
prompt: |
  你是一名结构化抽取专家。请直接输出最终 JSON 结果。

  {input}
metadata:
  strategy: rewrite
  parent_id: baseline_001
  teacher_model: fake/teacher-model
""",
        encoding="utf-8",
    )

    return project_dir, db_path
