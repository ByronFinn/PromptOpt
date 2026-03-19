"""LiteLLM adapter for PromptOpt."""

from collections.abc import AsyncIterator
from typing import Any

import litellm

from promptopt.models.base import ModelAdapter


class LiteLLMAdapter(ModelAdapter):
    """Model adapter using LiteLLM.
    
    Supports OpenAI, Azure, vLLM, Ollama, Anthropic, and 100+ other providers.
    """
    
    def __init__(
        self,
        model: str,
        api_key: str | None = None,
        base_url: str | None = None,
        **kwargs: Any,
    ) -> None:
        """Initialize LiteLLM adapter.
        
        Args:
            model: Model name (e.g., "gpt-4", "anthropic/claude-3", "ollama/llama2")
            api_key: API key (if required)
            base_url: Base URL for custom endpoints (for vLLM, Ollama, etc.)
            **kwargs: Additional LiteLLM parameters
        """
        self.model = model
        self.api_key = api_key
        self.base_url = base_url
        self.default_kwargs = kwargs
    
    async def generate(
        self,
        prompt: str,
        *,
        temperature: float = 0.0,
        max_tokens: int = 2048,
        **kwargs: Any,
    ) -> str:
        """Generate a response using LiteLLM."""
        merged_kwargs = {**self.default_kwargs, **kwargs}
        
        response = await litellm.acompletion(
            model=self.model,
            messages=[{"role": "user", "content": prompt}],
            temperature=temperature,
            max_tokens=max_tokens,
            api_key=self.api_key,
            api_base=self.base_url,
            **merged_kwargs,
        )
        
        return response.choices[0].message.content or ""
    
    async def generate_stream(
        self,
        prompt: str,
        *,
        temperature: float = 0.0,
        max_tokens: int = 2048,
        **kwargs: Any,
    ) -> AsyncIterator[str]:
        """Generate a streaming response using LiteLLM."""
        merged_kwargs = {**self.default_kwargs, **kwargs}
        
        response = await litellm.acompletion(
            model=self.model,
            messages=[{"role": "user", "content": prompt}],
            temperature=temperature,
            max_tokens=max_tokens,
            api_key=self.api_key,
            api_base=self.base_url,
            stream=True,
            **merged_kwargs,
        )
        
        async for chunk in response:
            if chunk.choices and chunk.choices[0].delta.content:
                yield chunk.choices[0].delta.content
    
    def get_token_count(self, text: str) -> int:
        """Get token count using LiteLLM's token counter."""
        count = litellm.get_token_count(text, model=self.model)
        return int(count) if count else 0
    
    @property
    def model_name(self) -> str:
        """Get the model name."""
        return self.model
