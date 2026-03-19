"""Tests for core models."""

from pathlib import Path

import pytest
from pydantic import ValidationError

from promptopt.core import (
    Task,
    Dataset,
    Candidate,
    CandidateMetadata,
    Run,
    RunResult,
    EvalResult,
)


class TestDataset:
    """Tests for Dataset model."""
    
    def test_valid_dataset(self):
        dataset = Dataset(
            name="test_ds",
            path="data/test.json",
            split_field="split",
        )
        assert dataset.name == "test_ds"
        assert dataset.path == "data/test.json"
        assert dataset.split_field == "split"
    
    def test_dataset_defaults(self):
        dataset = Dataset(name="test", path="test.json")
        assert dataset.split_field == "split"


class TestTask:
    """Tests for Task model."""
    
    DATA_DIR = Path(__file__).parent / "data"
    
    def test_valid_task(self):
        task = Task(
            name="test_task",
            description="A test task",
            dataset=Dataset(name="ds", path="ds.json"),
            prompt_template="Extract: {input}",
        )
        assert task.name == "test_task"
        assert "{input}" in task.prompt_template
    
    def test_format_prompt(self):
        task = Task(
            name="test",
            description="test",
            dataset=Dataset(name="ds", path="ds.json"),
            prompt_template="Input: {input}",
        )
        formatted = task.format_prompt("hello world")
        assert "hello world" in formatted
    
    def test_from_yaml(self):
        """Test loading Task from YAML file."""
        task = Task.from_yaml(self.DATA_DIR / "task.yaml")
        assert task.name == "test_task"
        assert task.description == "A test task for validation"
        assert task.dataset.name == "test_dataset"
        assert task.prompt_template == "Extract: {input}"
        assert task.output_schema == '{"type": "object"}'
        assert "exact_match" in task.evaluation_metrics
        assert "json_validator" in task.evaluation_metrics
    
    def test_from_yaml_file_not_found(self):
        """Test that FileNotFoundError is raised for missing file."""
        with pytest.raises(FileNotFoundError):
            Task.from_yaml(self.DATA_DIR / "nonexistent.yaml")


class TestCandidate:
    """Tests for Candidate model."""

    DATA_DIR = Path(__file__).parent / "data"

    def test_valid_candidate(self):
        candidate = Candidate(
            id="cand_001",
            name="baseline",
            prompt="Extract: {input}",
        )
        assert candidate.id == "cand_001"
        assert candidate.metadata.strategy == "baseline"

    def test_candidate_with_metadata(self):
        metadata = CandidateMetadata(
            strategy="rewrite",
            parent_id="cand_000",
            teacher_model="gpt-4",
        )
        candidate = Candidate(
            id="cand_002",
            name="rewrite_v1",
            prompt="Improved: {input}",
            metadata=metadata,
        )
        assert candidate.metadata.strategy == "rewrite"
        assert candidate.metadata.parent_id == "cand_000"

    def test_from_yaml(self):
        """Test loading Candidate from YAML file."""
        candidate = Candidate.from_yaml(self.DATA_DIR / "candidate.yaml")
        assert candidate.id == "test_cand_001"
        assert candidate.name == "test_candidate"
        assert candidate.description == "A test candidate prompt"
        assert "{input}" in candidate.prompt
        assert candidate.metadata.strategy == "rewrite"
        assert candidate.metadata.parent_id == "baseline_001"
        assert candidate.metadata.teacher_model == "gpt-4"
        assert candidate.metadata.generation_params["temperature"] == 0.7

    def test_from_yaml_file_not_found(self):
        """Test that FileNotFoundError is raised for missing file."""
        with pytest.raises(FileNotFoundError):
            Candidate.from_yaml(self.DATA_DIR / "nonexistent.yaml")


class TestEvalResult:
    """Tests for EvalResult model."""
    
    def test_eval_result(self):
        result = EvalResult(
            sample_id="s_001",
            input_text="test input",
            expected_output="expected",
            actual_output="actual",
            is_correct=False,
            metrics={"f1": 0.5},
        )
        assert result.sample_id == "s_001"
        assert result.is_correct is False
        assert result.metrics["f1"] == 0.5


class TestRunResult:
    """Tests for RunResult model."""
    
    def test_run_result_accuracy(self):
        result = RunResult(
            candidate_id="c_001",
            run_id="r_001",
            total_samples=10,
            correct_count=7,
        )
        assert result.accuracy == 0.7
    
    def test_run_result_empty(self):
        result = RunResult(
            candidate_id="c_001",
            run_id="r_001",
        )
        assert result.accuracy == 0.0
        assert result.total_samples == 0


class TestRun:
    """Tests for Run model."""
    
    def test_pending_run(self):
        run = Run(
            id="run_001",
            task_id="task_001",
            candidate_id="cand_001",
        )
        assert run.status == "pending"
        assert run.result is None
