"""Output contract optimizer."""

from collections.abc import Mapping

from promptopt.optimizers.base import Optimizer


class ContractOptimizer(Optimizer):
    """Output constraint/contract optimizer.
    
    Strengthens output format constraints in the prompt.
    """
    
    name: str = "contract"
    
    def optimize(
        self,
        current_prompt: str,
        eval_results: Mapping[str, object],
        task_description: str,
        **kwargs: object,
    ) -> list[str]:
        """Generate contract-enhanced prompt candidates."""
        contract_template = """
        
【重要】输出格式要求：
1. 必须输出有效的JSON格式
2. 必须包含所有必填字段
3. 不要输出任何解释或额外文本
4. 直接输出JSON对象，不要有markdown标记
"""
        return [
            f"{current_prompt}{contract_template}",
            f"{current_prompt}\n\n【输出约束】\n- 仅输出JSON\n- 不要添加任何说明文字",
        ]
