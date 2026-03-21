"""Candidate prompt models."""

from __future__ import annotations

from datetime import datetime
from pathlib import Path

import yaml
from pydantic import BaseModel, ConfigDict, Field


class CandidateMetadata(BaseModel):
    """Metadata about how a candidate was created.
    
    Attributes:
        strategy: Optimization strategy used (rewrite, fewshot, contract)
        parent_id: ID of parent candidate (if any)
        teacher_model: Model used for generation
        generation_params: Parameters used for generation
    """
    strategy: str
    parent_id: str | None = None
    teacher_model: str | None = None
    generation_params: dict[str, object] = Field(default_factory=dict)


class Candidate(BaseModel):
    """A candidate prompt version.
    
    Attributes:
        id: Unique identifier
        name: Human-readable name
        prompt: The actual prompt text
        description: Optional description
        metadata: Creation metadata
        created_at: Creation timestamp
    """
    id: str
    name: str
    prompt: str
    description: str | None = None
    metadata: CandidateMetadata = Field(default_factory=lambda: CandidateMetadata(strategy="baseline"))
    created_at: datetime = Field(default_factory=datetime.now)

    model_config = ConfigDict(ser_json_timedelta="iso8601")

    @classmethod
    def from_yaml(cls, path: str | Path) -> Candidate:
        """Load a Candidate from a YAML file.

        Args:
            path: Path to the candidate.yaml file.

        Returns:
            A Candidate instance parsed from the YAML file.

        Raises:
            FileNotFoundError: If the YAML file doesn't exist.
            ValueError: If the YAML content is invalid.
        """
        file_path = Path(path)
        if not file_path.exists():
            raise FileNotFoundError(f"Candidate file not found: {path}")

        with open(file_path, encoding="utf-8") as f:
            data = yaml.safe_load(f)

        if not isinstance(data, dict):
            raise ValueError("Candidate YAML must contain a mapping at the root.")

        return cls.model_validate(data)
