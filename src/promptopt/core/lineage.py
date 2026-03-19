"""Lineage tracking for prompt versions."""

from datetime import datetime

from pydantic import BaseModel, Field


class ParentInfo(BaseModel):
    """Information about parent candidate.
    
    Attributes:
        parent_id: ID of parent candidate
        change_type: Type of change made
        diff: Summary of changes
    """
    parent_id: str
    change_type: str
    diff: str | None = None


class Lineage(BaseModel):
    """Tracks the ancestry of a candidate prompt.
    
    Attributes:
        candidate_id: ID of the candidate
        ancestors: List of ancestor candidate IDs (oldest first)
        parent: Direct parent info
        created_at: When this version was created
    """
    candidate_id: str
    ancestors: list[str] = Field(default_factory=list)
    parent: ParentInfo | None = None
    created_at: datetime = Field(default_factory=datetime.now)
    
    def add_parent(self, parent_id: str, change_type: str, diff: str | None = None) -> None:
        """Add a parent to the lineage."""
        self.parent = ParentInfo(
            parent_id=parent_id,
            change_type=change_type,
            diff=diff
        )
        if parent_id not in self.ancestors:
            self.ancestors.append(parent_id)
