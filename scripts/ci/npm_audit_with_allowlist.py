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
GATE_SEVERITIES = ("high", "critical")
DEFAULT_ALLOWLIST = Path("frontend/.npm-audit-allowlist.toml")
REQUIRED_KEYS = ("id", "date", "rationale")
GHSA_RE = re.compile(r"GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}", re.IGNORECASE)


def repo_root() -> Path:
    out = subprocess.run(
        ["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True
    )
    if out.returncode != 0:
        raise ValueError("not inside a git worktree; pass --root")
    return Path(out.stdout.strip())


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
        if not GHSA_RE.fullmatch(advisory_id):
            raise ValueError(f"advisory id must be a GHSA id: {advisory_id!r}")
        key = advisory_id.upper()
        if key in allowlist:
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
        allowlist[key] = {"id": advisory_id, "date": entry_date, "rationale": rationale}
    return allowlist


def parse_audit_json(text: str) -> dict:
    data = json.loads(text)
    if not isinstance(data, dict):
        raise ValueError("expected a top-level object")
    vulnerabilities = data.get("vulnerabilities")
    if not isinstance(vulnerabilities, dict):
        raise ValueError("expected a top-level vulnerabilities object")
    for name, info in vulnerabilities.items():
        if not isinstance(name, str) or not isinstance(info, dict):
            raise ValueError("each vulnerabilities entry must be an object")
        via = info.get("via", [])
        if not isinstance(via, list):
            raise ValueError(f"vulnerability {name!r}: via must be a list")
        for entry in via:
            if not isinstance(entry, (str, dict)):
                raise ValueError(
                    f"vulnerability {name!r}: via entries must be strings or objects"
                )
    return data


def advisory_id_for(entry: dict) -> str | None:
    url = entry.get("url", "")
    if not isinstance(url, str):
        return None
    match = GHSA_RE.search(url)
    return match.group(0).upper() if match else None


def seen_advisory_ids(vulnerabilities: dict) -> set[str]:
    seen: set[str] = set()
    for info in vulnerabilities.values():
        for entry in info.get("via", []):
            if isinstance(entry, dict):
                advisory_id = advisory_id_for(entry)
                if advisory_id is not None:
                    seen.add(advisory_id)
    return seen


def classify(vulnerabilities: dict, allowlist: dict[str, dict]) -> list[dict]:
    gate = {
        name: info
        for name, info in vulnerabilities.items()
        if info.get("severity") in GATE_SEVERITIES
    }
    covered: set[str] = set()
    blocked: dict[str, str] = {}
    changed = True
    while changed:
        changed = False
        for name, info in gate.items():
            if name in covered:
                continue
            reasons: list[str] = []
            for entry in info.get("via", []):
                if isinstance(entry, str):
                    target = vulnerabilities.get(entry)
                    if (
                        target is not None
                        and target.get("severity") not in GATE_SEVERITIES
                    ):
                        continue
                    if entry not in covered:
                        reasons.append(f"depends on vulnerable {entry!r}")
                    continue
                if entry.get("severity") not in GATE_SEVERITIES:
                    continue
                advisory_id = advisory_id_for(entry)
                if advisory_id is None:
                    reasons.append("advisory without a GHSA id")
                elif advisory_id not in allowlist:
                    reasons.append(f"{advisory_id} is not allowlisted")
            if reasons:
                blocked[name] = "; ".join(dict.fromkeys(reasons))
                continue
            covered.add(name)
            changed = True
    return [
        {
            "name": name,
            "severity": info.get("severity"),
            "reason": blocked.get(name, "not covered"),
        }
        for name, info in sorted(gate.items())
        if name not in covered
    ]


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Run npm audit over the frontend and apply the dated allowlist. "
            "npm exits 1 whenever it reports advisories — that is expected "
            "output, not the verdict: this wrapper parses the JSON and "
            "re-classifies each high/critical vulnerability against the "
            "allowlist, walking the via chain so a deferred advisory also "
            "covers the packages that inherit it. The allowlist TOML holds one "
            "[[advisories]] table per deferred advisory with id (GHSA), date "
            "(YYYY-MM-DD) and rationale. A matched entry aged <= 90 days "
            "passes; older than 90 days — or dated in the future, which would "
            "never expire — fails the gate. Moderate/low advisories do not "
            "gate, matching --audit-level=high. Exit 0 = no failing "
            "advisories; 1 = at least one unallowlisted or expired-entry "
            "advisory; 2 = allowlist missing/malformed, npm missing, npm "
            "exiting outside its documented 0/1 contract, or unparseable / "
            "schema-violating npm output (the reason goes to stderr)."
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
        "--npm-bin",
        default="npm",
        help="npm binary to invoke; default: npm on PATH",
    )
    args = parser.parse_args(argv)

    try:
        root = Path(args.root) if args.root else repo_root()
    except ValueError as exc:
        print(f"npm-audit-with-allowlist: {exc}", file=sys.stderr)
        return 2

    allowlist_path = (
        Path(args.allowlist) if args.allowlist else root / DEFAULT_ALLOWLIST
    )
    try:
        allowlist = load_allowlist(allowlist_path)
    except (OSError, tomllib.TOMLDecodeError, ValueError) as exc:
        print(
            f"npm-audit-with-allowlist: cannot load allowlist {allowlist_path}: {exc}",
            file=sys.stderr,
        )
        return 2

    try:
        proc = subprocess.run(
            [args.npm_bin, "audit", "--json", "--workspaces=false"],
            cwd=root / "frontend",
            capture_output=True,
            text=True,
            timeout=AUDIT_TIMEOUT_SECONDS,
        )
    except FileNotFoundError:
        print(
            f"npm-audit-with-allowlist: npm binary not found: {args.npm_bin}",
            file=sys.stderr,
        )
        return 2
    except subprocess.TimeoutExpired:
        print(
            f"npm-audit-with-allowlist: npm audit timed out after {AUDIT_TIMEOUT_SECONDS}s",
            file=sys.stderr,
        )
        return 2

    if proc.returncode not in (0, 1):
        stderr_lines = proc.stderr.strip().splitlines()
        tail = stderr_lines[-1][:200] if stderr_lines else "<no stderr>"
        print(
            "npm-audit-with-allowlist: npm exited "
            f"{proc.returncode}, outside its 0 (clean) / 1 (advisories) "
            f"contract: {tail}",
            file=sys.stderr,
        )
        return 2

    try:
        report = parse_audit_json(proc.stdout)
    except json.JSONDecodeError:
        stderr_lines = proc.stderr.strip().splitlines()
        tail = stderr_lines[-1][:200] if stderr_lines else "<no stderr>"
        print(
            "npm-audit-with-allowlist: unparseable npm output "
            f"(exit {proc.returncode}): {tail}",
            file=sys.stderr,
        )
        return 2
    except ValueError as exc:
        print(
            f"npm-audit-with-allowlist: unexpected npm JSON schema: {exc}",
            file=sys.stderr,
        )
        return 2

    vulnerabilities = report["vulnerabilities"]
    seen = seen_advisory_ids(vulnerabilities)
    today = date.today()
    failing: list[str] = []

    for advisory_id in sorted(seen & set(allowlist)):
        entry = allowlist[advisory_id]
        age_days = (today - entry["date"]).days
        if age_days > MAX_AGE_DAYS:
            print(
                f"FAIL {entry['id']}: allowlist entry expired (entered "
                f"{entry['date'].isoformat()}, {age_days} days old > "
                f"{MAX_AGE_DAYS}); re-evaluate the advisory or remove the entry"
            )
            failing.append(advisory_id)
        else:
            print(
                f"ALLOW {entry['id']} (entered {entry['date'].isoformat()}, "
                f"{age_days} days old): {entry['rationale']}"
            )

    for stale_id in sorted(set(allowlist) - seen):
        print(
            f"WARN {allowlist[stale_id]['id']}: allowlisted but no longer "
            f"reported by npm audit — upstream fixed? remove the entry so it "
            f"cannot mask a regression"
        )

    for finding in classify(vulnerabilities, allowlist):
        print(
            f"FAIL {finding['name']}: {finding['severity']} vulnerability is "
            f"not covered by the allowlist ({finding['reason']})"
        )
        failing.append(finding["name"])

    if failing:
        print(
            f"npm-audit-with-allowlist: {len(failing)} failing item(s); fix "
            f"them or add a dated entry to {allowlist_path}",
            file=sys.stderr,
        )
        return 1

    counts = report.get("metadata", {}).get("vulnerabilities", {})
    print(
        "npm-audit-with-allowlist: no unallowlisted high/critical advisories "
        f"(thresholds: {counts})"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
