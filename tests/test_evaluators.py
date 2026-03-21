"""Tests for evaluators."""

from promptopt.evaluators import (
    ExactMatchEvaluator,
    F1Evaluator,
    JSONValidatorEvaluator,
)


class TestExactMatchEvaluator:
    """Tests for ExactMatchEvaluator."""
    
    def test_exact_match_success(self):
        evaluator = ExactMatchEvaluator()
        is_correct, metrics = evaluator.evaluate("hello", "hello")
        assert is_correct is True
        assert metrics["exact_match"] == 1.0
    
    def test_exact_match_failure(self):
        evaluator = ExactMatchEvaluator()
        is_correct, metrics = evaluator.evaluate("hello", "world")
        assert is_correct is False
        assert metrics["exact_match"] == 0.0
    
    def test_case_insensitive(self):
        evaluator = ExactMatchEvaluator(ignore_case=True)
        is_correct, _ = evaluator.evaluate("Hello", "hello")
        assert is_correct is True
    
    def test_strip_whitespace(self):
        evaluator = ExactMatchEvaluator(strip_whitespace=True)
        is_correct, _ = evaluator.evaluate("  hello  ", "hello")
        assert is_correct is True


class TestF1Evaluator:
    """Tests for F1Evaluator."""
    
    def test_f1_perfect_match(self):
        evaluator = F1Evaluator()
        is_correct, metrics = evaluator.evaluate("hello world", "hello world")
        assert is_correct is True
        assert metrics["f1"] == 1.0
    
    def test_f1_partial_match(self):
        evaluator = F1Evaluator()
        is_correct, metrics = evaluator.evaluate("hello world foo", "hello world bar")
        assert is_correct is False
        assert 0.0 < metrics["f1"] < 1.0
    
    def test_f1_no_overlap(self):
        evaluator = F1Evaluator()
        is_correct, metrics = evaluator.evaluate("hello", "world")
        assert metrics["f1"] == 0.0
    
    def test_f1_word_mode(self):
        evaluator = F1Evaluator(mode="word")
        is_correct, metrics = evaluator.evaluate("hello world", "hello world")
        assert metrics["f1"] == 1.0


class TestJSONValidatorEvaluator:
    """Tests for JSONValidatorEvaluator."""
    
    def test_valid_json(self):
        evaluator = JSONValidatorEvaluator()
        is_correct, metrics = evaluator.evaluate('{"key": "value"}', '{"key": "value"}')
        assert is_correct is True
        assert metrics["json_validity"] == 1.0
    
    def test_invalid_json(self):
        evaluator = JSONValidatorEvaluator()
        is_correct, metrics = evaluator.evaluate('{"key": "value"}', 'not json')
        assert is_correct is False
        assert metrics["json_validity"] == 0.0
    
    def test_strict_parsing(self):
        evaluator = JSONValidatorEvaluator(strict=True)
        is_correct, metrics = evaluator.evaluate('{"key": "value"}', '{"key": "value"}')
        assert is_correct is True
