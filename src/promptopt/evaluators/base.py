"""Base evaluator interface."""

from abc import ABC, abstractmethod
from typing import Protocol, TypeVar

from pydantic import BaseModel

T = TypeVar("T")


class EvalCallback(Protocol):
    """Callback protocol for evaluation progress.
    
    Called for each sample during evaluation.
    """
    
    def on_sample_eval(
        self,
        sample_id: str,
        is_correct: bool,
        metrics: dict[str, float],
    ) -> None:
        """Called after each sample is evaluated.
        
        Args:
            sample_id: Sample identifier
            is_correct: Whether prediction was correct
            metrics: Dict of metric name to value
        """
        ...
    
    def on_error(self, sample_id: str, error: str) -> None:
        """Called when an error occurs during evaluation.
        
        Args:
            sample_id: Sample identifier
            error: Error message
        """
        ...


class Evaluator(ABC, BaseModel):
    """Abstract base class for evaluators.
    
    Evaluators compute metrics by comparing model predictions against expected outputs.
    """
    
    name: str
    
    @abstractmethod
    def evaluate(
        self,
        expected: str | dict[str, object],
        actual: str,
        **kwargs: object,
    ) -> tuple[bool, dict[str, float]]:
        """Evaluate a single prediction.
        
        Args:
            expected: Expected output
            actual: Actual model output
            **kwargs: Additional context
            
        Returns:
            Tuple of (is_correct, metrics_dict)
        """
        ...
    
    def evaluate_batch(
        self,
        samples: list[dict[str, object]],
        callback: EvalCallback | None = None,
    ) -> dict[str, object]:
        """Evaluate a batch of samples.
        
        Args:
            samples: List of dicts with 'id', 'expected', 'actual' keys
            callback: Optional progress callback
            
        Returns:
            Aggregated results
        """
        results = []
        correct_count = 0
        
        for sample in samples:
            sample_id = str(sample.get("id", "unknown"))
            expected_value = sample["expected"]
            actual = str(sample["actual"])
            
            # Convert expected to appropriate type
            if isinstance(expected_value, dict):
                expected: str | dict[str, object] = expected_value
            else:
                expected = str(expected_value)
            
            try:
                is_correct, metrics = self.evaluate(expected, actual)
                results.append({
                    "sample_id": sample_id,
                    "is_correct": is_correct,
                    "metrics": metrics,
                })
                if is_correct:
                    correct_count += 1
                    
                if callback:
                    callback.on_sample_eval(sample_id, is_correct, metrics)
                    
            except Exception as e:
                if callback:
                    callback.on_error(sample_id, str(e))
                results.append({
                    "sample_id": sample_id,
                    "is_correct": False,
                    "error": str(e),
                })
        
        return {
            "total": len(samples),
            "correct": correct_count,
            "accuracy": correct_count / len(samples) if samples else 0.0,
            "results": results,
        }
