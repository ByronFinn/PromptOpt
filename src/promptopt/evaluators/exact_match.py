"""Exact match evaluator."""

from pydantic import Field

from promptopt.evaluators.base import Evaluator


class ExactMatchEvaluator(Evaluator):
    """Exact string match evaluator.
    
    Returns 1.0 if strings match exactly, 0.0 otherwise.
    """
    
    name: str = "exact_match"
    ignore_case: bool = Field(default=False, description="Case insensitive comparison")
    strip_whitespace: bool = Field(default=True, description="Strip whitespace before comparison")
    
    def evaluate(
        self,
        expected: str | dict[str, object],
        actual: str,
        **kwargs: object,
    ) -> tuple[bool, dict[str, float]]:
        """Evaluate exact match."""
        exp_str = str(expected)
        act_str = str(actual)
        
        if self.ignore_case:
            exp_str = exp_str.lower()
            act_str = act_str.lower()
        
        if self.strip_whitespace:
            exp_str = exp_str.strip()
            act_str = act_str.strip()
        
        is_correct = exp_str == act_str
        
        return is_correct, {"exact_match": 1.0 if is_correct else 0.0}
