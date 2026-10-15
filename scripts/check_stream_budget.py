import argparse
import re
import subprocess
import sys
from pathlib import Path

CAP_PATTERN = re.compile(
    r'viper\.SetDefault\("LLM_MAX_STREAM_DURATION",\s*"([^"]+)"\)'
)
TTL_PATTERN = re.compile(
    r"^\s*approval_ttl_seconds:\s*int\s*=\s*(\d+)", re.MULTILINE
)
DURATION_PATTERN = re.compile(r"^(\d+(?:\.\d+)?)([smhd])$")
DURATION_FACTORS = {"s": 1, "m": 60, "h": 3600, "d": 86400}

MIN_APPROVALS = 1
SLACK_SECONDS = 120


def parse_duration(text: str) -> float:
    match = DURATION_PATTERN.match(text.strip())
    if not match:
        raise ValueError(f"unsupported duration literal: {text!r}")
    return float(match.group(1)) * DURATION_FACTORS[match.group(2)]


def read_defaults(root: Path) -> tuple[float, float]:
    go_source = (root / "backend/internal/platform/config/config.go").read_text()
    py_source = (root / "backend/agent_py/agent/config.py").read_text()
    cap_match = CAP_PATTERN.search(go_source)
    if not cap_match:
        raise ValueError(
            "LLM_MAX_STREAM_DURATION default not found in backend/internal/platform/config/config.go"
        )
    ttl_match = TTL_PATTERN.search(py_source)
    if not ttl_match:
        raise ValueError(
            "approval_ttl_seconds default not found in backend/agent_py/agent/config.py"
        )
    return parse_duration(cap_match.group(1)), float(ttl_match.group(1))


def required_cap(ttl_seconds: float) -> float:
    return ttl_seconds * MIN_APPROVALS + SLACK_SECONDS


def repo_root() -> Path:
    out = subprocess.run(
        ["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True
    )
    if out.returncode != 0:
        raise ValueError("not inside a git worktree; pass --root")
    return Path(out.stdout.strip())


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Fail when the default chat-stream cap (LLM_MAX_STREAM_DURATION, "
            "Go) is shorter than one full approval wait "
            "(PYTHON_AGENT_APPROVAL_TTL_SECONDS default, Python) plus slack — "
            "i.e. when a default-configured run could not even answer a single "
            "confirmation before the stream cap cuts it (the 2026-10-14 "
            "60s-cut incident class, in config form). Multi-approval plans are "
            "out of the hard bound by design: a cap cut emits "
            'error{code:"stream_timeout"} and the run stays resumable via its '
            "echoed run_id. Reads both defaults from source; exits 2 when "
            "either cannot be parsed (config moved → update this check)."
        )
    )
    parser.add_argument(
        "--root",
        default="",
        help="repository root to read defaults from; default: git rev-parse --show-toplevel",
    )
    args = parser.parse_args(argv)

    try:
        root = Path(args.root) if args.root else repo_root()
        cap_seconds, ttl_seconds = read_defaults(root)
    except (OSError, ValueError) as exc:
        print(f"check-stream-budget: cannot read defaults: {exc}", file=sys.stderr)
        return 2

    needed = required_cap(ttl_seconds)
    if cap_seconds < needed:
        print(
            "check-stream-budget: LLM_MAX_STREAM_DURATION default "
            f"({cap_seconds:.0f}s) < approval TTL default ({ttl_seconds:.0f}s) "
            f"x {MIN_APPROVALS} + {SLACK_SECONDS}s slack (needs >= {needed:.0f}s)",
            file=sys.stderr,
        )
        print(
            "A stream must be able to carry at least one full approval wait. "
            "Raise LLM_MAX_STREAM_DURATION or lower PYTHON_AGENT_APPROVAL_TTL_SECONDS.",
            file=sys.stderr,
        )
        return 1

    print(
        f"check-stream-budget: ok ({cap_seconds:.0f}s cap >= {ttl_seconds:.0f}s "
        f"TTL + {SLACK_SECONDS}s slack; multi-approval runs rely on "
        "stream_timeout frames + resume)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
