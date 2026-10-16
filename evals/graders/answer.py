"""Answer-content grader — deterministic keyword assertions, no LLM judge.

`expect.answer_contains` / `expect.answer_forbidden` assert on the LAST turn's
`answer` text (the provider's concatenation of that turn's `delta` frames).
`expect.turns` overrides the target per entry: `{index: 1-based turn,
answer_contains: [...], answer_forbidden: [...]}` asserts on that turn's answer
— the memory scenario's seed turn and its recall turns are graded that way.

An empty answer fails a `contains` assertion (nothing was said) and vacuously
satisfies a `forbidden` one (nothing forbidden was said).
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from _common import (  # noqa: E402
    clip,
    expect_of,
    grader,
    turn_answer,
    turns_of,
    verdict,
)


def _substrings(value, field: str, case_prefix: str) -> list[str]:
    if value is None:
        return []
    if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
        raise ValueError(f"{case_prefix}{field} must be a list of strings")
    return list(value)


def _assertions(expect: dict) -> list[tuple[int | None, str, str]]:
    """Flatten the expectation into `(turn index | None for last, kind, text)`."""
    assertions: list[tuple[int | None, str, str]] = []

    for text in _substrings(expect.get("answer_contains"), "answer_contains", ""):
        assertions.append((None, "contains", text))
    for text in _substrings(expect.get("answer_forbidden"), "answer_forbidden", ""):
        assertions.append((None, "forbidden", text))

    turns = expect.get("turns") or []
    if not isinstance(turns, list):
        raise ValueError("expect.turns must be a list")
    for position, entry in enumerate(turns):
        if not isinstance(entry, dict):
            raise ValueError(f"expect.turns[{position}] must be a mapping")
        index = entry.get("index")
        if not isinstance(index, int) or isinstance(index, bool) or index < 1:
            raise ValueError(f"expect.turns[{position}].index must be a positive integer")
        prefix = f"turns[{position}]."
        for text in _substrings(entry.get("answer_contains"), "answer_contains", prefix):
            assertions.append((index, "contains", text))
        for text in _substrings(entry.get("answer_forbidden"), "answer_forbidden", prefix):
            assertions.append((index, "forbidden", text))

    return assertions


@grader
def get_assert(transcript: dict, context: dict) -> dict:
    expect = expect_of(context)
    try:
        assertions = _assertions(expect)
    except ValueError as exc:
        return verdict(False, f"invalid expectation: {exc}")

    if not assertions:
        return verdict(False, "no answer assertions declared in expect")

    turns = turns_of(transcript)
    failures: list[str] = []
    for index, kind, text in assertions:
        target = len(turns) if index is None else index
        label = f"turn {target}"
        if target < 1 or target > len(turns):
            failures.append(
                f"{label}: transcript has {len(turns)} turn(s); cannot grade turn {target}"
            )
            continue
        answer = turn_answer(turns[target - 1])
        if kind == "contains":
            if not answer:
                failures.append(f"{label}: answer is empty; expected to contain {text!r}")
            elif text not in answer:
                failures.append(
                    f"{label}: answer must contain {text!r}; got {clip(answer)!r}"
                )
        elif text in answer:
            failures.append(
                f"{label}: answer must NOT contain {text!r}; got {clip(answer)!r}"
            )

    if failures:
        return verdict(False, "answer content failed: " + " | ".join(failures))
    return verdict(True, f"answer content ok ({len(assertions)} assertion(s))")
