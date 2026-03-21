"""Smoke tests for built-in examples and init templates."""

from __future__ import annotations

from pathlib import Path

from typer.testing import CliRunner

from promptopt.cli.main import app
from promptopt.core import Candidate, DatasetLoader, Task

RUNNER = CliRunner()


EXAMPLE_DIRS = [
    Path("/home/ubuntu/Projects/PromptOpt/examples/json_extraction"),
    Path("/home/ubuntu/Projects/PromptOpt/examples/classification"),
    Path("/home/ubuntu/Projects/PromptOpt/examples/qa"),
]


def test_examples_are_loadable() -> None:
    for example_dir in EXAMPLE_DIRS:
        task = Task.from_yaml(example_dir / "tasks" / "task.yaml")
        candidate = Candidate.from_yaml(example_dir / "candidates" / "baseline.yaml")
        dataset_loader = DatasetLoader(path=str(example_dir / "datasets" / "dataset.yaml"))
        samples = dataset_loader.load()

        assert task.name
        assert candidate.id
        assert samples



def test_init_supports_example_templates(tmp_path: Path) -> None:
    output_path = tmp_path / "classification_project"
    result = RUNNER.invoke(
        app,
        ["init", "demo", "--output", str(output_path), "--template", "classification"],
    )

    assert result.exit_code == 0, result.output
    assert (output_path / ".promptopt.yaml").exists()
    assert (output_path / "tasks" / "task.yaml").exists()
    assert (output_path / "candidates" / "baseline.yaml").exists()
    assert (output_path / "datasets" / "dataset.yaml").exists()
    assert (output_path / "datasets" / "intent.json").exists()
