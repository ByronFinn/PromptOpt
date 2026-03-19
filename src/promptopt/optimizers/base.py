"""Base optimizer interface."""

from __future__ import annotations

from abc import ABC, abstractmethod
from collections.abc import Mapping

from pydantic import BaseModel


class Optimizer(ABC, BaseModel):
    """Abstract base class for prompt optimizers.
    
    Optimizers generate improved candidate prompts based on evaluation results.
    """
    
    name: str
    
    @abstractmethod
    def optimize(
        self,
        current_prompt: str,
        eval_results: Mapping[str, object],
        task_description: str,
        **kwargs: object,
    ) -> list[str]:
        """Generate optimized prompt candidates.
        
        Args:
            current_prompt: The current prompt to improve
            eval_results: Results from evaluation including errors and metrics
            task_description: Description of the task
            **kwargs: Additional context
            
        Returns:
            List of optimized prompt candidates
        """
        ...


class RewriteOptimizer(Optimizer):
    """Instruction rewrite optimizer.
    
    Rewrites the instruction part of the prompt for clarity and completeness.
    """
    
    name: str = "rewrite"
    
    def optimize(
        self,
        current_prompt: str,
        eval_results: Mapping[str, object],
        task_description: str,
        **kwargs: object,
    ) -> list[str]:
        """Generate rewritten instruction candidates."""
        # Placeholder - actual implementation would use LLM
        return [
            f"请仔细阅读以下任务并严格按照要求执行：\n\n{task_description}\n\n{current_prompt}",
            f"你是一个专业的任务助手。请根据以下要求完成：\n\n{task_description}\n\n{current_prompt}",
        ]
