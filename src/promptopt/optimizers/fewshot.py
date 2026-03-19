"""Few-shot optimizer."""

from collections.abc import Mapping

from promptopt.optimizers.base import Optimizer


class FewShotOptimizer(Optimizer):
    """Few-shot example optimizer.
    
    Adds or improves few-shot examples in the prompt.
    """
    
    name: str = "fewshot"
    
    def optimize(
        self,
        current_prompt: str,
        eval_results: Mapping[str, object],
        task_description: str,
        **kwargs: object,
    ) -> list[str]:
        """Generate few-shot enhanced prompt candidates."""
        # Placeholder - actual implementation would select/add examples
        return [
            f"{current_prompt}\n\n示例：\n输入：xxx\n输出：{{...}}",
            f"{current_prompt}\n\n注意：请参考以下示例格式：\n1. 示例1...\n2. 示例2...",
        ]
