"""Shared transcript helpers for the golden-set graders.

A grader's `output` is the provider's transcript JSON (the frozen contract in
`openspec/changes/agent-golden-eval`): a `status`/`turns`/`meta` object whose
turns carry ordered `frames` and the concatenated `answer` text. Every grader
parses it through `load_transcript` and answers through `verdict` /
`skipped_verdict`, so the SKIPPED convention stays byte-identical across the
suite: a skipped transcript returns `pass=True, score=0.0` with a reason
prefixed `SKIPPED:` — `evals/bin/check_rate.py` keys off that prefix to keep
skipped samples out of every rate denominator.
"""

from __future__ import annotations

import functools
import json
import os
from typing import Any, Callable, Iterable

SKIPPED_PREFIX = "SKIPPED:"

DEFAULT_WEB_SEARCH_MODE = "on"


class TranscriptError(ValueError):
    """Provider output that cannot be read as a transcript."""


def load_transcript(output: Any) -> dict:
    """Coerce provider output into a transcript dict.

    Accepts the JSON string promptfoo hands a python assertion, and — for the
    unit tests and any in-process caller — an already-parsed dict.
    """
    if isinstance(output, dict):
        data = output
    elif isinstance(output, (str, bytes, bytearray)):
        try:
            data = json.loads(output)
        except (TypeError, ValueError) as exc:
            raise TranscriptError(f"transcript is not valid JSON ({exc})") from exc
    else:
        raise TranscriptError(f"transcript must be JSON, got {type(output).__name__}")

    if not isinstance(data, dict):
        raise TranscriptError(f"transcript must be a JSON object, got {type(data).__name__}")

    turns = data.get("turns")
    if turns is None:
        turns = []
    if not isinstance(turns, list):
        raise TranscriptError("transcript.turns must be a list")

    normalized = dict(data)
    normalized["turns"] = [turn if isinstance(turn, dict) else {} for turn in turns]
    return normalized


def verdict(passed: bool, reason: str, **extra: Any) -> dict:
    """Build a grader verdict in the frozen `{pass, score, reason}` shape."""
    out = {"pass": bool(passed), "score": 1.0 if passed else 0.0, "reason": reason}
    out.update(extra)
    return out


def skipped_verdict(reason: str | None) -> dict:
    """SKIPPED convention — never counts as a pass, never inflates a rate."""
    out = verdict(True, f"{SKIPPED_PREFIX} {reason or 'no skip_reason given'}")
    out["score"] = 0.0
    return out


def grader(fn: Callable[[dict, dict], dict]) -> Callable[[Any, Any], dict]:
    """Wrap a body that takes `(transcript, context)` as a `get_assert`."""

    @functools.wraps(fn)
    def get_assert(output: Any, context: Any = None) -> dict:
        try:
            transcript = load_transcript(output)
        except TranscriptError as exc:
            return verdict(False, f"unusable transcript: {exc}")
        if transcript.get("status") == "skipped":
            return skipped_verdict(transcript.get("skip_reason"))
        return fn(transcript, context if isinstance(context, dict) else {})

    return get_assert


def context_vars(context: Any) -> dict:
    """`context["vars"]` when promptfoo supplied it, else an empty mapping."""
    if not isinstance(context, dict):
        return {}
    vars_ = context.get("vars")
    return vars_ if isinstance(vars_, dict) else {}


def expect_of(context: Any) -> dict:
    """The case's `expect` block from `context["vars"]`, defensively typed.

    An absent or malformed block returns `{}`, which every grader treats as
    "nothing was declared" — a loud failure, never a silent pass.
    """
    expect = context_vars(context).get("expect")
    return expect if isinstance(expect, dict) else {}


def web_search_mode(context: Any) -> str:
    """Active web-search mode: `vars.mode.web_search`, else `EVAL_WEB_SEARCH`.

    The operator sets `EVAL_WEB_SEARCH` to match the agent's real
    `PYTHON_AGENT_WEB_SEARCH_ENABLED` flag; a mismatch surfaces as
    mode-keyed case failures, by design (agent-golden-eval D4).
    """
    mode = context_vars(context).get("mode")
    value = mode.get("web_search") if isinstance(mode, dict) else None
    if value is None:
        value = os.environ.get("EVAL_WEB_SEARCH", DEFAULT_WEB_SEARCH_MODE)
    text = str(value).strip().lower()
    return text or DEFAULT_WEB_SEARCH_MODE


def turns_of(transcript: dict) -> list[dict]:
    turns = transcript.get("turns")
    return turns if isinstance(turns, list) else []


def frames_of(turn: dict) -> list[dict]:
    frames = turn.get("frames")
    return frames if isinstance(frames, list) else []


def frame_data(frame: dict) -> dict:
    data = frame.get("data")
    return data if isinstance(data, dict) else {}


def turn_answer(turn: dict) -> str:
    """The turn's concatenated answer text (empty string when absent)."""
    answer = turn.get("answer")
    return answer if isinstance(answer, str) else ""


def clip(text: str, limit: int = 240) -> str:
    """Bound a value before it lands in a reason string."""
    text = text if isinstance(text, str) else repr(text)
    return text if len(text) <= limit else text[: limit - 3] + "..."


def token_list(tokens: Iterable[Any]) -> str:
    """Render a token sequence the way `check_rate.py` re-parses it."""
    return "[" + ", ".join(str(token) for token in tokens) + "]"
