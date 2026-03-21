"""Output contract optimizer."""

import json
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
        output_schema = kwargs.get("output_schema")
        suggestions = self._format_suggestions(eval_results.get("suggestions"))
        schema_block = self._format_schema(output_schema)
        contract_template = f"""

【重要】输出格式要求：
1. 必须输出有效的 JSON 格式
2. 必须包含所有必填字段
3. 不要输出任何解释或额外文本
4. 直接输出 JSON 对象，不要有 markdown 标记
5. 输出前请根据以下约束自检：
{suggestions}

{schema_block}
"""
        return [
            f"{current_prompt}{contract_template}",
            (
                f"{current_prompt}\n\n"
                "【输出约束】\n"
                "- 仅输出 JSON\n"
                "- 不要添加任何说明文字\n"
                "- 如果字段缺失，请输出空字符串、空数组或空对象，而不是省略字段\n"
                f"- 遵循以下 schema/字段说明：\n{schema_block}"
            ),
        ]

    def _format_schema(self, output_schema: object) -> str:
        if output_schema is None:
            return "【Schema】未提供显式 schema，请至少保证字段齐全与 JSON 合法。"
        if isinstance(output_schema, str):
            stripped = output_schema.strip()
            if not stripped:
                return "【Schema】未提供显式 schema，请至少保证字段齐全与 JSON 合法。"
            try:
                parsed: object = json.loads(stripped)
            except json.JSONDecodeError:
                return f"【Schema】\n{stripped}"
            return f"【Schema】\n{json.dumps(parsed, ensure_ascii=False, indent=2, sort_keys=True)}"
        return f"【Schema】\n{json.dumps(output_schema, ensure_ascii=False, indent=2, sort_keys=True)}"

    def _format_suggestions(self, raw_value: object) -> str:
        if not isinstance(raw_value, list):
            return "- 保持结构稳定"
        suggestions = [item for item in raw_value if isinstance(item, str)]
        if not suggestions:
            return "- 保持结构稳定"
        return "\n".join(f"- {item}" for item in suggestions[:3])
