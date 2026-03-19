"""Task, Dataset, and Split models."""

from enum import Enum

from pydantic import BaseModel, Field


class Split(str, Enum):
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
        return self.prompt_template.format(input=input_text)
