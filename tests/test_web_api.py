"""Tests for the PromptOpt Web API and prompt registry state flow."""

from __future__ import annotations

import json
from pathlib import Path

from fastapi.testclient import TestClient

from promptopt.storage.database import Database, reset_db
from promptopt.storage.models import (
    CandidateModel,
    PromptRegistryEntryModel,
    RunModel,
    SampleResultModel,
)
from promptopt.web import create_app


def test_web_api_exposes_runs_diagnostics_compare_and_registry(tmp_path: Path) -> None:
    reset_db()
    db_path = tmp_path / "web.db"
    _seed_web_db(Database(str(db_path)))
    client = TestClient(create_app(str(db_path)))

    runs_response = client.get("/api/runs")
    assert runs_response.status_code == 200
    runs_payload = runs_response.json()
    assert len(runs_payload["runs"]) == 2

    diagnostics_response = client.get("/api/runs/run_web_candidate/diagnostics")
    assert diagnostics_response.status_code == 200
    diagnostics_payload = diagnostics_response.json()
    assert diagnostics_payload["report"]["run_id"] == "run_web_candidate"

    compare_response = client.get(
        "/api/compare",
        params={"baseline_run_id": "run_web_baseline", "candidate_run_id": "run_web_candidate"},
    )
    assert compare_response.status_code == 200
    compare_payload = compare_response.json()
    assert compare_payload["diff_report"]["baseline_run_id"] == "run_web_baseline"
    assert compare_payload["prompt_diff"]

    registry_create = client.post(
        "/api/registry",
        json={
            "candidate_id": "rewrite_001",
            "registry_key": "medical_json_extraction",
            "state": "draft",
            "source_run_id": "run_web_candidate",
            "verify_run_id": "run_web_candidate",
        },
    )
    assert registry_create.status_code == 200

    review_response = client.post("/api/registry/rewrite_001/review")
    assert review_response.status_code == 200
    approve_response = client.post("/api/registry/rewrite_001/approve", params={"actor": "alice"})
    assert approve_response.status_code == 200
    deploy_response = client.post("/api/registry/rewrite_001/deploy", params={"actor": "alice"})
    assert deploy_response.status_code == 200

    registry_response = client.get("/api/registry")
    assert registry_response.status_code == 200
    entries = registry_response.json()["entries"]
    assert entries[0]["state"] == "deployed"



def _seed_web_db(db: Database) -> None:
    db.create_tables()
    with db.session() as session:
        session.add_all(
            [
                CandidateModel(
                    id="baseline_001",
                    name="baseline",
                    prompt="{input}",
                    strategy="baseline",
                ),
                CandidateModel(
                    id="rewrite_001",
                    name="rewrite_v1",
                    prompt="你是一名结构化抽取专家。\n\n{input}",
                    strategy="rewrite",
                    parent_id="baseline_001",
                ),
            ]
        )
        session.add_all(
            [
                RunModel(
                    id="run_web_baseline",
                    task_id="medical_json_extraction",
                    candidate_id="baseline_001",
                    model_name="fake/model",
                    split="dev",
                    status="completed",
                    total_samples=2,
                    correct_count=1,
                    accuracy=0.5,
                    aggregate_metrics_json=json.dumps({"exact_match": 0.5, "json_validity": 0.5}, ensure_ascii=False, sort_keys=True),
                ),
                RunModel(
                    id="run_web_candidate",
                    task_id="medical_json_extraction",
                    candidate_id="rewrite_001",
                    model_name="fake/model",
                    split="dev",
                    status="completed",
                    total_samples=2,
                    correct_count=2,
                    accuracy=1.0,
                    aggregate_metrics_json=json.dumps({"exact_match": 1.0, "json_validity": 1.0}, ensure_ascii=False, sort_keys=True),
                ),
            ]
        )
        session.add_all(
            [
                SampleResultModel(
                    run_id="run_web_baseline",
                    sample_id="sample_001",
                    input_text="患者咳嗽 3 天。",
                    expected_output=json.dumps({"疾病": "感冒", "症状": ["咳嗽"]}, ensure_ascii=False, sort_keys=True),
                    actual_output=json.dumps({"疾病": "感冒", "症状": ["咳嗽"]}, ensure_ascii=False, sort_keys=True),
                    is_correct=True,
                    metrics_json=json.dumps({"exact_match": 1.0, "json_validity": 1.0}, ensure_ascii=False, sort_keys=True),
                ),
                SampleResultModel(
                    run_id="run_web_baseline",
                    sample_id="sample_002",
                    input_text="患者血糖升高 3 年。",
                    expected_output=json.dumps({"疾病": "糖尿病", "症状": ["血糖升高"]}, ensure_ascii=False, sort_keys=True),
                    actual_output="not-json",
                    is_correct=False,
                    metrics_json=json.dumps({"exact_match": 0.0, "json_validity": 0.0}, ensure_ascii=False, sort_keys=True),
                ),
                SampleResultModel(
                    run_id="run_web_candidate",
                    sample_id="sample_001",
                    input_text="患者咳嗽 3 天。",
                    expected_output=json.dumps({"疾病": "感冒", "症状": ["咳嗽"]}, ensure_ascii=False, sort_keys=True),
                    actual_output=json.dumps({"疾病": "感冒", "症状": ["咳嗽"]}, ensure_ascii=False, sort_keys=True),
                    is_correct=True,
                    metrics_json=json.dumps({"exact_match": 1.0, "json_validity": 1.0}, ensure_ascii=False, sort_keys=True),
                ),
                SampleResultModel(
                    run_id="run_web_candidate",
                    sample_id="sample_002",
                    input_text="患者血糖升高 3 年。",
                    expected_output=json.dumps({"疾病": "糖尿病", "症状": ["血糖升高"]}, ensure_ascii=False, sort_keys=True),
                    actual_output=json.dumps({"疾病": "糖尿病", "症状": ["血糖升高"]}, ensure_ascii=False, sort_keys=True),
                    is_correct=True,
                    metrics_json=json.dumps({"exact_match": 1.0, "json_validity": 1.0}, ensure_ascii=False, sort_keys=True),
                ),
            ]
        )
        session.add(
            PromptRegistryEntryModel(
                candidate_id="baseline_001",
                registry_key="medical_json_extraction",
                state="deployed",
                source_run_id="run_web_baseline",
                verify_run_id="run_web_baseline",
            )
        )
