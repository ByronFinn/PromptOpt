"""F1 score evaluator."""

import json
import re
from collections import Counter

from pydantic import Field

from promptopt.evaluators.base import Evaluator


class F1Evaluator(Evaluator):
    """F1/macro-F1 evaluator for text classification and extraction.

    Computes token-level or word-level F1 between expected and actual outputs.
    """

    name: str = "f1"
    mode: str = Field(default="token", description="Comparison mode: 'token' or 'word'")
    average: str = Field(default="macro", description="Averaging method: 'macro' or 'micro'")

    def _tokenize(self, text: str) -> list[str]:
        """Tokenize text."""
        if self.mode == "word":
            return re.findall(r"\w+", text.lower())
        return re.findall(r"\S+", text.lower())

    def _compute_metrics(
        self,
        expected_tokens: list[str],
        actual_tokens: list[str],
    ) -> tuple[float, float, float]:
        """Compute precision, recall, and F1 between two token lists."""
        expected_counter = Counter(expected_tokens)
        actual_counter = Counter(actual_tokens)

        overlap = sum((expected_counter & actual_counter).values())

        if not expected_tokens or not actual_tokens:
            return 0.0, 0.0, 0.0

        precision = overlap / len(actual_tokens) if actual_tokens else 0.0
        recall = overlap / len(expected_tokens) if expected_tokens else 0.0

        if precision + recall == 0:
            return precision, recall, 0.0

        f1 = 2 * precision * recall / (precision + recall)
        return precision, recall, f1

    def evaluate(
        self,
        expected: str | dict[str, object],
        actual: str,
        **kwargs: object,
    ) -> tuple[bool, dict[str, float]]:
        """Evaluate F1 score."""
        exp_str, act_str = _normalize_structured_pair(expected, actual)
        exp_str = exp_str.strip()
        act_str = act_str.strip()

        exp_tokens = self._tokenize(exp_str)
        act_tokens = self._tokenize(act_str)

        precision, recall, f1 = self._compute_metrics(exp_tokens, act_tokens)
        is_correct = exp_str == act_str

        metrics = {
            "f1": f1,
            "precision": precision,
            "recall": recall,
        }
        if self.average == "macro":
            metrics["macro_f1"] = f1

        return is_correct, metrics


def _normalize_structured_pair(
    expected: str | dict[str, object],
    actual: str,
) -> tuple[str, str]:
    if not isinstance(expected, dict):
        return str(expected), str(actual)

    expected_json = json.dumps(expected, ensure_ascii=False, sort_keys=True)
    try:
        actual_parsed = json.loads(actual)
    except json.JSONDecodeError:
        return expected_json, str(actual)
    actual_json = json.dumps(actual_parsed, ensure_ascii=False, sort_keys=True)
    return expected_json, actual_json
