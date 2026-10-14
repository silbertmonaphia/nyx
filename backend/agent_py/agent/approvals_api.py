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
        run = await run_metadata.get_run(body.run_id, user_id)
        if run is None:
            raise HTTPException(status_code=404, detail="run not found")

        store = _approval_store(request)
        try:
            expires_at = await store.check_intent(body.run_id, body.approval_id)
        except Exception as exc:  # noqa: BLE001 — static detail, never internals
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
                body.run_id, body.approval_id, body.decision
            )
        except Exception as exc:  # noqa: BLE001
            log.warning("approval decision write failed: %r", exc)
            raise HTTPException(
                status_code=503, detail="approval store unavailable"
            ) from exc
        span.set_attribute("recorded", recorded)
        return {"recorded": recorded}


def _require_user_id(request: Request) -> int:
    """Read the Go-verified user id from `X-Nyx-User-Id`.

    Mirrors `/agent/run` and the sessions router exactly: the 400 is not
    an auth check (this hop has no auth — the network is the trust
    boundary) but a loud failure for a misconfigured caller.
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
