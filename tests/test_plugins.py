"""Tests for plugin registry discovery and routing."""

from __future__ import annotations

from dataclasses import dataclass

from promptopt.core.evaluation import (
    ProjectConfig,
    build_evaluators,
    build_model_adapter,
)
from promptopt.evaluators.base import Evaluator
from promptopt.models.base import ModelAdapter, ModelProvider
from promptopt.optimizers.base import Optimizer
from promptopt.plugins import (
    get_evaluator_registry,
    get_optimizer_registry,
    resolve_provider,
)


class CustomEvaluator(Evaluator):
    name: str = "custom_metric"

    def evaluate(self, expected: str | dict[str, object], actual: str, **kwargs: object) -> tuple[bool, dict[str, float]]:
        del expected, actual, kwargs
        return True, {"custom_metric": 1.0}


class CustomOptimizer(Optimizer):
    name: str = "custom_optimizer"

    def optimize(self, current_prompt: str, eval_results: dict[str, object], task_description: str, **kwargs: object) -> list[str]:
        del eval_results, task_description, kwargs
        return [f"custom::{current_prompt}"]


class CustomAdapter(ModelAdapter):
    async def generate(self, prompt: str, *, temperature: float = 0.0, max_tokens: int = 2048, **kwargs: object) -> str:
        del prompt, temperature, max_tokens, kwargs
        return "ok"

    def generate_stream(self, prompt: str, *, temperature: float = 0.0, max_tokens: int = 2048, **kwargs: object):
        del prompt, temperature, max_tokens, kwargs
        async def iterator():
            yield "ok"
        return iterator()

    def get_token_count(self, text: str) -> int:
        return len(text)

    @property
    def model_name(self) -> str:
        return "custom/model"


@dataclass(slots=True)
class CustomProvider(ModelProvider):
    name: str = "custom"

    def supports(self, model_name: str) -> bool:
        return model_name.startswith("custom/")

    def build(self, model_name: str, *, api_key: str | None = None, base_url: str | None = None, **kwargs: object) -> ModelAdapter:
        del model_name, api_key, base_url, kwargs
        return CustomAdapter()


class FakeEntryPoint:
    def __init__(self, name: str, value: str, plugin: object) -> None:
        self.name = name
        self.value = value
        self._plugin = plugin

    def load(self) -> object:
        return self._plugin



def test_plugin_registries_load_entry_points(monkeypatch) -> None:
    monkeypatch.setattr(
        "promptopt.plugins.entry_points",
        lambda *, group: {
            "promptopt.evaluators": [FakeEntryPoint("custom_metric", "pkg:CustomEvaluator", CustomEvaluator)],
            "promptopt.optimizers": [FakeEntryPoint("custom_optimizer", "pkg:CustomOptimizer", CustomOptimizer)],
            "promptopt.providers": [FakeEntryPoint("custom", "pkg:CustomProvider", CustomProvider)],
        }[group],
    )

    evaluator_registry = get_evaluator_registry()
    optimizer_registry = get_optimizer_registry()
    provider = resolve_provider("custom/model")

    assert "custom_metric" in evaluator_registry
    assert "custom_optimizer" in optimizer_registry
    assert isinstance(provider, CustomProvider)

    evaluators = build_evaluators(["custom_metric"])
    assert evaluators[0].name == "custom_metric"

    adapter = build_model_adapter(ProjectConfig(target_model="custom/model"))
    assert adapter.model_name == "custom/model"
