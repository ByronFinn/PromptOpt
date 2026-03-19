"""Candidate prompt models."""

from datetime import datetime
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field


class CandidateMetadata(BaseModel):
    """Metadata about how a candidate was created.
    
    Attributes:
        strategy: Optimization strategy used (rewrite, fewshot, contract)
        parent_id: ID of parent candidate (if any)
        teacher_model: Model used for generation
        generation_params: Parameters used for generation
    """
    strategy: Literal["rewrite", "fewshot", "contract", "baseline"]
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
