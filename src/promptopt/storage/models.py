"""SQLAlchemy models for PromptOpt storage."""

from datetime import datetime

from sqlalchemy import Boolean, DateTime, Float, ForeignKey, Integer, String, Text
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column, relationship


class Base(DeclarativeBase):
    """Base class for SQLAlchemy models."""
    pass


class CandidateModel(Base):
    """Candidate prompt storage model."""
    
    __tablename__ = "candidates"
    
    id: Mapped[str] = mapped_column(String(100), primary_key=True)
    name: Mapped[str] = mapped_column(String(200), nullable=False)
    prompt: Mapped[str] = mapped_column(Text, nullable=False)
    description: Mapped[str | None] = mapped_column(Text, nullable=True)
    strategy: Mapped[str] = mapped_column(String(50), nullable=False, default="baseline")
    parent_id: Mapped[str | None] = mapped_column(String(100), nullable=True)
    teacher_model: Mapped[str | None] = mapped_column(String(100), nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=datetime.now)
    
    # Relationships
    runs: Mapped[list[RunModel]] = relationship("RunModel", back_populates="candidate")
    
    def __repr__(self) -> str:
        return f"<CandidateModel(id={self.id}, name={self.name})>"


class RunModel(Base):
    """Run execution storage model."""
    
    __tablename__ = "runs"
    
    id: Mapped[str] = mapped_column(String(100), primary_key=True)
    task_id: Mapped[str] = mapped_column(String(100), nullable=False)
    task_path: Mapped[str | None] = mapped_column(Text, nullable=True)
    candidate_id: Mapped[str] = mapped_column(String(100), ForeignKey("candidates.id"), nullable=False)
    candidate_path: Mapped[str | None] = mapped_column(Text, nullable=True)
    dataset_path: Mapped[str | None] = mapped_column(Text, nullable=True)
    model_name: Mapped[str | None] = mapped_column(String(200), nullable=True)
    split: Mapped[str] = mapped_column(String(20), nullable=False, default="dev")
    status: Mapped[str] = mapped_column(String(20), nullable=False, default="pending")
    total_samples: Mapped[int] = mapped_column(Integer, default=0)
    correct_count: Mapped[int] = mapped_column(Integer, default=0)
    accuracy: Mapped[float] = mapped_column(Float, default=0.0)
    aggregate_metrics_json: Mapped[str] = mapped_column(Text, default="{}")
    duration_seconds: Mapped[float] = mapped_column(Float, default=0.0)
    cost: Mapped[float] = mapped_column(Float, default=0.0)
    latency_ms: Mapped[float] = mapped_column(Float, default=0.0)
    error: Mapped[str | None] = mapped_column(Text, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=datetime.now)
    completed_at: Mapped[datetime | None] = mapped_column(DateTime, nullable=True)
    
    # Relationships
    candidate: Mapped[CandidateModel] = relationship("CandidateModel", back_populates="runs")
    sample_results: Mapped[list[SampleResultModel]] = relationship(
        "SampleResultModel",
        back_populates="run",
        cascade="all, delete-orphan",
    )
    
    def __repr__(self) -> str:
        return f"<RunModel(id={self.id}, status={self.status})>"


class LineageModel(Base):
    """Lineage tracking storage model."""
    
    __tablename__ = "lineages"
    
    candidate_id: Mapped[str] = mapped_column(String(100), ForeignKey("candidates.id"), primary_key=True)
    ancestors: Mapped[str] = mapped_column(Text, nullable=False, default="[]")  # JSON list
    parent_id: Mapped[str | None] = mapped_column(String(100), nullable=True)
    change_type: Mapped[str | None] = mapped_column(String(50), nullable=True)
    diff: Mapped[str | None] = mapped_column(Text, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=datetime.now)
    
    def __repr__(self) -> str:
        return f"<LineageModel(candidate_id={self.candidate_id})>"


class PromptRegistryEntryModel(Base):
    """Prompt registry and approval-flow state model."""

    __tablename__ = "prompt_registry_entries"

    candidate_id: Mapped[str] = mapped_column(String(100), ForeignKey("candidates.id"), primary_key=True)
    registry_key: Mapped[str] = mapped_column(String(200), nullable=False)
    state: Mapped[str] = mapped_column(String(20), nullable=False, default="draft")
    source_run_id: Mapped[str | None] = mapped_column(String(100), nullable=True)
    verify_run_id: Mapped[str | None] = mapped_column(String(100), nullable=True)
    approved_by: Mapped[str | None] = mapped_column(String(100), nullable=True)
    approved_at: Mapped[datetime | None] = mapped_column(DateTime, nullable=True)
    deployed_by: Mapped[str | None] = mapped_column(String(100), nullable=True)
    deployed_at: Mapped[datetime | None] = mapped_column(DateTime, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime, default=datetime.now)
    updated_at: Mapped[datetime] = mapped_column(DateTime, default=datetime.now, onupdate=datetime.now)

    def __repr__(self) -> str:
        return f"<PromptRegistryEntryModel(candidate_id={self.candidate_id}, state={self.state})>"


class SampleResultModel(Base):
    """Per-sample evaluation result storage model."""

    __tablename__ = "run_samples"

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    run_id: Mapped[str] = mapped_column(String(100), ForeignKey("runs.id"), nullable=False)
    sample_id: Mapped[str] = mapped_column(String(100), nullable=False)
    input_text: Mapped[str] = mapped_column(Text, nullable=False)
    expected_output: Mapped[str] = mapped_column(Text, nullable=False)
    actual_output: Mapped[str] = mapped_column(Text, nullable=False)
    is_correct: Mapped[bool] = mapped_column(Boolean, default=False)
    metrics_json: Mapped[str] = mapped_column(Text, default="{}")
    error: Mapped[str | None] = mapped_column(Text, nullable=True)

    run: Mapped[RunModel] = relationship("RunModel", back_populates="sample_results")

    def __repr__(self) -> str:
        return f"<SampleResultModel(run_id={self.run_id}, sample_id={self.sample_id})>"
