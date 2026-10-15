#!/usr/bin/env python3
# Schema-migration safety linter for backend/migrations/*.up.sql.
#
# Catches single-step destructive operations that should be split into
# expand → backfill → contract:
#   * DROP COLUMN                          — drop in one step instead of two
#   * DROP TABLE                           — drop in one step instead of back-up/rename
#   * ALTER COLUMN ... SET NOT NULL        — set NOT NULL before backfill is done
#   * ALTER COLUMN ... TYPE / SET DATA TYPE — type change in one step instead of add-new/backfill/drop-old
#
# Two bypasses:
#   1. The migration is listed in scripts/migrations-baseline.txt (pre-shipped
#      migrations grandfathered when this linter landed).
#   2. The SQL file carries an explicit `-- safe-migration: <reason>` annotation
#      anywhere in the file. The annotation forces the author to articulate why
#      a single-step op is acceptable (e.g. drop a temp table, no-op USING cast,
#      fresh-DB-only path).
#
# Comment stripping:
#   * `--` comments strip to end of line.
#   * `/* ... */` block comments are stripped before pattern matching.
# Patterns are matched against the cleaned text so a line like
# `-- TODO: do not DROP COLUMN` is not a false positive.
#
# Exit: 0 = clean (or only baseline files inspected), 1 = violations found.
#
# Usage:
#   python3 scripts/check-migrations.py
#
# Wired into:
#   * backend/Makefile `migration-check` target
#   * .github/workflows/ci.yml `backend-test` job

from __future__ import annotations

import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
MIG_DIR = REPO_ROOT / "backend" / "migrations"
BASELINE_PATH = REPO_ROOT / "scripts" / "migrations-baseline.txt"

# Migration filename: NNNNNN_description.up.sql. Capture the numeric prefix.
MIG_FILENAME_RE = re.compile(r"^(\d{6})_.*\.up\.sql$")

# Pattern catalogue. Each entry: (compiled regex, human-readable label).
# Order is the order they're reported in; keep most-severe first.
# `(?i)` for case-insensitive matching (SQL keywords are conventionally
# uppercased but Postgres accepts mixed case).
PATTERNS: list[tuple[re.Pattern[str], str]] = [
    (
        re.compile(r"\bDROP\s+COLUMN\b", re.IGNORECASE),
        "DROP COLUMN (split: deprecate reads, then drop in a later migration)",
    ),
    (
        re.compile(r"\bDROP\s+TABLE\b", re.IGNORECASE),
        "DROP TABLE (rename to a backup first, drop in a later migration)",
    ),
    (
        re.compile(r"\bALTER\s+COLUMN\s+\w+\s+SET\s+NOT\s+NULL\b", re.IGNORECASE),
        "ALTER COLUMN ... SET NOT NULL (add nullable, backfill, then SET NOT NULL)",
    ),
    (
        re.compile(r"\bALTER\s+COLUMN\s+\w+\s+(?:SET\s+DATA\s+)?TYPE\b", re.IGNORECASE),
        "ALTER COLUMN ... TYPE (add new column, backfill, drop old in a later migration)",
    ),
]

# Annotation: `-- safe-migration: <reason>`. Reason may be empty but the
# colon and the prefix are required so a stray `-- safe-migration` comment
# doesn't accidentally bypass the gate.
SAFE_ANNOTATION_RE = re.compile(r"^\s*--\s*safe-migration\s*:\s*\S", re.IGNORECASE | re.MULTILINE)


def migration_id(path: Path) -> str:
    """Extract the NNNNNN prefix from `NNNNNN_description.up.sql`."""
    m = MIG_FILENAME_RE.match(path.name)
    if not m:
        raise ValueError(f"unexpected migration filename: {path}")
    return m.group(1)


def load_baseline() -> set[str]:
    """Read the baseline migration IDs. Empty/missing baseline = no exemptions."""
    if not BASELINE_PATH.is_file():
        return set()
    ids: set[str] = set()
    for raw in BASELINE_PATH.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if not re.fullmatch(r"\d{6}", line):
            print(
                f"warning: baseline line `{raw!r}` is not a 6-digit migration ID; ignored",
                file=sys.stderr,
            )
            continue
        ids.add(line)
    return ids


def strip_comments(sql: str) -> str:
    """Remove `--` line comments and `/* ... */` block comments.

    Block comments are stripped first because `--` inside a block comment
    is just text and would otherwise create spurious line-comment starts.
    """
    sql = re.sub(r"/\*.*?\*/", " ", sql, flags=re.DOTALL)
    out_lines = []
    for line in sql.splitlines():
        idx = line.find("--")
        out_lines.append(line if idx < 0 else line[:idx])
    return "\n".join(out_lines)


def scan_file(path: Path) -> list[tuple[int, str, str]]:
    """Return [(line_no, pattern_label, matched_text), ...] for violations.

    Returns an empty list if the file carries a `-- safe-migration:` annotation.
    """
    text = path.read_text(encoding="utf-8")
    if SAFE_ANNOTATION_RE.search(text):
        return []
    cleaned = strip_comments(text)
    findings: list[tuple[int, str, str]] = []
    # Track line offsets so we can report the original line number (1-based).
    line_starts = [0]
    for i, ch in enumerate(cleaned):
        if ch == "\n":
            line_starts.append(i + 1)
    for pattern, label in PATTERNS:
        for m in pattern.finditer(cleaned):
            # Map character offset to line number.
            offset = m.start()
            line_no = 1 + sum(1 for start in line_starts if start <= offset)
            findings.append((line_no, label, m.group(0)))
    return findings


def main() -> int:
    baseline = load_baseline()
    migration_files = sorted(MIG_DIR.glob("*.up.sql"))
    if not migration_files:
        print(f"error: no migrations found under {MIG_DIR}", file=sys.stderr)
        return 1

    total_scanned = 0
    total_violations = 0
    for path in migration_files:
        mig_id = migration_id(path)
        if mig_id in baseline:
            continue
        total_scanned += 1
        findings = scan_file(path)
        if not findings:
            continue
        total_violations += len(findings)
        print(f"\n{path.relative_to(REPO_ROOT)}:")
        for line_no, label, matched in findings:
            print(f"  line {line_no}: {matched!r}  —  {label}")

    print(
        f"\n[migration-check] scanned {total_scanned} migration(s)"
        f" outside baseline; {len(baseline)} grandfathered; {total_violations} violation(s).",
    )
    if total_violations:
        print(
            "\nTo allow a single-step destructive op, add `-- safe-migration: <reason>` "
            "to the migration file (the reason is required so reviewers see why).",
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
