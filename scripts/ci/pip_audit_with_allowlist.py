import argparse
import json
import re
import subprocess
import sys
import tomllib
from datetime import date, timedelta
from pathlib import Path

MAX_AGE_DAYS = 90
FUTURE_TOLERANCE_DAYS = 1
AUDIT_TIMEOUT_SECONDS = 600
DEFAULT_ALLOWLIST = Path("backend/agent_py/.pip-audit-allowlist.toml")
REQUIRED_KEYS = ("id", "date", "rationale")


def repo_root() -> Path:
    out = subprocess.run(
        ["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True
    )
    if out.returncode != 0:
        raise ValueError("not inside a git worktree; pass --root")
    return Path(out.stdout.strip())


def normalize_dist_name(name: str) -> str:
    return re.sub(r"[-_.]+", "-", name).strip().lower()


def load_allowlist(path: Path) -> dict[str, dict]:
    with path.open("rb") as fh:
        data = tomllib.load(fh)
    advisories = data.get("advisories", [])
    if not isinstance(advisories, list):
        raise ValueError("[[advisories]] must be an array of tables")
    allowlist: dict[str, dict] = {}
    for entry in advisories:
        if not isinstance(entry, dict):
            raise ValueError("each [[advisories]] entry must be a table")
        missing = [key for key in REQUIRED_KEYS if key not in entry]
        if missing:
            raise ValueError(
                f"[[advisories]] entry missing keys: {', '.join(missing)}"
            )
        advisory_id = entry["id"]
        if not isinstance(advisory_id, str) or not advisory_id:
            raise ValueError("advisory id must be a non-empty string")
        if advisory_id in allowlist:
            raise ValueError(f"duplicate allowlist entry for {advisory_id}")
        rationale = entry["rationale"]
        if not isinstance(rationale, str) or not rationale.strip():
            raise ValueError(
                f"advisory {advisory_id}: rationale must be a non-empty string"
            )
        raw_date = entry["date"]
        if isinstance(raw_date, str):
            try:
                entry_date = date.fromisoformat(raw_date)
            except ValueError as exc:
                raise ValueError(
                    f"advisory {advisory_id}: date must be YYYY-MM-DD"
                ) from exc
        elif type(raw_date) is date:
            entry_date = raw_date
        else:
            raise ValueError(f"advisory {advisory_id}: date must be YYYY-MM-DD")
        if entry_date > date.today() + timedelta(days=FUTURE_TOLERANCE_DAYS):
            raise ValueError(
                f"advisory {advisory_id}: date {entry_date.isoformat()} is in the "
                f"future, so the entry would never expire"
            )
        allowlist[advisory_id] = {"date": entry_date, "rationale": rationale}
    return allowlist


def load_self_package(allowlist_path: Path) -> str:
    pyproject = allowlist_path.parent / "pyproject.toml"
    try:
        with pyproject.open("rb") as fh:
            data = tomllib.load(fh)
    except (OSError, tomllib.TOMLDecodeError):
        return ""
    project = data.get("project", {})
    if not isinstance(project, dict):
        return ""
    name = project.get("name", "")
    return normalize_dist_name(name) if isinstance(name, str) else ""


def parse_audit_json(text: str) -> tuple[list[dict], list[dict]]:
    data = json.loads(text)
    if not isinstance(data, dict) or not isinstance(data.get("dependencies"), list):
        raise ValueError("expected a top-level dependencies list")
    findings: list[dict] = []
    skipped: list[dict] = []
    for dep in data["dependencies"]:
        if not isinstance(dep, dict):
            raise ValueError("each dependency entry must be an object")
        name = dep.get("name", "")
        name = name if isinstance(name, str) else ""
        if "skip_reason" in dep:
            reason = dep["skip_reason"]
            if not isinstance(reason, str) or not reason.strip():
                raise ValueError(f"dependency {name!r}: skip_reason must be a string")
            skipped.append({"name": name, "reason": reason})
            continue
        version = dep.get("version", "")
        vulns = dep.get("vulns", [])
        if not isinstance(vulns, list):
            raise ValueError(f"dependency {name!r}: vulns must be a list")
        for vuln in vulns:
            if (
                not isinstance(vuln, dict)
                or not isinstance(vuln.get("id"), str)
                or not vuln["id"]
            ):
                raise ValueError(f"dependency {name!r}: vuln entry without a string id")
            findings.append(
                {
                    "id": vuln["id"],
                    "package": name,
                    "version": version,
                    "fix_versions": [
                        v for v in vuln.get("fix_versions", []) if isinstance(v, str)
                    ],
                    "aliases": [
                        a for a in vuln.get("aliases", []) if isinstance(a, str)
                    ],
                }
            )
    return findings, skipped


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Run pip-audit over the agent venv and apply the dated allowlist. "
            "pip-audit exits 1 whenever it finds advisories — that is expected "
            "output, not the verdict: this wrapper parses the JSON it printed "
            "and re-classifies each advisory against the allowlist. The "
            "allowlist TOML holds one [[advisories]] table per deferred "
            "advisory with id, date (YYYY-MM-DD) and rationale. A matched "
            "entry aged <= 90 days prints the id, rationale and entry date "
            "and passes; an entry older than 90 days — or dated in the future, "
            "which would never expire — fails the gate. Dependencies pip-audit "
            "could not audit (skip_reason) also fail it, except for the "
            "project's own distribution, which is never on PyPI by design. "
            "Exit 0 = no failing advisories; 1 = at least one unallowlisted or "
            "expired-entry advisory, or an unauditable non-self dependency; "
            "2 = allowlist file missing/malformed, pip-audit binary missing, "
            "pip-audit exiting outside its documented 0/1 contract, or "
            "unparseable / schema-violating pip-audit output (the reason goes "
            "to stderr). Typical CI use: let --root locate "
            "backend/agent_py/.pip-audit-allowlist.toml and point "
            "--pip-audit-bin at the venv's pip-audit (PATH lookup by "
            "default)."
        )
    )
    parser.add_argument(
        "--root",
        default="",
        help="repository root for the default allowlist path; default: git rev-parse --show-toplevel",
    )
    parser.add_argument(
        "--allowlist",
        default="",
        help=f"allowlist TOML path; default: <root>/{DEFAULT_ALLOWLIST}",
    )
    parser.add_argument(
        "--pip-audit-bin",
        default="pip-audit",
        help="pip-audit binary to invoke; default: pip-audit on PATH",
    )
    parser.add_argument(
        "--self-package",
        default="",
        help=(
            "distribution name of the project itself, whose skip_reason is not "
            "a gate failure; default: [project].name from the pyproject.toml "
            "sitting next to the allowlist"
        ),
    )
    args = parser.parse_args(argv)

    try:
        root = Path(args.root) if args.root else repo_root()
    except ValueError as exc:
        print(f"pip-audit-with-allowlist: {exc}", file=sys.stderr)
        return 2

    allowlist_path = (
        Path(args.allowlist) if args.allowlist else root / DEFAULT_ALLOWLIST
    )
    try:
        allowlist = load_allowlist(allowlist_path)
    except (OSError, tomllib.TOMLDecodeError, ValueError) as exc:
        print(
            f"pip-audit-with-allowlist: cannot load allowlist {allowlist_path}: {exc}",
            file=sys.stderr,
        )
        return 2

    self_package = (
        normalize_dist_name(args.self_package)
        if args.self_package
        else load_self_package(allowlist_path)
    )

    try:
        proc = subprocess.run(
            [args.pip_audit_bin, "--format", "json"],
            capture_output=True,
            text=True,
            timeout=AUDIT_TIMEOUT_SECONDS,
        )
    except FileNotFoundError:
        print(
            f"pip-audit-with-allowlist: pip-audit binary not found: {args.pip_audit_bin}",
            file=sys.stderr,
        )
        return 2
    except subprocess.TimeoutExpired:
        print(
            f"pip-audit-with-allowlist: pip-audit timed out after {AUDIT_TIMEOUT_SECONDS}s",
            file=sys.stderr,
        )
        return 2

    if proc.returncode not in (0, 1):
        stderr_lines = proc.stderr.strip().splitlines()
        tail = stderr_lines[-1][:200] if stderr_lines else "<no stderr>"
        print(
            "pip-audit-with-allowlist: pip-audit exited "
            f"{proc.returncode}, outside its 0 (clean) / 1 (advisories) "
            f"contract: {tail}",
            file=sys.stderr,
        )
        return 2

    try:
        findings, skipped = parse_audit_json(proc.stdout)
    except json.JSONDecodeError:
        stderr_lines = proc.stderr.strip().splitlines()
        tail = stderr_lines[-1][:200] if stderr_lines else "<no stderr>"
        print(
            "pip-audit-with-allowlist: unparseable pip-audit output "
            f"(exit {proc.returncode}): {tail}",
            file=sys.stderr,
        )
        return 2
    except ValueError as exc:
        print(
            f"pip-audit-with-allowlist: unexpected pip-audit JSON schema: {exc}",
            file=sys.stderr,
        )
        return 2

    today = date.today()
    failing: list[str] = []
    unaudited: list[str] = []
    matched: set[str] = set()
    for finding in findings:
        advisory_id = finding["id"]
        entry = allowlist.get(advisory_id)
        if entry is None:
            fixes = ", ".join(finding["fix_versions"]) or "none"
            aliases = ", ".join(finding["aliases"]) or "none"
            print(
                f"FAIL {advisory_id}: {finding['package']}=={finding['version']} "
                f"(fix versions: {fixes}; aliases: {aliases}) is not allowlisted"
            )
            if advisory_id not in failing:
                failing.append(advisory_id)
            continue
        matched.add(advisory_id)
        age_days = (today - entry["date"]).days
        if age_days > MAX_AGE_DAYS:
            print(
                f"FAIL {advisory_id}: allowlist entry expired (entered "
                f"{entry['date'].isoformat()}, {age_days} days old > "
                f"{MAX_AGE_DAYS}); re-evaluate the advisory or remove the entry"
            )
            if advisory_id not in failing:
                failing.append(advisory_id)
        else:
            print(
                f"ALLOW {advisory_id} (entered {entry['date'].isoformat()}, "
                f"{age_days} days old): {entry['rationale']}"
            )

    for stale_id in sorted(set(allowlist) - matched):
        print(
            f"WARN {stale_id}: allowlisted but no longer reported by pip-audit "
            f"— upstream fixed? remove the entry so it cannot mask a regression"
        )

    for skip in skipped:
        if self_package and normalize_dist_name(skip["name"]) == self_package:
            print(
                f"SKIP {skip['name']}: {skip['reason']} "
                f"(the project's own distribution; not a gate failure)"
            )
            continue
        print(
            f"FAIL {skip['name']}: pip-audit could not audit this dependency: "
            f"{skip['reason']}"
        )
        if skip["name"] not in unaudited:
            unaudited.append(skip["name"])

    if failing or unaudited:
        print(
            f"pip-audit-with-allowlist: {len(failing)} failing advisory(ies) and "
            f"{len(unaudited)} unauditable dependency(ies) out of "
            f"{len(findings)} scanned; fix them or add a dated entry to "
            f"{allowlist_path}",
            file=sys.stderr,
        )
        return 1

    allowlisted = len(matched) - len(failing)
    print(
        f"pip-audit-with-allowlist: ok ({len(findings)} advisory(ies) scanned, "
        f"{allowlisted} allowlisted, {len(skipped)} dependency(ies) skipped by "
        f"pip-audit (see above), 0 failing)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
