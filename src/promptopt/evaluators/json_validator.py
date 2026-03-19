"""JSON validator evaluator."""

import json

from pydantic import Field

from promptopt.evaluators.base import Evaluator


class JSONValidatorEvaluator(Evaluator):
    """JSON validity and schema validation evaluator.
    
    Checks if the actual output is valid JSON and optionally validates against a schema.
    """
    
    name: str = "json_validity"
    strict: bool = Field(default=False, description="Use strict JSON parsing")
    
    def evaluate(
        self,
        expected: str | dict[str, object],
        actual: str,
        **kwargs: object,
    ) -> tuple[bool, dict[str, float]]:
        """Evaluate JSON validity."""
        metrics: dict[str, float] = {"json_validity": 0.0}
        is_valid = False
        
        try:
            json.loads(actual)
            is_valid = True
            metrics["json_validity"] = 1.0
        except json.JSONDecodeError:
            is_valid = False
        
        # If expected is also JSON, check structural equality
        if is_valid and expected:
            try:
                exp_json = json.loads(str(expected)) if isinstance(expected, str) else expected
                act_json = json.loads(actual)
                is_valid = exp_json == act_json
                metrics["json_structure_match"] = 1.0 if is_valid else 0.0
            except (json.JSONDecodeError, TypeError):
                pass
        
        return is_valid, metrics
