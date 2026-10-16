"""Plan-shape grader — strict ordered token equality against the case's expectation.

The `plan` frame's steps normalize to tokens (the step's `kind` for `rag` /
`llm` / `react`, the step's `tool` name for tool steps) and are compared to
`expect.plan` with strict ordered equality: an extra step, a missing step, a
wrong tool, or a wrong order fails the sample (agent-golden-eval D1). Exactly
one trailing `llm` token is normalized away on BOTH sides — the planner appends
an answering `llm` step when the model's plan ends on a data step, so
`[rag, create_feed]` and `[rag, create_feed, llm]` are the same shape. An extra
`llm` anywhere else (mid-sequence, or a second one at the tail) still fails.

`expect.plan` is either a plain token list or a mode matrix keyed by web-search
mode (`{on: [...], off: [...]}`). The active mode is `vars.mode.web_search`
when the corpus declares one, else `EVAL_WEB_SEARCH` (default `on`); a matrix
without an entry for the active mode reports SKIPPED rather than guessing.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from _common import (  # noqa: E402
    clip,
    expect_of,
    frame_data,
    frames_of,
    grader,
    skipped_verdict,
    token_list,
    turns_of,
    verdict,
    web_search_mode,
)

STEP_KIND_TOKENS = frozenset({"rag", "llm", "react"})
ANSWER_TAIL_TOKEN = "llm"
MISSING_TOOL_NAME = "<missing-tool-name>"


def normalize_steps(steps) -> list[str]:
    """Normalize plan steps to the frozen token vocabulary."""
    tokens: list[str] = []
    for step in steps if isinstance(steps, list) else []:
        if not isinstance(step, dict):
            tokens.append("<malformed-step>")
            continue
        kind = step.get("kind")
        if kind == "tool":
            name = step.get("tool")
            tokens.append(name if isinstance(name, str) and name else MISSING_TOOL_NAME)
        elif kind in STEP_KIND_TOKENS:
            tokens.append(kind)
        else:
            tokens.append(f"<unknown-kind:{kind}>")
    return tokens


def strip_answer_tail(tokens: list[str]) -> list[str]:
    """Drop exactly one trailing answering `llm` token."""
    if tokens and tokens[-1] == ANSWER_TAIL_TOKEN:
        return tokens[:-1]
    return list(tokens)


def last_plan_steps(transcript: dict) -> tuple[int, list | None]:
    """Steps of the last turn's last `plan` frame: `(turn index, steps|None)`."""
    turns = turns_of(transcript)
    if not turns:
        return 0, None
    turn_index = len(turns)
    steps = None
    for frame in frames_of(turns[-1]):
        if frame.get("event") == "plan":
            steps = frame_data(frame).get("steps")
    return turn_index, steps


def select_expectation(expect: dict, context: dict) -> tuple[list[str] | None, dict | None]:
    """Resolve `expect.plan` for the active mode.

    Returns `(tokens, None)` when an expectation applies, `(None, skip)` when
    the matrix has no entry for the active mode, and `(None, None)` when the
    case declares no plan expectation at all.
    """
    plan = expect.get("plan")
    if plan is None:
        return None, None
    if isinstance(plan, dict):
        mode = web_search_mode(context)
        if mode not in plan:
            return None, {
                "reason": f"no plan expectation for web_search={mode}",
                "mode": mode,
            }
        return [str(token) for token in plan[mode] or []], None
    return [str(token) for token in plan], None


@grader
def get_assert(transcript: dict, context: dict) -> dict:
    expect = expect_of(context)
    expected, skip = select_expectation(expect, context)
    if skip is not None:
        return skipped_verdict(skip["reason"])
    if expected is None:
        return verdict(False, "expect.plan is not set; nothing to compare")

    turn_index, steps = last_plan_steps(transcript)
    if steps is None:
        return verdict(
            False,
            f"no plan frame in the last turn (turn {turn_index} of {len(turns_of(transcript))})",
        )
    if not steps:
        return verdict(False, f"plan frame in turn {turn_index} carries no steps")

    raw_plan = {"steps": steps}
    observed = normalize_steps(steps)
    expected_tokens = strip_answer_tail(expected)
    observed_tokens = strip_answer_tail(observed)
    metadata = {
        "expected_tokens": expected_tokens,
        "observed_tokens": observed_tokens,
        "raw_plan": raw_plan,
        "mode": web_search_mode(context),
    }

    if expected_tokens == observed_tokens:
        return verdict(
            True,
            f"plan ok: expected={token_list(expected_tokens)} observed={token_list(observed_tokens)}",
            metadata=metadata,
        )

    reason = (
        f"plan mismatch: expected={token_list(expected_tokens)} "
        f"observed={token_list(observed_tokens)} "
        f"raw_plan={clip(json.dumps(raw_plan, ensure_ascii=False), 1500)}"
    )
    return verdict(False, reason, metadata=metadata)
