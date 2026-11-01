"""In-process frame replay buffer for one run's SSE stream.

The contract (openspec/changes/agent-stream-frame-replay): every frame a
run emits is appended here BEFORE it reaches any subscriber, under a
monotonic decimal sequence. A reconnecting client presents the last
sequence it rendered as a cursor; `since` returns the finished frame
bytes strictly after that cursor, or `TRUNCATED` when the cursor is
malformed or names a frame the ring has already rolled past — the caller
then falls back to the step projection rather than replaying a span with
holes.

The buffer is deliberately in-process (`collections.deque`): it lives
and dies with the run's session, makes no network calls, and can never
fail its append short of process death — which is what makes the
append-before-broadcast invariant unconditional. Redis was considered
and rejected: this project never lets Redis hold the only copy of
anything, and re-attach must land on the instance that holds the run
anyway (the detached registry is process-local).
"""

from __future__ import annotations

from collections import deque
from typing import Final


class TruncatedType:
    """Sentinel returned by `FrameLog.since` for an unservable cursor."""

    __slots__ = ()

    def __repr__(self) -> str:
        return "TRUNCATED"


TRUNCATED: Final = TruncatedType()


def parse_cursor(raw: str | int | None) -> int | None:
    """Parse a client-supplied cursor; `None` when malformed or absent.

    Absent (`None` in, `None` out) means the caller takes the no-cursor
    branch; malformed (non-integer text) ALSO maps to `None` here and the
    caller reports truncation only when a cursor was actually presented —
    so the two cases stay distinguishable at the call site.
    """
    if raw is None:
        return None
    if isinstance(raw, int):
        return raw if raw >= 0 else None
    try:
        value = int(raw.strip())
    except (ValueError, AttributeError):
        return None
    return value if value >= 0 else None


class FrameLog:
    """Bounded ring of finished frame bytes, keyed by monotonic sequence."""

    def __init__(self, maxlen: int) -> None:
        self._frames: deque[tuple[int, bytes]] = deque(maxlen=maxlen)
        self._seq = 0

    def append(self, body: bytes) -> tuple[int, bytes]:
        """Stamp `body` with the next id and store the finished frame.

        Returns `(seq, frame)` where `frame` is the exact bytes to put on
        the wire — the `id:` line plus the body. The ring holds those
        finished bytes, so a replay is byte-identical to the original
        rather than a re-stamped reconstruction of it.
        """
        self._seq += 1
        frame = b"id: %d\n" % self._seq + body
        self._frames.append((self._seq, frame))
        return self._seq, frame

    def watermark(self) -> int:
        return self._seq

    def oldest_seq(self) -> int | None:
        if not self._frames:
            return None
        return self._frames[0][0]

    def since(self, cursor: int | None, upto: int) -> list[bytes] | TruncatedType:
        """Finished frame bytes with `cursor < seq <= upto`, oldest first.

        Returns `TRUNCATED` when the cursor cannot be served contiguously
        from the ring: malformed, above the watermark (a client from the
        future), or pointing at a frame whose successor has already been
        dropped (the ring rolled past the span the client still needs).
        An empty result means the client missed nothing in `(cursor, upto]`.

        The rolled-past test is `cursor + 1 < oldest`: the client needs
        every frame in `(cursor, oldest)`, and only those are gone. A
        cursor naming the frame just below the oldest retained one is
        still servable — it needs that frame's successor onwards, all of
        which are still held.
        """
        if cursor is None or cursor > upto:
            return TRUNCATED
        oldest = self.oldest_seq()
        if oldest is not None and cursor + 1 < oldest:
            return TRUNCATED
        return [frame for seq, frame in self._frames if cursor < seq <= upto]
