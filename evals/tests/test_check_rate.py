"""check_rate.py: floors, --no-fail, SKIPPED accounting, artifacts, redaction."""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

EVALS_DIR = Path(__file__).resolve().parents[1]
if str(EVALS_DIR) not in sys.path:
    sys.path.insert(0, str(EVALS_DIR))
if str(EVALS_DIR / "bin") not in sys.path:
    sys.path.insert(0, str(EVALS_DIR / "bin"))

from check_rate import observed_tokens_of, scrub  # noqa: E402

CHECK_RATE_CLI = EVALS_DIR / "bin" / "check_rate.py"
MODEL_ID = "Qwen/Qwen3-4B-Instruct-2507-FP8"

CORPUS_CASES = [
    {
        "id": "rag-basic",
        "line": "main",
        "prompt": "整理一下我的喂",
        "vars": {},
        "expect": {"plan": ["rag", "llm"]},
        "min_rate": 1.0,
    },
    {
        "id": "weather",
        "line": "main",
        "prompt": "今天天气怎么样",
        "vars": {"mode": {"web_search": "on"}},
        "expect": {"plan": {"on": ["web_search", "llm"]}},
        "min_rate": 0.5,
    },
]

PLAN_MISMATCH = (
    "plan mismatch: expected=[web_search] observed=[llm] raw_plan={\"steps\": [{\"kind\": \"llm\"}]}"
)


def assertion(reason: str, *, passed: bool = True, observed_tokens=None) -> dict:
    component = {"pass": passed, "score": 1.0 if passed else 0.0, "reason": reason}
    if observed_tokens is not None:
        component["metadata"] = {"observed_tokens": observed_tokens}
    return component


def row(case_id: str, *, success: bool, components=(), output: str = '{"status": "completed"}') -> dict:
    return {
        "vars": {"case_id": case_id, "prompt": "…"},
        "success": success,
        "componentResults": list(components),
        "response": {"output": output},
    }


def skipped_row(case_id: str, reason: str) -> dict:
    return row(
        case_id,
        success=True,
        components=[assertion(f"SKIPPED: {reason}", passed=True)],
    )


def write_corpus(tmp_path: Path, cases=None) -> Path:
    path = tmp_path / "cases.yaml"
    path.write_text(yaml.safe_dump({"cases": cases or CORPUS_CASES}, allow_unicode=True), encoding="utf-8")
    return path


def write_results(tmp_path: Path, rows: list[dict], metadata: dict | None = None) -> Path:
    path = tmp_path / "results.json"
    document = {"results": {"version": 3, "results": rows, "stats": {}}}
    document["metadata"] = metadata if metadata is not None else {"model": MODEL_ID}
    path.write_text(json.dumps(document, ensure_ascii=False), encoding="utf-8")
    return path


def run_cli(
    results_path: Path,
    corpus_path: Path,
    artifacts: Path,
    *extra: str,
    env: dict | None = None,
):
    return subprocess.run(
        [
            sys.executable,
            str(CHECK_RATE_CLI),
            str(results_path),
            "--corpus-glob",
            str(corpus_path),
            "--artifacts-dir",
            str(artifacts),
            *extra,
        ],
        capture_output=True,
        text=True,
        env={**os.environ, **(env or {})},
    )


def read_report(artifacts: Path) -> dict:
    return json.loads((artifacts / "report.json").read_text(encoding="utf-8"))


def case_entry(report: dict, case_id: str) -> dict:
    return next(entry for entry in report["cases"] if entry["id"] == case_id)


def test_within_floors_exits_zero_and_writes_artifacts(tmp_path):
    rows = [
        row("rag-basic", success=True, components=[assertion("plan ok")]),
        row("rag-basic", success=True, components=[assertion("plan ok")]),
        row("weather", success=True, components=[assertion("plan ok")]),
        row("weather", success=True, components=[assertion("plan ok")]),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts)

    assert completed.returncode == 0, completed.stderr
    report = read_report(artifacts)
    assert report["overall"] == {
        "rate": 1.0,
        "floor": 0.85,
        "graded": 4,
        "passed": 4,
        "skipped": 0,
        "breach": False,
    }
    assert report["breaches"] == []
    assert report["errors"] == []
    assert report["model"] == MODEL_ID
    assert report["git_revision"] is None or len(report["git_revision"]) == 40
    assert report["exit_code"] == 0
    assert report["baseline_rate"] is None
    assert report["baseline"] == {
        "is_baseline": True,
        "not_run_beyond_prerequisites": [],
        "skipped_beyond_prerequisites": [],
    }
    assert case_entry(report, "rag-basic")["rate"] == 1.0
    summary = (artifacts / "summary.md").read_text(encoding="utf-8")
    assert "| rag-basic | main | 1.00 |" in summary
    assert "## SKIPPED" in summary
    assert "baseline: yes" in summary
    assert "baseline rate not recorded" in summary
    assert (artifacts / "samples" / "rag-basic__1.json").exists()


def test_case_floor_breach_exits_non_zero(tmp_path):
    rows = [
        row("rag-basic", success=True, components=[assertion("plan ok")]),
        row("rag-basic", success=False, components=[assertion(PLAN_MISMATCH, passed=False)]),
        row("rag-basic", success=False, components=[assertion(PLAN_MISMATCH, passed=False)]),
        row("weather", success=True, components=[assertion("plan ok")]),
        row("weather", success=True, components=[assertion("plan ok")]),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(
        write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--min-rate", "0.5"
    )

    assert completed.returncode == 1
    assert "breach: rag-basic" in completed.stdout
    report = read_report(artifacts)
    assert report["breaches"] == ["rag-basic"]
    entry = case_entry(report, "rag-basic")
    assert entry["rate"] == 0.3333
    assert entry["graded"] == 3
    assert entry["failed"] == 2
    assert entry["breach"] is True
    assert case_entry(report, "weather")["breach"] is False


def test_observed_tokens_land_in_the_report(tmp_path):
    rows = [
        row(
            "rag-basic",
            success=False,
            components=[assertion(PLAN_MISMATCH, passed=False)],
        ),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--no-fail")

    assert completed.returncode == 0
    sample = case_entry(read_report(artifacts), "rag-basic")["samples"][0]
    assert sample["observed_tokens"] == ["llm"]
    assert sample["verdict"] == "fail"


def test_observed_tokens_prefer_grader_metadata(tmp_path):
    rows = [
        row(
            "rag-basic",
            success=False,
            components=[
                assertion(PLAN_MISMATCH, passed=False, observed_tokens=["rag", "create_feed"])
            ],
        ),
    ]
    artifacts = tmp_path / "artifacts"
    run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--no-fail")
    sample = case_entry(read_report(artifacts), "rag-basic")["samples"][0]
    assert sample["observed_tokens"] == ["rag", "create_feed"]


def test_no_fail_reports_without_failing(tmp_path):
    rows = [
        row("rag-basic", success=False, components=[assertion(PLAN_MISMATCH, passed=False)]),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(
        write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--no-fail"
    )

    assert completed.returncode == 0
    report = read_report(artifacts)
    assert report["no_fail"] is True
    assert report["breaches"] == ["rag-basic", "<overall>"]
    assert report["overall"]["breach"] is True
    summary = (artifacts / "summary.md").read_text(encoding="utf-8")
    assert "BREACH" in summary


def test_skipped_samples_leave_the_denominator(tmp_path):
    rows = [
        skipped_row("rag-basic", "requires budget_stack"),
        skipped_row("rag-basic", "requires budget_stack"),
        skipped_row("rag-basic", "requires budget_stack"),
        row("weather", success=True, components=[assertion("plan ok")]),
        row("weather", success=True, components=[assertion("plan ok")]),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts)

    assert completed.returncode == 0, completed.stderr
    report = read_report(artifacts)
    entry = case_entry(report, "rag-basic")
    assert entry["rate"] is None
    assert entry["graded"] == 0
    assert entry["skipped"] == 3
    assert entry["breach"] is False
    assert report["overall"] == {
        "rate": 1.0,
        "floor": 0.85,
        "graded": 2,
        "passed": 2,
        "skipped": 3,
        "breach": False,
    }
    assert [item["reason"] for item in report["skipped"]] == ["requires budget_stack"] * 3
    assert all(item["verdict"] == "skipped" for item in entry["samples"])
    summary = (artifacts / "summary.md").read_text(encoding="utf-8")
    assert "`rag-basic` sample 1: requires budget_stack" in summary
    assert "every sample skipped" in summary


def test_case_with_no_samples_is_not_run(tmp_path):
    artifacts = tmp_path / "artifacts"
    rows = [
        row("rag-basic", success=True, components=[assertion("plan ok")]),
    ]
    completed = run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts)

    assert completed.returncode == 0
    report = read_report(artifacts)
    entry = case_entry(report, "weather")
    assert entry["not_run"] is True
    assert entry["rate"] is None
    assert entry["breach"] is False
    assert report["baseline"] == {
        "is_baseline": False,
        "not_run_beyond_prerequisites": ["weather"],
        "skipped_beyond_prerequisites": [],
    }
    summary = (artifacts / "summary.md").read_text(encoding="utf-8")
    assert "## Not run" in summary
    assert "beyond declared prerequisites; the run is not a baseline" in summary
    assert "PARTIAL RUN (not a baseline)" in summary
    assert "the overall rate is not evidence the suite passed" in summary
    assert "within floor" not in summary
    assert "partial run" in completed.stderr


def test_not_run_case_with_declared_prerequisite_keeps_baseline(tmp_path):
    budget_case = {
        "id": "budget-exceeded",
        "line": "main",
        "prompt": "随便聊聊",
        "vars": {},
        "expect": {"error_code": "budget_exceeded"},
        "min_rate": 1.0,
        "requires": "budget_stack",
    }
    cases = [CORPUS_CASES[0], budget_case]
    artifacts = tmp_path / "artifacts"
    rows = [row("rag-basic", success=True, components=[assertion("plan ok")])]
    completed = run_cli(
        write_results(tmp_path, rows), write_corpus(tmp_path, cases), artifacts
    )

    assert completed.returncode == 0
    report = read_report(artifacts)
    assert case_entry(report, "budget-exceeded")["not_run"] is True
    assert report["baseline"] == {
        "is_baseline": True,
        "not_run_beyond_prerequisites": [],
        "skipped_beyond_prerequisites": [],
    }
    summary = (artifacts / "summary.md").read_text(encoding="utf-8")
    assert "declared prerequisite: budget_stack" in summary
    assert "baseline: yes" in summary


def test_skipped_case_with_declared_prerequisite_keeps_baseline(tmp_path):
    budget_case = {
        "id": "budget-exceeded",
        "line": "main",
        "prompt": "随便聊聊",
        "vars": {},
        "expect": {"error_code": "budget_exceeded"},
        "min_rate": 1.0,
        "requires": "budget_stack",
    }
    cases = [CORPUS_CASES[0], budget_case]
    artifacts = tmp_path / "artifacts"
    rows = [
        row("rag-basic", success=True, components=[assertion("plan ok")]),
        skipped_row("budget-exceeded", "requires budget_stack"),
    ]
    completed = run_cli(
        write_results(tmp_path, rows), write_corpus(tmp_path, cases), artifacts
    )

    assert completed.returncode == 0
    report = read_report(artifacts)
    entry = case_entry(report, "budget-exceeded")
    assert entry["skipped"] == 1
    assert entry["rate"] is None
    assert report["baseline"]["is_baseline"] is True


def test_unexpectedly_skipped_case_is_not_a_baseline(tmp_path):
    artifacts = tmp_path / "artifacts"
    rows = [
        row("rag-basic", success=True, components=[assertion("plan ok")]),
        skipped_row("weather", "mode matrix has no entry"),
        skipped_row("weather", "mode matrix has no entry"),
    ]
    completed = run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts)

    assert completed.returncode == 0
    report = read_report(artifacts)
    assert case_entry(report, "weather")["not_run"] is False
    assert case_entry(report, "weather")["rate"] is None
    assert report["baseline"] == {
        "is_baseline": False,
        "not_run_beyond_prerequisites": [],
        "skipped_beyond_prerequisites": ["weather"],
    }
    assert "SKIPPED: `weather`" in (artifacts / "summary.md").read_text(encoding="utf-8")


def test_missing_results_file_exits_2(tmp_path):
    completed = run_cli(
        tmp_path / "absent.json", write_corpus(tmp_path), tmp_path / "artifacts"
    )
    assert completed.returncode == 2
    assert "cannot read promptfoo output" in completed.stderr
    assert not (tmp_path / "artifacts" / "report.json").exists()


def test_unparseable_results_file_exits_2(tmp_path):
    path = tmp_path / "results.json"
    path.write_text("{not json", encoding="utf-8")
    completed = run_cli(path, write_corpus(tmp_path), tmp_path / "artifacts")
    assert completed.returncode == 2
    assert "not valid JSON" in completed.stderr


def test_results_without_a_rows_array_exits_2(tmp_path):
    path = tmp_path / "results.json"
    path.write_text(json.dumps({"results": {}}), encoding="utf-8")
    completed = run_cli(path, write_corpus(tmp_path), tmp_path / "artifacts")
    assert completed.returncode == 2
    assert "results.results[]" in completed.stderr


def test_unknown_case_id_is_a_harness_error(tmp_path):
    rows = [
        row("ghost-case", success=True, components=[assertion("plan ok")]),
        row("rag-basic", success=True, components=[assertion("plan ok")]),
        row("weather", success=True, components=[assertion("plan ok")]),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts)

    assert completed.returncode == 1
    report = read_report(artifacts)
    assert any("unknown case id 'ghost-case'" in error for error in report["errors"])
    assert "## Harness errors" in (artifacts / "summary.md").read_text(encoding="utf-8")


def test_unknown_case_id_still_exits_non_zero_under_no_fail(tmp_path):
    rows = [row("ghost-case", success=True, components=[assertion("plan ok")])]
    completed = run_cli(
        write_results(tmp_path, rows),
        write_corpus(tmp_path),
        tmp_path / "artifacts",
        "--no-fail",
    )
    assert completed.returncode == 1


def test_reasons_are_scrubbed_of_credentials(tmp_path):
    leaked = (
        "plan mismatch: observed=[llm] auth=Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0."
        "c2VjcmV0c2lnbmF0dXJl dsn=postgresql://nyx:hunter2@db:5432/nyx"
    )
    rows = [row("rag-basic", success=False, components=[assertion(leaked, passed=False)])]
    artifacts = tmp_path / "artifacts"
    run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--no-fail")

    report_text = (artifacts / "report.json").read_text(encoding="utf-8")
    summary_text = (artifacts / "summary.md").read_text(encoding="utf-8")
    for text in (report_text, summary_text):
        assert "hunter2" not in text
        assert "c2VjcmV0c2lnbmF0dXJl" not in text
        assert "<redacted-dsn>" in text
        assert "Bearer <redacted>" in text


def test_mode_env_and_model_are_recorded(tmp_path):
    rows = [row("rag-basic", success=True, components=[assertion("plan ok")])]
    artifacts = tmp_path / "artifacts"
    run_cli(
        write_results(tmp_path, rows, metadata={"model": MODEL_ID}),
        write_corpus(tmp_path),
        artifacts,
        "--no-fail",
        env={"EVAL_WEB_SEARCH": "off"},
    )
    report = read_report(artifacts)
    assert report["mode"]["web_search"] == "off"
    assert report["model"] == MODEL_ID


def test_a_skip_cannot_launder_a_failed_assertion(tmp_path):
    rows = [
        row(
            "rag-basic",
            success=False,
            components=[
                assertion("SKIPPED: no plan expectation for web_search=off", passed=True),
                assertion(PLAN_MISMATCH, passed=False),
            ],
        ),
        row("rag-basic", success=True, components=[assertion("plan ok")]),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts)

    assert completed.returncode == 1
    report = read_report(artifacts)
    entry = case_entry(report, "rag-basic")
    assert entry["graded"] == 2
    assert entry["failed"] == 1
    assert entry["rate"] == 0.5
    assert entry["breach"] is True
    first = entry["samples"][0]
    assert first["skipped"] is False
    assert first["verdict"] == "fail"
    assert report["skipped"] == []
    summary = (artifacts / "summary.md").read_text(encoding="utf-8")
    assert "## Failing samples" in summary
    assert "plan mismatch" in summary


def test_nothing_graded_is_a_failure_not_a_pass(tmp_path):
    rows = [skipped_row("rag-basic", "requires budget_stack")]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(
        write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--no-fail"
    )

    assert completed.returncode == 1
    report = read_report(artifacts)
    assert any("no graded samples" in error for error in report["errors"])
    assert report["overall"]["rate"] is None
    summary = (artifacts / "summary.md").read_text(encoding="utf-8")
    assert "NO GRADED SAMPLES" in summary
    assert "within floor" not in summary


def test_empty_results_are_a_failure_not_a_pass(tmp_path):
    artifacts = tmp_path / "artifacts"
    completed = run_cli(write_results(tmp_path, []), write_corpus(tmp_path), artifacts)

    assert completed.returncode == 1
    report = read_report(artifacts)
    assert any("no graded samples" in error for error in report["errors"])
    assert all(entry["not_run"] for entry in report["cases"])


def test_overall_only_breach_exits_non_zero(tmp_path):
    rows = [
        row("weather", success=True, components=[assertion("plan ok")]),
        row("weather", success=False, components=[assertion(PLAN_MISMATCH, passed=False)]),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts)

    assert completed.returncode == 1
    report = read_report(artifacts)
    assert report["breaches"] == ["<overall>"]
    assert case_entry(report, "weather")["breach"] is False
    assert "BREACH" in (artifacts / "summary.md").read_text(encoding="utf-8")


def test_case_entry_records_the_declared_mode(tmp_path):
    rows = [row("weather", success=True, components=[assertion("plan ok")])]
    artifacts = tmp_path / "artifacts"
    run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--no-fail")
    report = read_report(artifacts)
    assert case_entry(report, "weather")["mode"] == {"web_search": "on"}
    assert case_entry(report, "rag-basic")["mode"] is None


def test_baseline_rate_is_recorded_for_floor_provenance(tmp_path):
    rows = [
        row("rag-basic", success=True, components=[assertion("plan ok")]),
        row("rag-basic", success=True, components=[assertion("plan ok")]),
        row("weather", success=True, components=[assertion("plan ok")]),
        row("weather", success=True, components=[assertion("plan ok")]),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(
        write_results(tmp_path, rows),
        write_corpus(tmp_path),
        artifacts,
        "--min-rate",
        "0.7",
        "--baseline-rate",
        "0.73",
    )

    assert completed.returncode == 0, completed.stderr
    report = read_report(artifacts)
    assert report["min_rate"] == 0.7
    assert report["overall"]["floor"] == 0.7
    assert report["baseline_rate"] == 0.73
    summary = (artifacts / "summary.md").read_text(encoding="utf-8")
    assert "floor provenance: applied floor 0.7 derived from baseline rate 0.73" in summary


def test_out_of_range_baseline_rate_exits_2(tmp_path):
    artifacts = tmp_path / "artifacts"
    completed = run_cli(
        write_results(tmp_path, []),
        write_corpus(tmp_path),
        artifacts,
        "--baseline-rate",
        "1.5",
    )
    assert completed.returncode == 2
    assert "--baseline-rate" in completed.stderr
    assert not (artifacts / "report.json").exists()


def test_answer_coverage_marks_plan_shape_only_cases_ungraded(tmp_path):
    cases = [
        {
            "id": "shape-only",
            "line": "main",
            "prompt": "整理我的喂",
            "vars": {},
            "expect": {"plan": ["llm"]},
            "min_rate": 0.0,
        },
        {
            "id": "answer-graded",
            "line": "main",
            "prompt": "整理我的喂",
            "vars": {},
            "expect": {"plan": ["llm"], "answer_contains": ["秋刀鱼"]},
            "min_rate": 0.0,
        },
        {
            "id": "forbidden-graded",
            "line": "main",
            "prompt": "整理我的喂",
            "vars": {},
            "expect": {"plan": ["llm"], "answer_forbidden": ["无法直接访问"]},
            "min_rate": 0.0,
        },
        {
            "id": "turn-answer-graded",
            "line": "main",
            "prompt": "整理我的喂",
            "vars": {},
            "expect": {"turns": [{"index": 1, "answer_contains": ["秋刀鱼"]}]},
            "min_rate": 0.0,
        },
        {
            "id": "tool-effect-claims-graded",
            "line": "main",
            "prompt": "删掉那些feed",
            "vars": {},
            "expect": {
                "plan": ["rag", "delete_feed"],
                "tool_effect": {
                    "tool": "delete_feed",
                    "effect": "deleted",
                    "success_claims": ["已删除"],
                },
            },
            "min_rate": 0.0,
        },
        {
            "id": "tool-effect-no-claims",
            "line": "main",
            "prompt": "删掉那些feed",
            "vars": {},
            "expect": {
                "plan": ["rag", "delete_feed"],
                "tool_effect": {"tool": "delete_feed", "effect": "deleted"},
            },
            "min_rate": 0.0,
        },
        {
            "id": "error-only",
            "line": "main",
            "prompt": "随便聊聊",
            "vars": {},
            "expect": {"error_code": "budget_exceeded"},
            "min_rate": 0.0,
        },
    ]
    rows = [
        row(case["id"], success=True, components=[assertion("plan ok")]) for case in cases
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(
        write_results(tmp_path, rows), write_corpus(tmp_path, cases), artifacts
    )

    assert completed.returncode == 0, completed.stderr
    report = read_report(artifacts)
    assert case_entry(report, "shape-only")["rate"] == 1.0
    assert case_entry(report, "shape-only")["answer_coverage"] == "ungraded"
    assert case_entry(report, "answer-graded")["answer_coverage"] == "graded"
    assert case_entry(report, "forbidden-graded")["answer_coverage"] == "graded"
    assert case_entry(report, "turn-answer-graded")["answer_coverage"] == "graded"
    assert case_entry(report, "tool-effect-claims-graded")["answer_coverage"] == "graded"
    assert case_entry(report, "tool-effect-no-claims")["answer_coverage"] == "ungraded"
    assert case_entry(report, "error-only")["answer_coverage"] == "ungraded"
    summary = (artifacts / "summary.md").read_text(encoding="utf-8")
    assert "| shape-only | main | 1.00 |" in summary
    assert "| shape-only | main | 1.00 | 1 | 0 | 0 | 0.0 | PASS | ungraded |" in summary
    assert (
        "| answer-graded | main | 1.00 | 1 | 0 | 0 | 0.0 | PASS | graded |" in summary
    )
    assert "not evidence about the answer text" in summary


def test_scrub_covers_short_credentials():
    text = scrub("auth=Bearer a1b2 jwt=eyJh.eyJi.c2ln")
    assert "Bearer <redacted>" in text
    assert "<redacted-jwt>" in text
    assert "a1b2" not in text
    assert "eyJh" not in text


def test_scrub_helper_redacts_jwt_and_dsn():
    fake_jwt = ".".join(("eyJhbGciOiJIUzI1NiJ9", "eyJzdWIiOiIxIn0", "AbCdEfGhIjKl"))
    text = scrub(f"value={fake_jwt} DSN redis://x:y@h:6379")
    assert "eyJhbGciOiJIUzI1NiJ9" not in text
    assert "<redacted-jwt>" in text
    assert "<redacted-dsn>" in text


def test_observed_tokens_helper_fallbacks():
    assert observed_tokens_of([assertion(PLAN_MISMATCH, passed=False)]) == ["llm"]
    assert observed_tokens_of([assertion("plan ok", observed_tokens=[])]) == []
    assert observed_tokens_of([assertion("no plan frame")]) is None
    assert observed_tokens_of([]) is None


def test_failing_sample_transcript_is_written_for_triage(tmp_path):
    output = json.dumps({"status": "completed", "turns": [{"answer": "嗯"}]}, ensure_ascii=False)
    rows = [
        row("rag-basic", success=False, components=[assertion(PLAN_MISMATCH, passed=False)], output=output)
    ]
    artifacts = tmp_path / "artifacts"
    run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--no-fail")

    artifact = artifacts / "samples" / "rag-basic__1.json"
    assert artifact.read_text(encoding="utf-8") == output
    sample = case_entry(read_report(artifacts), "rag-basic")["samples"][0]
    assert sample["artifact"] == "samples/rag-basic__1.json"


def test_provider_error_is_recorded_on_the_sample(tmp_path):
    rows = [
        {
            "vars": {"case_id": "rag-basic"},
            "success": False,
            "error": "agent unreachable at http://127.0.0.1:8099/agent/run",
            "componentResults": [],
        },
        row("weather", success=True, components=[assertion("plan ok")]),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(
        write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--no-fail"
    )

    assert completed.returncode == 0
    sample = case_entry(read_report(artifacts), "rag-basic")["samples"][0]
    assert sample["verdict"] == "fail"
    assert any("agent unreachable" in reason for reason in sample["reasons"])
    summary = (artifacts / "summary.md").read_text(encoding="utf-8")
    assert "agent unreachable" in summary


def test_unattributed_sample_is_reported(tmp_path):
    rows = [{"vars": {}, "success": True, "componentResults": [], "response": {"output": "{}"}}]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts)
    assert completed.returncode == 1
    report = read_report(artifacts)
    assert any("no vars.case_id" in error for error in report["errors"])


@pytest.mark.parametrize("case_id", ["rag-basic", "weather"])
def test_every_corpus_case_appears_in_the_report(tmp_path, case_id):
    rows = [row("rag-basic", success=True, components=[assertion("plan ok")])]
    artifacts = tmp_path / "artifacts"
    run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--no-fail")
    assert case_entry(read_report(artifacts), case_id)["id"] == case_id


def promptfoo_124_row(
    case_id: str, *, success: bool, components=(), error=None, output: str = '{"status": "completed"}'
) -> dict:
    """A results row in the shape promptfoo 0.124.1 actually writes.

    Assertion detail lives under `gradingResult.componentResults`, the failure
    text rides `error`, and there is no row-level `componentResults` at all.
    """
    return {
        "vars": {"case_id": case_id, "prompt": "…"},
        "success": success,
        "error": error,
        "gradingResult": {"componentResults": list(components), "pass": success},
        "response": {"output": output},
    }


def test_promptfoo_124_nested_components_are_read(tmp_path):
    rows = [
        promptfoo_124_row(
            "rag-basic",
            success=False,
            components=[
                assertion(PLAN_MISMATCH, passed=False, observed_tokens=["rag", "create_feed"])
            ],
            error=PLAN_MISMATCH,
        ),
    ]
    artifacts = tmp_path / "artifacts"
    run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts, "--no-fail")
    sample = case_entry(read_report(artifacts), "rag-basic")["samples"][0]
    assert sample["observed_tokens"] == ["rag", "create_feed"]
    assert sample["verdict"] == "fail"


def test_promptfoo_124_nested_skip_is_not_a_pass(tmp_path):
    rows = [
        promptfoo_124_row(
            "rag-basic",
            success=True,
            components=[assertion("SKIPPED: preference extraction timeout", passed=True)],
        ),
        promptfoo_124_row("weather", success=True, components=[assertion("plan ok")]),
    ]
    artifacts = tmp_path / "artifacts"
    completed = run_cli(write_results(tmp_path, rows), write_corpus(tmp_path), artifacts)
    assert completed.returncode == 0, completed.stderr
    report = read_report(artifacts)
    entry = case_entry(report, "rag-basic")
    assert entry["graded"] == 0
    assert entry["skipped"] == 1
    assert entry["samples"][0]["verdict"] == "skipped"
    assert report["overall"]["rate"] == 1.0
