"""FastAPI application for PromptOpt Web UI."""

from __future__ import annotations

import difflib
import json
from pathlib import Path
from typing import Literal

from fastapi import FastAPI, HTTPException
from fastapi.responses import FileResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field

from promptopt.cli.reporting import to_jsonable
from promptopt.core import discover_project_config
from promptopt.diagnostics import DiagnosticsAnalyzer
from promptopt.storage import Database, get_db
from promptopt.storage.models import (
    CandidateModel,
    LineageModel,
    PromptRegistryEntryModel,
    RunModel,
    SampleResultModel,
)


def create_app(db_path: str | None = None) -> FastAPI:
    """Create the FastAPI application for PromptOpt Web UI."""
    app = FastAPI(title="PromptOpt Web", version="0.1.0")
    static_dir = Path(__file__).with_name("static")
    app.mount("/static", StaticFiles(directory=static_dir), name="static")
    analyzer = DiagnosticsAnalyzer()

    def get_database() -> Database:
        if db_path is not None:
            return get_db(db_path)
        project_config = discover_project_config([Path.cwd()])
        return get_db(project_config.db_path if project_config else None)

    @app.get("/")
    def index() -> FileResponse:
        return FileResponse(static_dir / "index.html")

    @app.get("/api/runs")
    def list_runs(limit: int = 50) -> dict[str, object]:
        db = get_database()
        with db.session() as session:
            runs = session.query(RunModel).order_by(RunModel.created_at.desc()).limit(limit).all()
            payload = []
            for run in runs:
                lineage = session.get(LineageModel, run.candidate_id)
                payload.append(
                    {
                        "id": run.id,
                        "task_id": run.task_id,
                        "candidate_id": run.candidate_id,
                        "model_name": run.model_name,
                        "split": run.split,
                        "status": run.status,
                        "accuracy": run.accuracy,
                        "total_samples": run.total_samples,
                        "created_at": run.created_at,
                        "completed_at": run.completed_at,
                        "parent_id": lineage.parent_id if lineage is not None else None,
                        "change_type": lineage.change_type if lineage is not None else None,
                        "has_diff": bool(lineage and lineage.diff),
                    }
                )
        return {"runs": to_jsonable(payload)}

    @app.get("/api/runs/{run_id}")
    def get_run(run_id: str) -> dict[str, object]:
        db = get_database()
        with db.session() as session:
            run = session.get(RunModel, run_id)
            if run is None:
                raise HTTPException(status_code=404, detail="run not found")
            candidate = session.get(CandidateModel, run.candidate_id)
            lineage = session.get(LineageModel, run.candidate_id)
            aggregate_metrics = _parse_metrics(run.aggregate_metrics_json)
        return {
            "run": to_jsonable(
                {
                    "id": run.id,
                    "task_id": run.task_id,
                    "task_path": run.task_path,
                    "candidate_id": run.candidate_id,
                    "candidate_path": run.candidate_path,
                    "dataset_path": run.dataset_path,
                    "model_name": run.model_name,
                    "split": run.split,
                    "status": run.status,
                    "accuracy": run.accuracy,
                    "aggregate_metrics": aggregate_metrics,
                    "latency_ms": run.latency_ms,
                    "cost": run.cost,
                    "created_at": run.created_at,
                    "completed_at": run.completed_at,
                }
            ),
            "candidate": _candidate_payload(candidate),
            "lineage": _lineage_payload(lineage),
        }

    @app.get("/api/runs/{run_id}/diagnostics")
    def get_run_diagnostics(run_id: str, top_k: int = 10) -> dict[str, object]:
        db = get_database()
        with db.session() as session:
            run = session.get(RunModel, run_id)
            if run is None:
                raise HTTPException(status_code=404, detail="run not found")
            sample_results = (
                session.query(SampleResultModel)
                .filter(SampleResultModel.run_id == run_id)
                .order_by(SampleResultModel.sample_id.asc())
                .all()
            )
            report = analyzer.analyze_run(run, sample_results, top_k=top_k)
            lineage = session.get(LineageModel, run.candidate_id)
        return {
            "report": to_jsonable(report),
            "prompt_diff": lineage.diff if lineage is not None else None,
        }

    @app.get("/api/compare")
    def compare_runs(baseline_run_id: str, candidate_run_id: str, top_k: int = 20) -> dict[str, object]:
        db = get_database()
        with db.session() as session:
            baseline_run = session.get(RunModel, baseline_run_id)
            candidate_run = session.get(RunModel, candidate_run_id)
            if baseline_run is None or candidate_run is None:
                raise HTTPException(status_code=404, detail="baseline or candidate run not found")
            baseline_samples = (
                session.query(SampleResultModel)
                .filter(SampleResultModel.run_id == baseline_run_id)
                .order_by(SampleResultModel.sample_id.asc())
                .all()
            )
            candidate_samples = (
                session.query(SampleResultModel)
                .filter(SampleResultModel.run_id == candidate_run_id)
                .order_by(SampleResultModel.sample_id.asc())
                .all()
            )
            diff_report = analyzer.compare_runs(
                baseline_run,
                baseline_samples,
                candidate_run,
                candidate_samples,
                top_k=top_k,
            )
            regressed_slices = analyzer.detect_slice_regressions(
                baseline_samples,
                candidate_samples,
            )
            baseline_candidate = session.get(CandidateModel, baseline_run.candidate_id)
            candidate_model = session.get(CandidateModel, candidate_run.candidate_id)
            prompt_diff = None
            if baseline_candidate is not None and candidate_model is not None:
                prompt_diff = _build_prompt_diff(
                    baseline_run.candidate_id,
                    baseline_candidate.prompt,
                    candidate_run.candidate_id,
                    candidate_model.prompt,
                )
        return {
            "diff_report": to_jsonable(diff_report),
            "regressed_slices": to_jsonable(regressed_slices),
            "prompt_diff": prompt_diff,
        }

    @app.get("/api/registry")
    def list_registry_entries() -> dict[str, object]:
        db = get_database()
        with db.session() as session:
            entries = session.query(PromptRegistryEntryModel).order_by(PromptRegistryEntryModel.created_at.desc()).all()
        return {"entries": [_registry_entry_payload(entry) for entry in entries]}

    @app.post("/api/registry")
    def create_registry_entry(request: RegistryCreateRequest) -> dict[str, object]:
        db = get_database()
        with db.session() as session:
            candidate = session.get(CandidateModel, request.candidate_id)
            if candidate is None:
                raise HTTPException(status_code=404, detail="candidate not found")
            entry = session.get(PromptRegistryEntryModel, request.candidate_id)
            if entry is None:
                entry = PromptRegistryEntryModel(
                    candidate_id=request.candidate_id,
                    registry_key=request.registry_key,
                    state=request.state,
                    source_run_id=request.source_run_id,
                    verify_run_id=request.verify_run_id,
                )
                session.add(entry)
            else:
                entry.registry_key = request.registry_key
                entry.state = request.state
                entry.source_run_id = request.source_run_id
                entry.verify_run_id = request.verify_run_id
            return {"entry": _registry_entry_payload(entry)}

    @app.post("/api/registry/{candidate_id}/review")
    def submit_registry_entry_for_review(candidate_id: str) -> dict[str, object]:
        return _transition_registry_state(
            get_database(),
            candidate_id,
            target_state="review",
        )

    @app.post("/api/registry/{candidate_id}/approve")
    def approve_registry_entry(candidate_id: str, actor: str = "system") -> dict[str, object]:
        return _transition_registry_state(
            get_database(),
            candidate_id,
            target_state="approved",
            actor=actor,
        )

    @app.post("/api/registry/{candidate_id}/deploy")
    def deploy_registry_entry(candidate_id: str, actor: str = "system") -> dict[str, object]:
        return _transition_registry_state(
            get_database(),
            candidate_id,
            target_state="deployed",
            actor=actor,
        )

    return app


class RegistryCreateRequest(BaseModel):
    candidate_id: str
    registry_key: str
    state: Literal["draft", "review", "approved", "deployed"] = Field(default="draft")
    source_run_id: str | None = None
    verify_run_id: str | None = None


def _transition_registry_state(
    db: Database,
    candidate_id: str,
    *,
    target_state: str,
    actor: str | None = None,
) -> dict[str, object]:
    with db.session() as session:
        entry = session.get(PromptRegistryEntryModel, candidate_id)
        if entry is None:
            raise HTTPException(status_code=404, detail="registry entry not found")

        if target_state == "review" and entry.state != "draft":
            raise HTTPException(status_code=400, detail="only draft can enter review")
        if target_state == "approved" and entry.state != "review":
            raise HTTPException(status_code=400, detail="only review can be approved")
        if target_state == "deployed" and entry.state != "approved":
            raise HTTPException(status_code=400, detail="only approved can be deployed")

        if target_state == "deployed":
            deployed_entries = (
                session.query(PromptRegistryEntryModel)
                .filter(PromptRegistryEntryModel.registry_key == entry.registry_key)
                .filter(PromptRegistryEntryModel.state == "deployed")
                .all()
            )
            for deployed_entry in deployed_entries:
                deployed_entry.state = "approved"

        entry.state = target_state
        if target_state == "approved":
            entry.approved_by = actor
        if target_state == "deployed":
            entry.deployed_by = actor

    return {"entry": _registry_entry_payload(entry)}


def _parse_metrics(raw_metrics: str) -> dict[str, float]:
    try:
        parsed: object = json.loads(raw_metrics)
    except json.JSONDecodeError:
        return {}
    if not isinstance(parsed, dict):
        return {}
    metrics: dict[str, float] = {}
    for key, value in parsed.items():
        if isinstance(value, bool):
            continue
        if isinstance(value, (int, float)):
            metrics[str(key)] = float(value)
    return metrics


def _build_prompt_diff(
    from_label: str,
    from_prompt: str,
    to_label: str,
    to_prompt: str,
) -> str:
    diff_lines = difflib.unified_diff(
        from_prompt.splitlines(),
        to_prompt.splitlines(),
        fromfile=from_label,
        tofile=to_label,
        lineterm="",
    )
    return "\n".join(diff_lines)


def _candidate_payload(candidate: CandidateModel | None) -> dict[str, object] | None:
    if candidate is None:
        return None
    return {
        "id": candidate.id,
        "name": candidate.name,
        "prompt": candidate.prompt,
        "description": candidate.description,
        "strategy": candidate.strategy,
        "parent_id": candidate.parent_id,
        "teacher_model": candidate.teacher_model,
        "created_at": candidate.created_at,
    }


def _lineage_payload(lineage: LineageModel | None) -> dict[str, object] | None:
    if lineage is None:
        return None
    return {
        "candidate_id": lineage.candidate_id,
        "ancestors": lineage.ancestors,
        "parent_id": lineage.parent_id,
        "change_type": lineage.change_type,
        "diff": lineage.diff,
        "created_at": lineage.created_at,
    }


def _registry_entry_payload(entry: PromptRegistryEntryModel) -> dict[str, object]:
    return {
        "candidate_id": entry.candidate_id,
        "registry_key": entry.registry_key,
        "state": entry.state,
        "source_run_id": entry.source_run_id,
        "verify_run_id": entry.verify_run_id,
        "approved_by": entry.approved_by,
        "approved_at": entry.approved_at,
        "deployed_by": entry.deployed_by,
        "deployed_at": entry.deployed_at,
        "created_at": entry.created_at,
        "updated_at": entry.updated_at,
    }
