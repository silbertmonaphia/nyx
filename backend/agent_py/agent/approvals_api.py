"""Approval decision endpoint (openspec/changes/agent-feed-tools D7).

`POST /agent/approvals` accepts `{"run_id", "approval_id", "decision"}` and
records the decision in the shared Redis broker the paused run waits on.

Trust model: identical to `/agent/run` and the sessions surface — the Go
chat handler verifies the JWT and injects `X-Nyx-User-Id`; this service
never sees the token. A missing or non-integer header is a 400 (fail
loudly — never record as an implicit user 0). Run ownership is checked
against the run metadata layer; non-owned runs and unknown runs collapse
to the same 404 (single sentinel — no existence leak). A decision for an
approval id without a live, unexpired intent record is a typed 404 and is
never recorded (D6): a late decision cannot resurrect an expired
approval. The first decision wins (`SET NX`); duplicates return 200
without changing the recorded outcome. No internal error text ever
reaches the response body.
"""

from __future__ import annotations

import logging
import time
from typing import Literal

from fastapi import APIRouter, HTTPException, Request
from opentelemetry import trace
from pydantic import BaseModel

from .approvals import ApprovalStore
from .chat_store import ChatStoreUnavailable

log = logging.getLogger("nyx.agent.approvals_api")

router = APIRouter()

# Same tracer name as the graph nodes / sessions router; the export
# destination is owned by `setup_observability` in the lifespan.
_tracer = trace.get_tracer("nyx.agent")


class ApprovalDecisionBody(BaseModel):
    run_id: str
    approval_id: str
    decision: Literal["approve", "deny"]


@router.post("/agent/approvals")
async def post_approval(request: Request, body: ApprovalDecisionBody) -> dict:
    """Record one approval decision. Response: `{"recorded": <bool>}`.

    `recorded` is False on a duplicate decision (the first one stands).
    """
    with _tracer.start_as_current_span("agent.approvals.decide") as span:
        span.set_attribute("endpoint", "post_approval")
        user_id = _require_user_id(request)
        span.set_attribute("user_id", user_id)

        run_metadata = getattr(request.app.state, "run_metadata", None)
        # Without the metadata layer run ownership cannot be verified —
        # fail closed with the same sentinel as an unknown run.
        if run_metadata is None:
            raise HTTPException(status_code=404, detail="run not found")
        if not await _run_ownership_verified(
            request, run_metadata, body.run_id, user_id
        ):
            raise HTTPException(status_code=404, detail="run not found")

        store = _approval_store(request)
        try:
            expires_at = await store.check_intent(body.run_id, body.approval_id)
        except Exception as exc:
            log.warning("approval intent lookup failed: %r", exc)
            raise HTTPException(
                status_code=503, detail="approval store unavailable"
            ) from exc
        if expires_at is None or time.time() >= expires_at:
            # Unknown, already-consumed, or expired approval id: rejected
            # with a typed 404 and never recorded (spec D6).
            raise HTTPException(
                status_code=404, detail="approval not found or expired"
            )

        try:
            recorded = await store.record_decision(
                body.run_id,
                body.approval_id,
                body.decision,
                # Remaining window + margin (review round 1): a stalled
                # waiter must not lose an already-recorded decision.
                expires_at=expires_at,
            )
        except Exception as exc:
            log.warning("approval decision write failed: %r", exc)
            raise HTTPException(
                status_code=503, detail="approval store unavailable"
            ) from exc
        span.set_attribute("recorded", recorded)
        return {"recorded": recorded}


async def _run_ownership_verified(
    request: Request, run_metadata, run_id: str, user_id: int
) -> bool:
    """True when `user_id` owns the run, or owns the chat session the run
    continues. Session-first conversations (SPA "+ New chat" →
    `POST /agent/sessions`) never mint an `agent.runs` row — continuation
    turns key the confirmation gate on the session id — so the runs-row
    lookup alone would 404 every decision inside those conversations.
    Ownership semantics mirror the run entrypoint's dual lookup in
    `main.py`. Any store failure closes as False (fail closed, same
    single 404 sentinel as an unknown run — no existence leak).
    """
    run = await run_metadata.get_run(run_id, user_id)
    if run is not None:
        return True
    chat_store = getattr(request.app.state, "chat_store", None)
    if chat_store is None:
        return False
    try:
        session = await chat_store.get_session(run_id, user_id)
    except ChatStoreUnavailable:
        return False
    return session is not None


def _require_user_id(request: Request) -> int:
    """Read the Go-verified user id from `X-Nyx-User-Id`.

    Mirrors `/agent/run` and the sessions router exactly: the service-token
    middleware has already authenticated the caller, and this 400 is a loud
    failure for a misconfigured caller rather than a second auth check.
    """
    user_id_raw = request.headers.get("x-nyx-user-id", "").strip()
    if not user_id_raw:
        raise HTTPException(
            status_code=400,
            detail="missing X-Nyx-User-Id header (set by the Go chat handler)",
        )
    try:
        return int(user_id_raw)
    except ValueError as exc:
        raise HTTPException(
            status_code=400,
            detail=f"X-Nyx-User-Id must be an integer, got {user_id_raw!r}",
        ) from exc


def _approval_store(request: Request) -> ApprovalStore:
    """The shared Redis connection wired by the lifespan."""
    redis = getattr(request.app.state, "redis", None)
    if redis is None:
        raise HTTPException(status_code=503, detail="approval store unavailable")
    return ApprovalStore(redis)
