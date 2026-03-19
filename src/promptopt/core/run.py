"""Run and evaluation result models."""

from __future__ import annotations

from datetime import datetime

from pydantic import BaseModel, Field


class EvalResult(BaseModel):
    """Evaluation result for a single sample.
    
    Attributes:
        sample_id: Identifier for the test sample
        input_text: Input text
        expected_output: Expected output
        actual_output: Actual model output
        is_correct: Whether the prediction is correct
        metrics: Dict of metric name to value
        error: Error message if evaluation failed
    """
    sample_id: str
    input_text: str
    expected_output: str | dict[str, object]
    actual_output: str | dict[str, object]
    is_correct: bool = False
    metrics: dict[str, float] = Field(default_factory=dict)
    error: str | None = None


class RunResult(BaseModel):
    """Aggregated results for a run.
    
    Attributes:
        candidate_id: ID of evaluated candidate
        run_id: ID of this run
        total_samples: Total number of samples
        correct_count: Number of correct predictions
        aggregate_metrics: Aggregated metrics (f1, precision, recall, etc.)
        sample_results: List of per-sample results
        started_at: Start timestamp
        completed_at: Completion timestamp
        duration_seconds: Total duration
        cost: API cost in USD
        latency_ms: Average latency in milliseconds
    """
    candidate_id: str
    run_id: str
    total_samples: int = 0
    correct_count: int = 0
    aggregate_metrics: dict[str, float] = Field(default_factory=dict)
    sample_results: list[EvalResult] = Field(default_factory=list)
    started_at: datetime = Field(default_factory=datetime.now)
    completed_at: datetime | None = None
    duration_seconds: float = 0.0
    cost: float = 0.0
    latency_ms: float = 0.0
    
    @property
    def accuracy(self) -> float:
        """Calculate accuracy."""
        if self.total_samples == 0:
            return 0.0
        return self.correct_count / self.total_samples


class Run(BaseModel):
    """A run tracking experiment execution.
    
    Attributes:
        id: Unique identifier
        task_id: ID of task being evaluated
        candidate_id: ID of candidate being evaluated
        split: Dataset split used
        status: Run status (pending, running, completed, failed)
        result: Evaluation result (if completed)
        created_at: Creation timestamp
    """
    id: str
    task_id: str
    candidate_id: str
    split: str = "dev"
    status: str = "pending"
    result: RunResult | None = None
    created_at: datetime = Field(default_factory=datetime.now)
    error: str | None = None
