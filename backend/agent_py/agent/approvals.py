"""Mutating-call confirmation: decision store + gate
(openspec/changes/agent-feed-tools D2/D5/D6).

Wire contract: the executor's shared dispatch funnel pauses before any
registry call whose annotations do not assert `readOnlyHint=true`, emits a
`confirm_request` frame, and waits inside the same SSE response for a
decision brokered through Redis — so a decision delivered to ANY agent
replica resolves the waiting dispatch.

Key layout (all TTL'd):

  - `approval_intent:{run_id}:{approval_id}` — written at pause time by
    the waiting process; value = the approval's expiry (epoch seconds).
    The decision endpoint requires a present, unexpired intent before
    recording, so a late decision for an expired approval gets a typed
    404 instead of resurrecting the approval (D6).
  - `approval_decision:{run_id}:{approval_id}` — `SET NX` first-wins;
    duplicate decisions no-op. Read by the waiter (~0.5 s poll).

Failure philosophy (spec: "Approval decisions are brokered across replicas
and fail closed"): every failure mode — store unavailable at pause time or
during the wait, TTL expiry, no gate configured — closes the call as
DENIED with a distinct reason. Nothing auto-approves.
"""

from __future__ import annotations

import asyncio
import logging
import time
import uuid
from redis.asyncio import Redis

from .state import AgentState
from .types import ConfirmRequestEvent, Event

log = logging.getLogger("nyx.agent.approvals")

DECISION_APPROVE = "approve"
DECISION_DENY = "deny"
DECISION_EXPIRED = "expired"
DECISION_UNAVAILABLE = "unavailable"

# Extra TTL on the Redis keys beyond the approval window: long enough for
# a late decision to meet an "intent present but expired" record (clean
# typed 404) instead of a missing key, and for the waiter's poll to read
# a recorded decision comfortably.
_KEY_MARGIN_SECONDS = 30

_POLL_INTERVAL_SECONDS = 0.5


class RunSink:
    """Live per-run SSE delivery channel (design D2 implementation note).

    Frames reach the wire through the LangGraph node boundary
    (`run_agent_stream`) only once per node, but the confirmation gate
    must surface its `confirm_request` DURING a pause inside the executor
    node. The sink is the single ordered delivery path: the node-boundary
    flush (`run_agent_stream`) and the gate's pre/post-prompt flush both
    call `flush(state_events)`, which pushes every not-yet-delivered
    append, in order, onto the run's queue — so wire order is preserved
    (step_start → confirm_request → …) regardless of which flush point
    delivers a frame.

    Queue items: `("event", Event)`, `("comment", str)` — an SSE comment
    frame for keep-alives, skipped by the existing frontend parser — or
    `None` to close the run's delivery. Lives in this module because the
    gate is its only mid-node user and owns the pause protocol;
    `run_agent_stream` consumes it as an optional parameter.
    """

    def __init__(self, queue: "asyncio.Queue") -> None:
        self._queue = queue
        self._delivered = 0

    async def flush(self, events: list) -> None:
        while self._delivered < len(events):
            await self._queue.put(("event", events[self._delivered]))
            self._delivered += 1

    async def comment(self, text: str) -> None:
        await self._queue.put(("comment", text))

    async def close(self) -> None:
        await self._queue.put(None)


class ApprovalStore:
    """Redis-backed decision broker shared across agent replicas."""

    def __init__(self, redis: Redis) -> None:
        self._redis = redis

    @staticmethod
    def intent_key(run_id: str, approval_id: str) -> str:
        return f"approval_intent:{run_id}:{approval_id}"

    @staticmethod
    def decision_key(run_id: str, approval_id: str) -> str:
        return f"approval_decision:{run_id}:{approval_id}"

    async def put_intent(
        self, run_id: str, approval_id: str, expires_at: float
    ) -> None:
        """Record that an approval was issued (proves the id is live).

        Raises on store errors — the caller closes the call as
        unavailable (fail closed).
        """
        ttl = int(expires_at - time.time()) + _KEY_MARGIN_SECONDS
        await self._redis.set(
            self.intent_key(run_id, approval_id), repr(expires_at), ex=max(ttl, 1)
        )

    async def check_intent(self, run_id: str, approval_id: str) -> float | None:
        """The approval's expiry, or None when unknown/already cleaned up.

        Raises on store errors (the endpoint maps them to a typed 503/404
        at its boundary — never internal text).
        """
        raw = await self._redis.get(self.intent_key(run_id, approval_id))
        if raw is None:
            return None
        try:
            return float(raw)
        except (TypeError, ValueError):
            return None

    async def record_decision(
        self, run_id: str, approval_id: str, decision: str
    ) -> bool:
        """`SET NX` — True when this call recorded the (first) decision.

        False on a duplicate: the first decision wins and is never
        overwritten. The key only needs to outlive the waiter's poll
        cadence after the decision, so the margin alone is its TTL.
        """
        stored = await self._redis.set(
            self.decision_key(run_id, approval_id),
            decision,
            nx=True,
            ex=_KEY_MARGIN_SECONDS,
        )
        return stored is not None

    async def get_decision(self, run_id: str, approval_id: str) -> str | None:
        raw = await self._redis.get(self.decision_key(run_id, approval_id))
        return raw if isinstance(raw, str) else None

    async def wait_for_decision(
        self, run_id: str, approval_id: str, deadline: float
    ) -> str:
        """Block until a decision is recorded or the deadline passes.

        Returns DECISION_APPROVE / DECISION_DENY / DECISION_EXPIRED /
        DECISION_UNAVAILABLE. Never raises; every failure closes the
        call as denied.
        """
        while True:
            try:
                decision = await self.get_decision(run_id, approval_id)
            except Exception as exc:  # noqa: BLE001 — fail closed
                log.warning("approval store unavailable while waiting: %r", exc)
                return DECISION_UNAVAILABLE
            if decision in (DECISION_APPROVE, DECISION_DENY):
                return decision
            if time.time() >= deadline:
                return DECISION_EXPIRED
            await asyncio.sleep(_POLL_INTERVAL_SECONDS)


class ApprovalGate:
    """Per-run confirmation orchestrator used by the dispatch funnel.

    The `sink` (optional) is the run's live SSE channel: the gate flushes
    frames appended earlier in the same executor node (step_start, …)
    before publishing the `confirm_request`, so the prompt is not
    overtaken on the wire (design D2 implementation note). Without a
    sink — unit tests — the frames ride the node-boundary flush as usual.
    """

    def __init__(
        self, store: ApprovalStore, ttl_seconds: int, sink: "RunSink | None" = None
    ) -> None:
        self._store = store
        self._ttl_seconds = ttl_seconds
        self._sink = sink

    async def request_confirmation(
        self, state: AgentState, *, step_id: str, tool_name: str, args: dict
    ) -> str:
        """Emit `confirm_request` and wait for the decision (D5/D6).

        Returns DECISION_APPROVE when the call may dispatch; otherwise
        the denial reason (deny / expired / unavailable). Never raises.
        """
        approval_id = str(uuid.uuid4())
        expires_at = time.time() + self._ttl_seconds
        try:
            await self._store.put_intent(state["run_id"], approval_id, expires_at)
        except Exception as exc:  # noqa: BLE001 — fail closed
            log.warning("approval store unavailable at pause time: %r", exc)
            return DECISION_UNAVAILABLE

        events = state["events_so_far"]
        if self._sink is not None:
            await self._sink.flush(events)
        events.append(
            Event(
                kind="confirm_request",
                payload=ConfirmRequestEvent(
                    run_id=state["run_id"],
                    step_id=step_id,
                    tool=tool_name,
                    args=args,
                    approval_id=approval_id,
                    expires_at=expires_at,
                ).model_dump(),
            )
        )
        if self._sink is not None:
            await self._sink.flush(events)

        decision = await self._store.wait_for_decision(
            state["run_id"], approval_id, expires_at
        )
        log.info(
            "approval %s for tool %r (step %r) resolved as %s",
            approval_id,
            tool_name,
            step_id,
            decision,
        )
        return decision


def denial_text_for(decision: str, tool_name: str) -> str:
    """Static, safe, model-facing denial text (no internal detail).

    Feeds the `tool_result{ok:false}` frame and the react-loop
    `ToolMessage` so the model can continue or adapt (D6); denials are
    deliberately NOT classified as correctable/transient failures.
    """
    if decision == DECISION_DENY:
        reason = "the user denied this call"
    elif decision == DECISION_EXPIRED:
        reason = "the approval request expired before a decision"
    else:
        reason = (
            "the approval service is unavailable, so the call cannot be confirmed"
        )
    return f"Confirmation required: {reason}. The `{tool_name}` tool was not executed."
