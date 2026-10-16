"""Terminal-frame grader — the run ended with the declared error code.

`expect.error_code` (e.g. `plan_too_large`, `budget_exceeded`) passes only when
a transcript `error` frame carries that `data.code`. A run that never errored,
or errored with a different code, fails: the case exists to pin a specific
failure mode, and a healthy-looking answer is the wrong outcome.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from _common import (  # noqa: E402
    expect_of,
    frame_data,
    frames_of,
    grader,
    turns_of,
    verdict,
)


@grader
def get_assert(transcript: dict, context: dict) -> dict:
    expect = expect_of(context)
    expected_code = expect.get("error_code")
    if not isinstance(expected_code, str) or not expected_code:
        return verdict(False, "expect.error_code is not set; nothing to assert")

    observed: list[tuple[int, str | None]] = []
    saw_done = False
    for turn_index, turn in enumerate(turns_of(transcript), start=1):
        for frame in frames_of(turn):
            event = frame.get("event")
            if event == "error":
                code = frame_data(frame).get("code")
                observed.append((turn_index, code if isinstance(code, str) else None))
            elif event == "done":
                saw_done = True

    for turn_index, code in observed:
        if code == expected_code:
            return verdict(True, f"error frame with code {expected_code!r} (turn {turn_index})")

    if not observed:
        ending = "the run emitted a done frame" if saw_done else "no terminal frame"
        return verdict(
            False,
            f"expected error code {expected_code!r}; no error frame in the transcript ({ending})",
        )

    rendered = ", ".join(repr(code) for _, code in observed)
    return verdict(
        False,
        f"expected error code {expected_code!r}; observed error code(s): {rendered}",
    )
