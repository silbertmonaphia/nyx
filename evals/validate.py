#!/usr/bin/env python
"""CLI: validate the golden-set corpus before anything calls the model.

    python evals/validate.py [--cases-glob 'evals/cases/*.yaml'] ...

Exits 0 when every case loads, 1 when a case is invalid (the offending case id
and field are named on stderr), 2 on an unexpected internal error. Relative
globs resolve against the current directory first, then the repo root.
"""

from __future__ import annotations

import argparse
import sys
from collections import Counter
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from lib.corpus import CorpusError, load_corpus  # noqa: E402

DEFAULT_CASES_GLOB = "evals/cases/*.yaml"


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="validate.py",
        description="Validate the golden-set corpus (ids, plan tokens, frame kinds, floors).",
    )
    parser.add_argument(
        "--cases-glob",
        action="append",
        default=None,
        metavar="GLOB",
        help=f"corpus glob, repeatable (default: {DEFAULT_CASES_GLOB!r})",
    )
    parser.add_argument(
        "--quiet",
        action="store_true",
        help="print nothing on success",
    )
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    patterns = args.cases_glob or [DEFAULT_CASES_GLOB]

    try:
        cases = load_corpus(patterns)
    except CorpusError as exc:
        print(f"error: {exc.render()}", file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001 — the CLI boundary must not traceback at operators
        print(f"error: unexpected validation failure: {exc!r}", file=sys.stderr)
        return 2

    if not args.quiet:
        lines = Counter(case.line for case in cases)
        files = len({case.source for case in cases})
        breakdown = ", ".join(f"{line}={count}" for line, count in sorted(lines.items()))
        print(f"ok: {len(cases)} case(s) from {files} file(s) ({breakdown})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
