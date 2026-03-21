"""Base optimizer interface."""

from __future__ import annotations

import asyncio
import json
from abc import ABC, abstractmethod
from collections.abc import Mapping

from pydantic import BaseModel

from promptopt.models import ModelAdapter


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
        teacher_adapter = kwargs.get("teacher_adapter")
        if teacher_adapter is None or not isinstance(teacher_adapter, ModelAdapter):
            raise ValueError("RewriteOptimizer 需要 teacher_adapter 才能生成候选。")

        num_candidates = self._coerce_positive_int(kwargs.get("num_candidates"), default=3)
        temperature = self._coerce_float(kwargs.get("temperature"), default=0.7)
        max_tokens = self._coerce_positive_int(kwargs.get("max_tokens"), default=2048)

        teacher_prompt = self._build_teacher_prompt(
            current_prompt=current_prompt,
            eval_results=eval_results,
            task_description=task_description,
            num_candidates=num_candidates,
        )
        response = asyncio.run(
            teacher_adapter.generate(
                teacher_prompt,
                temperature=temperature,
                max_tokens=max_tokens,
            )
        )
        parsed_candidates = self._parse_teacher_response(response)

        unique_candidates: list[str] = []
        seen: set[str] = set()
        for candidate_prompt in parsed_candidates + self._fallback_rewrites(
            current_prompt=current_prompt,
            task_description=task_description,
            eval_results=eval_results,
        ):
            normalized_prompt = candidate_prompt.strip()
            if not normalized_prompt or normalized_prompt == current_prompt.strip():
                continue
            if normalized_prompt in seen:
                continue
            seen.add(normalized_prompt)
            unique_candidates.append(normalized_prompt)
            if len(unique_candidates) >= num_candidates:
                break

        return unique_candidates

    def _build_teacher_prompt(
        self,
        *,
        current_prompt: str,
        eval_results: Mapping[str, object],
        task_description: str,
        num_candidates: int,
    ) -> str:
        suggestions = self._format_suggestions(eval_results.get("suggestions"))
        top_failures = self._format_top_failures(eval_results.get("top_failures"))
        aggregate_metrics = self._format_metrics(eval_results.get("aggregate_metrics"))

        return f"""你是一名 Prompt 优化专家。请基于给定任务、当前 prompt 和失败分析，生成 {num_candidates} 个更优的 instruction rewrite 候选。

要求：
1. 保留任务目标，不要改变输入输出语义。
2. 优先修复失败分析中暴露的问题。
3. 保留 `{{input}}` 占位符。
4. 只返回 JSON，不要解释。
5. JSON 格式必须为：
{{
  "candidates": [
    "候选 prompt 1",
    "候选 prompt 2"
  ]
}}

任务描述：
{task_description}

当前 prompt：
{current_prompt}

聚合指标：
{aggregate_metrics}

优化建议：
{suggestions}

典型失败样本：
{top_failures}
"""

    def _parse_teacher_response(self, response: str) -> list[str]:
        stripped = response.strip()
        if stripped.startswith("```"):
            stripped = self._strip_code_fence(stripped)

        try:
            parsed: object = json.loads(stripped)
        except json.JSONDecodeError:
            return []

        if isinstance(parsed, dict):
            candidates = parsed.get("candidates")
        else:
            candidates = parsed

        if not isinstance(candidates, list):
            return []

        candidate_prompts: list[str] = []
        for item in candidates:
            if isinstance(item, str):
                candidate_prompts.append(item)
        return candidate_prompts

    def _fallback_rewrites(
        self,
        *,
        current_prompt: str,
        task_description: str,
        eval_results: Mapping[str, object],
    ) -> list[str]:
        first_suggestion = ""
        suggestions = eval_results.get("suggestions")
        if isinstance(suggestions, list) and suggestions:
            first_item = suggestions[0]
            if isinstance(first_item, str):
                first_suggestion = first_item

        return [
            (
                "请严格遵循以下任务要求完成输出。"
                f"\n\n任务目标：{task_description}\n\n"
                f"重点改进：{first_suggestion or '减少失败样本并提高格式稳定性。'}\n\n"
                f"{current_prompt}"
            ),
            (
                "你是一个擅长结构化信息抽取的专业助手。"
                f"\n\n任务描述：{task_description}\n"
                "请先理解输入，再按要求直接生成最终结果。\n\n"
                f"{current_prompt}"
            ),
            (
                "请在保证语义准确的前提下，严格遵守输出要求。"
                "\n- 保留所有关键信息\n- 不要输出额外解释\n"
                f"- 优先修复：{first_suggestion or '格式错误与语义误判'}\n\n"
                f"{current_prompt}"
            ),
        ]

    def _format_suggestions(self, raw_value: object) -> str:
        if not isinstance(raw_value, list):
            return "- 无"
        suggestions = [item for item in raw_value if isinstance(item, str)]
        if not suggestions:
            return "- 无"
        return "\n".join(f"- {item}" for item in suggestions)

    def _format_top_failures(self, raw_value: object) -> str:
        if not isinstance(raw_value, list):
            return "- 无"

        lines: list[str] = []
        for item in raw_value[:3]:
            if not isinstance(item, object):
                continue
            sample_id = getattr(item, "sample_id", "unknown")
            reason = getattr(item, "reason", "")
            input_text = getattr(item, "input_text", "")
            if not isinstance(sample_id, str) or not isinstance(reason, str):
                continue
            input_preview = input_text if isinstance(input_text, str) else ""
            lines.append(
                f"- {sample_id}: {reason} | 输入片段: {input_preview[:80]}"
            )

        return "\n".join(lines) if lines else "- 无"

    def _format_metrics(self, raw_value: object) -> str:
        if not isinstance(raw_value, dict):
            return "{}"
        serializable_metrics = {
            str(key): float(value)
            for key, value in raw_value.items()
            if isinstance(value, (int, float)) and not isinstance(value, bool)
        }
        return json.dumps(serializable_metrics, ensure_ascii=False, sort_keys=True)

    def _strip_code_fence(self, response: str) -> str:
        stripped = response.strip()
        if not stripped.startswith("```"):
            return stripped
        lines = stripped.splitlines()
        if len(lines) >= 3:
            return "\n".join(lines[1:-1]).strip()
        return stripped.strip("`").strip()

    def _coerce_positive_int(self, raw_value: object, *, default: int) -> int:
        if isinstance(raw_value, int) and raw_value > 0:
            return raw_value
        return default

    def _coerce_float(self, raw_value: object, *, default: float) -> float:
        if isinstance(raw_value, bool):
            return default
        if isinstance(raw_value, (int, float)):
            return float(raw_value)
        return default
