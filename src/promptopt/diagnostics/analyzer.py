"""Diagnostics analyzer for failure analysis."""

from collections.abc import Mapping

from pydantic import BaseModel


class DiagnosticsAnalyzer(BaseModel):
    """Analyzes evaluation failures to identify patterns.
    
    Provides insights into:
    - Error types and frequencies
    - Slice-based analysis (e.g., negation, numbers)
    - Failure mode attribution
    """
    
    def analyze(
        self,
        eval_results: Mapping[str, object],
    ) -> dict[str, object]:
        """Analyze evaluation results for failure patterns.
        
        Args:
            eval_results: Dictionary containing evaluation results
            
        Returns:
            Analysis report with failure patterns and suggestions
        """
        results_list = eval_results.get("results", [])
        total = eval_results.get("total", 0) if isinstance(eval_results.get("total"), int) else 0
        correct = eval_results.get("correct", 0) if isinstance(eval_results.get("correct"), int) else 0
        accuracy = eval_results.get("accuracy", 0.0) if isinstance(eval_results.get("accuracy"), float) else 0.0
        
        # Placeholder - actual implementation would analyze errors
        report: dict[str, object] = {
            "total_samples": total,
            "correct_count": correct,
            "accuracy": accuracy,
            "error_types": {},
            "slice_analysis": {},
            "suggestions": [],
        }
        
        # Analyze sample-level results
        error_types: dict[str, int] = {}
        if isinstance(results_list, list):
            for result in results_list:
                if isinstance(result, dict) and "error" in result:
                    error_type = type(result["error"]).__name__
                    error_types[error_type] = error_types.get(error_type, 0) + 1
        
        report["error_types"] = error_types
        
        return report
    
    def suggest_improvements(
        self,
        analysis: Mapping[str, object],
    ) -> list[str]:
        """Suggest improvements based on analysis.
        
        Args:
            analysis: Analysis report from analyze()
            
        Returns:
            List of suggested improvements
        """
        suggestions = []
        
        accuracy = analysis.get("accuracy", 0.0)
        if isinstance(accuracy, (int, float)) and accuracy < 0.8:
            suggestions.append("准确率较低，建议优化prompt指令")
        
        if analysis.get("error_types"):
            suggestions.append("存在解析错误，建议强化输出格式约束")
        
        return suggestions
