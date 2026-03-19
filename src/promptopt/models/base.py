"""Base model adapter interface."""

from abc import ABC, abstractmethod
from collections.abc import AsyncIterator
from typing import Any


class ModelAdapter(ABC):
    """Abstract base class for model adapters.
    
    All model adapters must implement this interface to be used with PromptOpt.
    """
    
    @abstractmethod
    async def generate(
        self,
        prompt: str,
        *,
        temperature: float = 0.0,
        max_tokens: int = 2048,
        **kwargs: Any,
    ) -> str:
        """Generate a response from the model.
        
        Args:
            prompt: Input prompt
            temperature: Sampling temperature
            max_tokens: Maximum tokens to generate
            **kwargs: Additional provider-specific parameters
            
        Returns:
            Generated text response
        """
        ...
    
    @abstractmethod
    def generate_stream(
        self,
        prompt: str,
        *,
        temperature: float = 0.0,
        max_tokens: int = 2048,
        **kwargs: Any,
    ) -> AsyncIterator[str]:
        """Generate a streaming response from the model.
        
        Args:
            prompt: Input prompt
            temperature: Sampling temperature
            max_tokens: Maximum tokens to generate
            **kwargs: Additional provider-specific parameters
            
        Yields:
            Text chunks as they are generated
        """
        ...
    @abstractmethod
    def get_token_count(self, text: str) -> int:
        """Get the number of tokens in the text.
        
        Args:
            text: Input text
            
        Returns:
            Token count
        """
        ...
    
    @property
    @abstractmethod
    def model_name(self) -> str:
        """Get the model name."""
        ...
    
    @property
    def supports_streaming(self) -> bool:
        """Whether this adapter supports streaming."""
        return True
