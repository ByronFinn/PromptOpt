"""Task, Dataset, and Split models."""

from __future__ import annotations

from enum import StrEnum
from pathlib import Path

import yaml
from pydantic import BaseModel, Field


class Split(StrEnum):
    """Dataset split type."""
    DEV = "dev"
    TEST = "test"
    TRAIN = "train"


class Dataset(BaseModel):
    """Dataset configuration for a task.
    
    Attributes:
        name: Dataset identifier
        path: Path to dataset file (JSON, YAML, CSV)
        split_field: Field name containing split information
    """
    name: str
    path: str
    split_field: str = "split"
    description: str | None = None


class Task(BaseModel):
    """Task definition.
    
    Attributes:
        name: Task identifier
        description: Task description
        dataset: Dataset configuration
        prompt_template: Template string with {input} placeholder
        output_schema: Expected output JSON schema (optional)
        evaluation_metrics: List of metrics to compute
    """
    name: str
    description: str
    dataset: Dataset
    prompt_template: str = Field(
        description="Prompt template with {input} placeholder"
    )
    output_schema: str | None = None
    evaluation_metrics: list[str] = Field(default_factory=lambda: ["exact_match", "f1"])
    
    def format_prompt(self, input_text: str) -> str:
        """Format the prompt template with input text."""
        return self.prompt_template.replace("{input}", input_text)

    @classmethod
    def from_yaml(cls, path: str | Path) -> Task:
        """Load a Task from a YAML file.
        
        Args:
            path: Path to the task.yaml file.
        
        Returns:
            A Task instance parsed from the YAML file.
        
        Raises:
            FileNotFoundError: If the YAML file doesn't exist.
            ValueError: If the YAML content is invalid.
        """
        file_path = Path(path)
        if not file_path.exists():
            raise FileNotFoundError(f"Task file not found: {path}")
        
        with open(file_path, encoding="utf-8") as f:
            data = yaml.safe_load(f)
        
        if not isinstance(data, dict):
            raise ValueError("Task YAML must contain a dictionary at the root.")
        
        return cls.model_validate(data)
