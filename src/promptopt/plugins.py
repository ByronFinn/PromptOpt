"""Plugin registries and entry-point discovery helpers."""

from __future__ import annotations

from importlib.metadata import entry_points
from typing import Any

from promptopt.evaluators import (
    ExactMatchEvaluator,
    F1Evaluator,
    JSONValidatorEvaluator,
)
from promptopt.models import LiteLLMAdapter, ModelAdapter, ModelProvider
from promptopt.optimizers import ContractOptimizer, FewShotOptimizer, RewriteOptimizer


class LiteLLMProvider:
    """Default provider plugin backed by LiteLLM."""

    name = "litellm"

    def supports(self, model_name: str) -> bool:
        del model_name
        return True

    def build(
        self,
        model_name: str,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        **kwargs: Any,
    ) -> ModelAdapter:
        return LiteLLMAdapter(
            model=model_name,
            api_key=api_key,
            base_url=base_url,
            **kwargs,
        )


def get_evaluator_registry() -> dict[str, type[ExactMatchEvaluator] | type[F1Evaluator] | type[JSONValidatorEvaluator] | type[object]]:
    """Return the merged evaluator registry (built-ins + entry points)."""
    registry: dict[str, type[object]] = {
        "exact_match": ExactMatchEvaluator,
        "f1": F1Evaluator,
        "json_validity": JSONValidatorEvaluator,
    }
    registry.update(_load_named_entry_points("promptopt.evaluators"))
    return registry


def get_optimizer_registry() -> dict[str, type[RewriteOptimizer] | type[FewShotOptimizer] | type[ContractOptimizer] | type[object]]:
    """Return the merged optimizer registry (built-ins + entry points)."""
    registry: dict[str, type[object]] = {
        "rewrite": RewriteOptimizer,
        "fewshot": FewShotOptimizer,
        "contract": ContractOptimizer,
    }
    registry.update(_load_named_entry_points("promptopt.optimizers"))
    return registry


def get_provider_registry() -> dict[str, ModelProvider]:
    """Return the merged model provider registry (built-ins + entry points)."""
    registry: dict[str, ModelProvider] = {"litellm": LiteLLMProvider()}
    registry.update(_load_provider_entry_points())
    return registry


def resolve_provider(model_name: str) -> ModelProvider:
    """Resolve the most suitable provider for a model name."""
    provider_name = model_name.split("/", 1)[0] if "/" in model_name else model_name
    registry = get_provider_registry()
    direct_match = registry.get(provider_name)
    if direct_match is not None and direct_match.supports(model_name):
        return direct_match

    for provider in registry.values():
        if provider.supports(model_name):
            return provider
    raise ValueError(f"未找到可处理模型 `{model_name}` 的 provider 插件。")


def _load_named_entry_points(group: str) -> dict[str, type[object]]:
    loaded: dict[str, type[object]] = {}
    for entry_point in entry_points(group=group):
        plugin = entry_point.load()
        plugin_name = getattr(plugin, "name", entry_point.name)
        if not isinstance(plugin_name, str) or not plugin_name.strip():
            raise ValueError(f"插件 `{entry_point.value}` 缺少有效 name。")
        if not isinstance(plugin, type):
            raise ValueError(f"插件 `{entry_point.value}` 必须导出可实例化类。")
        loaded[plugin_name] = plugin
    return loaded


def _load_provider_entry_points() -> dict[str, ModelProvider]:
    loaded: dict[str, ModelProvider] = {}
    for entry_point in entry_points(group="promptopt.providers"):
        plugin = entry_point.load()
        provider = plugin() if isinstance(plugin, type) else plugin
        provider_name = getattr(provider, "name", entry_point.name)
        if not isinstance(provider_name, str) or not provider_name.strip():
            raise ValueError(f"Provider 插件 `{entry_point.value}` 缺少有效 name。")
        loaded[provider_name] = provider
    return loaded
