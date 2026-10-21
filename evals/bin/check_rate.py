#!/usr/bin/env python
"""Post-process a promptfoo JSON run into per-case rates, floors, and artifacts.

    check_rate.py <promptfoo-results.json> --corpus-glob 'evals/cases/*.yaml' \
        --artifacts-dir evals/artifacts/<stamp> [--min-rate 0.85] [--no-fail]

Reads promptfoo's `results.results[]` rows (`vars`, `success`,
`componentResults[]`), computes a pass rate per corpus case over its non-skipped
samples, compares each rate to the case's corpus `min_rate` floor and the
overall rate to `--min-rate`, and writes `<artifacts-dir>/report.json` +
`summary.md` plus one transcript artifact per sample.

The report records the applied overall floor together with the baseline rate it
was derived from (`--baseline-rate`), so a run can never be cited against a
floor the runner did not enforce. A run that leaves corpus cases NOT RUN — or
entirely SKIPPED — beyond their declared prerequisites is flagged as not a
baseline: its overall rate is still reported, but it is not evidence the suite
passed. Each case also reports `answer_coverage`: a case that declares no
answer-content assertion is marked `ungraded`, so a plan-shape-only green is
never read as answer-text evidence.

Exit codes: 0 within floors (or with `--no-fail`), 1 when a floor is breached,
when a sample cannot be attributed to the corpus, or when nothing was graded at
all, 2 when the promptfoo file or the corpus cannot be read. Samples whose
grader reasons start with `SKIPPED:` are excluded from every denominator and
listed in the report's SKIPPED section — but only when no component of that
sample failed, so a skip can never hide a failure. Nothing written here
contains credentials — reasons, model ids, and mode values are scrubbed of
bearer tokens, JWTs, and connection strings before they reach a report.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable, Mapping

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from lib.corpus import REPO_ROOT, Case, CorpusError, load_corpus  # noqa: E402

DEFAULT_CORPUS_GLOB = "evals/cases/*.yaml"
DEFAULT_MIN_RATE = 0.85
SKIPPED_PREFIX = "SKIPPED:"
RATE_EPSILON = 1e-9
ANSWER_ASSERTION_KEYS = ("answer_contains", "answer_forbidden")

_OBSERVED_RE = re.compile(r"observed=\[([^\]]*)\]")
_UNSAFE_NAME_RE = re.compile(r"[^A-Za-z0-9._-]+")

_SCRUB_PATTERNS = (
    (re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._~+/=\-]{4,}"), "Bearer <redacted>"),
    (
        re.compile(r"eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+"),
        "<redacted-jwt>",
    ),
    (
        re.compile(r"(?i)\b(?:postgres(?:ql)?|redis|rediss|amqp)://[^\s\"'\\]+"),
        "<redacted-dsn>",
    ),
)


class InputError(Exception):
    """The promptfoo output or corpus cannot be read."""


def scrub(text: Any) -> str:
    """Redact credentials from anything bound for a report."""
    value = text if isinstance(text, str) else str(text)
    for pattern, replacement in _SCRUB_PATTERNS:
        value = pattern.sub(replacement, value)
    return value


def scrub_optional(text: str | None) -> str | None:
    return None if text is None else scrub(text)


def normalized_web_search_mode() -> str:
    """`EVAL_WEB_SEARCH`, normalized the way the plan-shape grader normalizes it."""
    value = scrub(os.environ.get("EVAL_WEB_SEARCH", "")).strip().lower()
    return value or "on"


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="check_rate.py",
        description="Compute golden-set pass rates against corpus floors and write run artifacts.",
    )
    parser.add_argument("promptfoo_json", help="path to promptfoo's JSON output")
    parser.add_argument(
        "--corpus-glob",
        default=DEFAULT_CORPUS_GLOB,
        help=f"corpus glob for per-case floors (default: {DEFAULT_CORPUS_GLOB!r})",
    )
    parser.add_argument(
        "--artifacts-dir",
        required=True,
        help="directory to write report.json, summary.md, and per-sample transcripts into",
    )
    parser.add_argument(
        "--min-rate",
        type=float,
        default=DEFAULT_MIN_RATE,
        help=f"overall pass-rate floor (default: {DEFAULT_MIN_RATE})",
    )
    parser.add_argument(
        "--baseline-rate",
        type=float,
        default=None,
        help="baseline overall pass rate the --min-rate floor was derived from (recorded for provenance)",
    )
    parser.add_argument(
        "--no-fail",
        action="store_true",
        help="report floor breaches without failing (trend runs); input errors still exit non-zero",
    )
    args = parser.parse_args(argv)
    if args.baseline_rate is not None and not 0.0 <= args.baseline_rate <= 1.0:
        parser.error(f"--baseline-rate must be in [0, 1], got {args.baseline_rate}")
    return args


def read_promptfoo(path: str) -> dict:
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        raise InputError(f"cannot read promptfoo output {path!r}: {exc}") from exc
    try:
        document = json.loads(text)
    except ValueError as exc:
        raise InputError(f"promptfoo output {path!r} is not valid JSON: {exc}") from exc
    if not isinstance(document, dict):
        raise InputError(
            f"promptfoo output {path!r} must be a JSON object, got {type(document).__name__}"
        )
    return document


def sample_rows(document: Mapping[str, Any]) -> list[dict]:
    """Locate `results.results[]`, tolerating a bare list at the top level."""
    results = document.get("results")
    if isinstance(results, list):
        rows = results
    elif isinstance(results, dict) and isinstance(results.get("results"), list):
        rows = results["results"]
    else:
        raise InputError("promptfoo output has no results.results[] array")
    return [row for row in rows if isinstance(row, dict)]


def case_id_of(row: Mapping[str, Any]) -> str | None:
    vars_ = row.get("vars")
    if not isinstance(vars_, dict):
        return None
    for key in ("case_id", "caseId", "id"):
        value = vars_.get(key)
        if isinstance(value, str) and value:
            return value
    nested = vars_.get("case")
    if isinstance(nested, dict):
        value = nested.get("id")
        if isinstance(value, str) and value:
            return value
    return None


def components_of(row: Mapping[str, Any]) -> list[dict]:
    grading = row.get("gradingResult")
    if isinstance(grading, dict):
        value = grading.get("componentResults")
        if isinstance(value, list):
            return [item for item in value if isinstance(item, dict)]
    for key in ("componentResults", "assertionResults"):
        value = row.get(key)
        if isinstance(value, list):
            return [item for item in value if isinstance(item, dict)]
    return []


def output_text_of(row: Mapping[str, Any]) -> str | None:
    response = row.get("response")
    if isinstance(response, dict):
        value = response.get("output")
        if isinstance(value, str):
            return value
        if value is not None:
            return json.dumps(value, ensure_ascii=False)
    value = row.get("output")
    if isinstance(value, str):
        return value
    if value is not None:
        return json.dumps(value, ensure_ascii=False)
    return None


def observed_tokens_of(components: Iterable[Mapping[str, Any]]) -> list[str] | None:
    """Observed plan tokens, from grader metadata when promptfoo kept it, else the reason."""
    for component in components:
        metadata = component.get("metadata")
        if isinstance(metadata, dict):
            tokens = metadata.get("observed_tokens")
            if isinstance(tokens, list) and all(isinstance(token, str) for token in tokens):
                return list(tokens)
    for component in components:
        reason = component.get("reason")
        if not isinstance(reason, str):
            continue
        match = _OBSERVED_RE.search(reason)
        if match:
            inner = match.group(1).strip()
            return [token.strip() for token in inner.split(",") if token.strip()]
    return None


def sample_verdicts(row: Mapping[str, Any], index: int) -> dict:
    """Classify one promptfoo sample.

    A sample is SKIPPED only when the transcript was ungradeable AND nothing
    failed: one grader reporting SKIPPED (e.g. the mode matrix has no entry for
    the active mode) must not launder another grader's failure out of the rate.
    """
    components = components_of(row)
    reasons = [scrub(reason) for reason in (c.get("reason") for c in components) if reason]
    row_error = row.get("error")
    if isinstance(row_error, str) and row_error:
        reasons.append(scrub(row_error))
    skip_reasons = [
        reason[len(SKIPPED_PREFIX) :].strip()
        for reason in reasons
        if reason.startswith(SKIPPED_PREFIX)
    ]
    failed_components = [component for component in components if component.get("pass") is False]
    skipped = bool(skip_reasons) and not failed_components
    failed = (not skipped) and (bool(failed_components) or not row.get("success"))
    return {
        "index": index,
        "case_id": case_id_of(row),
        "verdict": "skipped" if skipped else ("fail" if failed else "pass"),
        "success": not skipped and not failed,
        "skipped": skipped,
        "skip_reason": skip_reasons[0] if skip_reasons else None,
        "observed_tokens": observed_tokens_of(components),
        "reasons": reasons,
        "artifact": None,
    }


def write_sample_artifact(
    artifacts_dir: Path, sample: dict, row: Mapping[str, Any]
) -> None:
    text = output_text_of(row)
    if text is None:
        return
    case_slug = _UNSAFE_NAME_RE.sub("_", sample.get("case_id") or "unattributed")
    relative = Path("samples") / f"{case_slug}__{sample['index']}.json"
    target = artifacts_dir / relative
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(scrub(text), encoding="utf-8")
    sample["artifact"] = relative.as_posix()


def git_revision() -> str | None:
    try:
        completed = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=str(REPO_ROOT),
            capture_output=True,
            text=True,
            timeout=15,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    revision = completed.stdout.strip()
    return revision if completed.returncode == 0 and revision else None


def model_id(document: Mapping[str, Any], samples: Iterable[Mapping[str, Any]]) -> str | None:
    for container in (document.get("metadata"), document.get("meta")):
        if isinstance(container, dict):
            for key in ("model", "model_id", "modelId"):
                value = container.get(key)
                if isinstance(value, str) and value:
                    return value
    for sample in samples:
        metadata = sample.get("metadata")
        if isinstance(metadata, dict):
            value = metadata.get("model")
            if isinstance(value, str) and value:
                return value
    return os.environ.get("EVAL_MODEL") or os.environ.get("EVAL_MODEL_ID") or None


def rate_of(passed: int, graded: int) -> float | None:
    return round(passed / graded, 4) if graded else None


def _asserts_answer_text(expect: Mapping[str, Any]) -> bool:
    if any(key in expect for key in ANSWER_ASSERTION_KEYS):
        return True
    tool_effect = expect.get("tool_effect")
    if isinstance(tool_effect, Mapping):
        claims = tool_effect.get("success_claims")
        if isinstance(claims, list) and claims:
            return True
    return False


def answer_coverage_of(case: Case) -> str:
    """`graded` only when the case asserts on answer text, else `ungraded`."""
    expect = case.expect if isinstance(case.expect, Mapping) else {}
    if _asserts_answer_text(expect):
        return "graded"
    turns = expect.get("turns")
    if isinstance(turns, list):
        for turn in turns:
            if isinstance(turn, dict) and _asserts_answer_text(turn):
                return "graded"
    return "ungraded"


def build_case_entry(case: Case, samples: list[dict]) -> dict:
    graded = [sample for sample in samples if not sample["skipped"]]
    passed = [sample for sample in graded if sample["success"]]
    failed = [sample for sample in graded if not sample["success"]]
    skipped = [sample for sample in samples if sample["skipped"]]
    rate = rate_of(len(passed), len(graded))
    return {
        "id": case.id,
        "line": case.line,
        "source": case.source,
        "mode": case.vars.get("mode") if isinstance(case.vars, dict) else None,
        "floor": case.min_rate,
        "requires": case.requires,
        "rate": rate,
        "graded": len(graded),
        "passed": len(passed),
        "failed": len(failed),
        "skipped": len(skipped),
        "not_run": not samples,
        "answer_coverage": answer_coverage_of(case),
        "breach": rate is not None and rate + RATE_EPSILON < case.min_rate,
        "samples": samples,
        "skipped_samples": [
            {"index": sample["index"], "reason": sample["skip_reason"]} for sample in skipped
        ],
    }


def baseline_status(entries: Iterable[Mapping[str, Any]]) -> dict:
    """A baseline requires every case to execute or be covered by a declared prerequisite."""
    entries = list(entries)
    not_run = [
        entry["id"] for entry in entries if entry["not_run"] and entry["requires"] is None
    ]
    skipped = [
        entry["id"]
        for entry in entries
        if not entry["not_run"] and entry["rate"] is None and entry["requires"] is None
    ]
    return {
        "is_baseline": not not_run and not skipped,
        "not_run_beyond_prerequisites": not_run,
        "skipped_beyond_prerequisites": skipped,
    }


def build_report(
    document: Mapping[str, Any],
    rows: list[dict],
    cases: list[Case],
    args: argparse.Namespace,
) -> dict:
    samples: list[dict] = []
    errors: list[str] = []
    cases_by_id = {case.id: case for case in cases}

    for index, row in enumerate(rows, start=1):
        sample = sample_verdicts(row, index)
        if sample["case_id"] is None:
            errors.append(
                f"sample {index}: no vars.case_id — cannot attribute the sample to a corpus case"
            )
        elif sample["case_id"] not in cases_by_id:
            errors.append(f"sample {index}: unknown case id {sample['case_id']!r} (not in the corpus)")
        samples.append(sample)

    entries = [
        build_case_entry(case, [sample for sample in samples if sample["case_id"] == case.id])
        for case in cases
    ]

    graded_total = sum(entry["graded"] for entry in entries)
    passed_total = sum(entry["passed"] for entry in entries)
    skipped_total = sum(entry["skipped"] for entry in entries)
    overall_rate = rate_of(passed_total, graded_total)
    overall_breach = overall_rate is not None and overall_rate + RATE_EPSILON < args.min_rate

    if graded_total == 0:
        errors.append(
            "no graded samples: the corpus declares cases but every sample was skipped or the "
            "promptfoo output carried no rows — there is nothing to compare to a floor"
        )

    breaches = [entry["id"] for entry in entries if entry["breach"]]
    if overall_breach:
        breaches.append("<overall>")

    return {
        "generated_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "git_revision": git_revision(),
        "model": scrub_optional(model_id(document, rows)),
        "mode": {
            "web_search": normalized_web_search_mode(),
            "budget_stack": scrub_optional(os.environ.get("EVAL_BUDGET_STACK")),
        },
        "min_rate": args.min_rate,
        "baseline_rate": args.baseline_rate,
        "promptfoo_results": str(Path(args.promptfoo_json)),
        "corpus_glob": args.corpus_glob,
        "artifacts_dir": str(Path(args.artifacts_dir)),
        "overall": {
            "rate": overall_rate,
            "floor": args.min_rate,
            "graded": graded_total,
            "passed": passed_total,
            "skipped": skipped_total,
            "breach": overall_breach,
        },
        "baseline": baseline_status(entries),
        "cases": entries,
        "skipped": [
            {"case_id": entry["id"], "index": item["index"], "reason": item["reason"]}
            for entry in entries
            for item in entry["skipped_samples"]
        ],
        "breaches": breaches,
        "errors": [scrub(error) for error in errors],
        "no_fail": args.no_fail,
    }


def render_rate(rate: float | None) -> str:
    return "—" if rate is None else f"{rate:.2f}"


def render_overall_verdict(report: Mapping[str, Any]) -> str:
    overall = report["overall"]
    if overall["breach"]:
        return "BREACH"
    if overall["rate"] is None:
        return "NO GRADED SAMPLES"
    if not report["baseline"]["is_baseline"]:
        return "PARTIAL RUN (not a baseline)"
    return "within floor"


def render_floor_provenance(report: Mapping[str, Any]) -> str:
    floor = report["overall"]["floor"]
    baseline_rate = report.get("baseline_rate")
    if baseline_rate is None:
        return f"- floor provenance: applied floor {floor}; baseline rate not recorded"
    return f"- floor provenance: applied floor {floor} derived from baseline rate {baseline_rate:.2f}"


def render_baseline(report: Mapping[str, Any]) -> str:
    baseline = report["baseline"]
    if baseline["is_baseline"]:
        return "- baseline: yes — every corpus case executed or is excluded by a declared prerequisite"
    details = []
    if baseline["not_run_beyond_prerequisites"]:
        cases = ", ".join(f"`{case_id}`" for case_id in baseline["not_run_beyond_prerequisites"])
        details.append(f"NOT RUN: {cases}")
    if baseline["skipped_beyond_prerequisites"]:
        cases = ", ".join(f"`{case_id}`" for case_id in baseline["skipped_beyond_prerequisites"])
        details.append(f"SKIPPED: {cases}")
    return (
        f"- baseline: NO — partial run ({'; '.join(details)}); "
        "the overall rate is not evidence the suite passed"
    )


def render_summary(report: Mapping[str, Any]) -> str:
    overall = report["overall"]
    mode = report["mode"]
    lines = [
        "# Golden-set run",
        "",
        f"- generated: {report['generated_at']}",
        f"- model: {report['model'] or 'unknown'}",
        f"- git revision: {report['git_revision'] or 'unknown'}",
        f"- mode: web_search={mode['web_search']} budget_stack={mode['budget_stack'] or 'off'}",
        f"- promptfoo results: {report['promptfoo_results']}",
        f"- corpus: {report['corpus_glob']}",
        (
            f"- overall: {render_rate(overall['rate'])} "
            f"({overall['passed']}/{overall['graded']} graded, {overall['skipped']} skipped) "
            f"vs floor {overall['floor']} — {render_overall_verdict(report)}"
        ),
        render_floor_provenance(report),
        render_baseline(report),
        "",
        "## Cases",
        "",
        "| case | line | rate | pass | fail | skip | floor | verdict | answer |",
        "| --- | --- | --- | --- | --- | --- | --- | --- | --- |",
    ]
    for entry in report["cases"]:
        if entry["not_run"]:
            verdict = "NOT RUN"
        elif entry["rate"] is None:
            verdict = "SKIPPED"
        elif entry["breach"]:
            verdict = "BREACH"
        else:
            verdict = "PASS"
        lines.append(
            f"| {entry['id']} | {entry['line']} | {render_rate(entry['rate'])} | "
            f"{entry['passed']} | {entry['failed']} | {entry['skipped']} | "
            f"{entry['floor']} | {verdict} | {entry['answer_coverage']} |"
        )
    lines += [
        "",
        (
            "Answer coverage `ungraded` marks a case with no answer-content assertion; "
            "its rate is not evidence about the answer text."
        ),
    ]

    all_skipped = [
        entry["id"] for entry in report["cases"] if entry["rate"] is None and not entry["not_run"]
    ]
    lines += ["", "## SKIPPED", ""]
    if report["skipped"] or all_skipped:
        lines.append("Skipped samples counted toward neither pass nor failure rates.")
        lines.append("")
        for item in report["skipped"]:
            lines.append(f"- `{item['case_id']}` sample {item['index']}: {item['reason']}")
        for case_id in all_skipped:
            lines.append(f"- `{case_id}` (case): every sample skipped — no rate recorded")
    else:
        lines.append("None.")

    failures = [entry for entry in report["cases"] if entry["failed"]]
    if failures:
        lines += ["", "## Failing samples", ""]
        for entry in failures:
            for sample in entry["samples"]:
                if sample["skipped"] or sample["success"]:
                    continue
                tokens = (
                    f" observed={sample['observed_tokens']}"
                    if sample["observed_tokens"] is not None
                    else ""
                )
                artifact = f" artifact={sample['artifact']}" if sample["artifact"] else ""
                detail = " | ".join(sample["reasons"]) or "no assertion reason recorded"
                lines.append(
                    f"- `{entry['id']}` sample {sample['index']}:{tokens}{artifact} — {detail}"
                )

    not_run = [entry for entry in report["cases"] if entry["not_run"]]
    if not_run:
        lines += ["", "## Not run", ""]
        lines.append("Cases in the corpus with no samples in this promptfoo output:")
        lines.append("")
        for entry in not_run:
            if entry["requires"]:
                lines.append(f"- `{entry['id']}` — declared prerequisite: {entry['requires']}")
            else:
                lines.append(
                    f"- `{entry['id']}` — beyond declared prerequisites; the run is not a baseline"
                )

    if report["errors"]:
        lines += ["", "## Harness errors", ""]
        for error in report["errors"]:
            lines.append(f"- {error}")

    lines.append("")
    return "\n".join(lines)


def write_artifacts(artifacts_dir: Path, report: dict) -> None:
    artifacts_dir.mkdir(parents=True, exist_ok=True)
    (artifacts_dir / "report.json").write_text(
        json.dumps(report, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )
    (artifacts_dir / "summary.md").write_text(render_summary(report), encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        document = read_promptfoo(args.promptfoo_json)
        rows = sample_rows(document)
        cases = load_corpus([args.corpus_glob])
    except (InputError, CorpusError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    try:
        report = build_report(document, rows, cases, args)
    except Exception as exc:  # noqa: BLE001 — CLI boundary: report, don't traceback
        print(f"error: unexpected post-processing failure: {exc!r}", file=sys.stderr)
        return 2

    artifacts_dir = Path(args.artifacts_dir)
    try:
        artifacts_dir.mkdir(parents=True, exist_ok=True)
        write_samples(artifacts_dir, report, rows)
        report["exit_code"] = decide_exit_code(report, args)
        write_artifacts(artifacts_dir, report)
    except OSError as exc:
        print(f"error: cannot write artifacts into {artifacts_dir}: {exc}", file=sys.stderr)
        return 2

    overall = report["overall"]
    print(
        f"overall {render_rate(overall['rate'])} "
        f"({overall['passed']}/{overall['graded']} graded, {overall['skipped']} skipped) "
        f"vs floor {overall['floor']}"
    )
    if not report["baseline"]["is_baseline"]:
        print(
            "warning: partial run — cases NOT RUN or SKIPPED beyond declared prerequisites; "
            "the overall rate is not a suite baseline",
            file=sys.stderr,
        )
    for entry in report["cases"]:
        if entry["breach"]:
            print(
                f"breach: {entry['id']} rate {render_rate(entry['rate'])} "
                f"< floor {entry['floor']}"
            )
    for error in report["errors"]:
        print(f"warning: {error}", file=sys.stderr)
    print(f"artifacts: {artifacts_dir}/report.json, {artifacts_dir}/summary.md")
    return report["exit_code"]


def write_samples(artifacts_dir: Path, report: dict, rows: list[dict]) -> None:
    """Persist each sample's provider transcript for triage."""
    by_index = {
        sample["index"]: sample
        for sample in (s for entry in report["cases"] for s in entry["samples"])
    }
    for index, row in enumerate(rows, start=1):
        sample = by_index.get(index)
        if sample is None:
            continue
        write_sample_artifact(artifacts_dir, sample, row)


def decide_exit_code(report: Mapping[str, Any], args: argparse.Namespace) -> int:
    if report["errors"]:
        return 1
    if args.no_fail:
        return 0
    return 1 if report["breaches"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
