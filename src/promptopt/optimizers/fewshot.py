"""Few-shot optimizer."""

import json
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
        del eval_results, task_description
        sample_results = kwargs.get("sample_results")
        max_examples = self._coerce_positive_int(kwargs.get("max_examples"), default=2)
        selected_examples = self._select_examples(sample_results, max_examples=max_examples)
        if not selected_examples:
            return []

        example_block = self._build_example_block(selected_examples)
        return [
            (
                f"{current_prompt}\n\n"
                "请严格参考以下 few-shot 示例的输入输出风格。\n\n"
                f"{example_block}"
            ),
            (
                f"{current_prompt}\n\n"
                "在生成最终答案前，请先隐式对照以下示例格式进行检查。\n\n"
                f"{example_block}"
            ),
        ]

    def _select_examples(
        self,
        raw_sample_results: object,
        *,
        max_examples: int,
    ) -> list[tuple[str, str]]:
        if not isinstance(raw_sample_results, list):
            return []

        examples: list[tuple[str, str]] = []
        for item in raw_sample_results:
            input_text = getattr(item, "input_text", None)
            expected_output = getattr(item, "expected_output", None)
            is_correct = getattr(item, "is_correct", None)
            if not isinstance(input_text, str) or not isinstance(is_correct, bool):
                continue
            if not is_correct:
                continue

            serialized_output = self._serialize_output(expected_output)
            examples.append((input_text, serialized_output))
            if len(examples) >= max_examples:
                break
        return examples

    def _build_example_block(self, examples: list[tuple[str, str]]) -> str:
        lines: list[str] = []
        for index, (input_text, output_text) in enumerate(examples, start=1):
            lines.append(f"示例 {index}：")
            lines.append(f"输入：{input_text}")
            lines.append(f"输出：{output_text}")
            lines.append("")
        return "\n".join(lines).strip()

    def _serialize_output(self, payload: object) -> str:
        if isinstance(payload, str):
            return payload
        return json.dumps(payload, ensure_ascii=False, sort_keys=True)

    def _coerce_positive_int(self, raw_value: object, *, default: int) -> int:
        if isinstance(raw_value, int) and raw_value > 0:
            return raw_value
        return default
