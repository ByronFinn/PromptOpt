"""Model adapters module for PromptOpt."""

from promptopt.models.base import ModelAdapter, ModelProvider
from promptopt.models.litellm_adapter import LiteLLMAdapter

__all__ = [
    "ModelAdapter",
    "ModelProvider",
    "LiteLLMAdapter",
]
