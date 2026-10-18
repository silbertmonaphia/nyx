"""Data-subject endpoints: export + erasure (openspec/changes/agent-gdpr-compliance).

Two owner-scoped endpoints, mirroring the `/agent/sessions` trust model
1:1 (see `chat_sessions.py`): the Go handler verifies the user JWT and
injects the verified id as `X-Nyx-User-Id`; this service never sees the
token. A missing or non-integer header is a 400 (fail loudly — never run
as an implicit user 0, which would export or erase the wrong subject's
data). Every store call carries that user id and there is no parameter a
caller could point at another user — no admin override exists.

  - `GET    /agent/me/export` — one JSON body carrying every row tied to
    the subject: all `agent.chat_sessions` rows (soft-deleted INCLUDED —
    a subject-access request must not hide rows that still exist), all
    their `agent.chat_messages` (content included — it is the subject's
    own data), all `agent.chat_user_prefs` rows, all `agent.runs` rows,
    plus `checkpoint_thread_ids` — every id that keys the user's
    langGraph checkpoint threads: run ids (single-turn runs) ∪ session
    ids (continuation turns bind their thread to the session and carry
    no `agent.runs` row — review 2026-10-14 compliance fix). Ids only,
    by design (D2): langGraph's checkpoint format is internal, and the
    thread id IS the unit of erasure, so the id list is the complete
    representation of "what holds your state and can be deleted". A user
    with no data gets the same object with empty arrays — never a 404.
    Every call appends an `export` audit row carrying counts only (D4).
  - `DELETE /agent/me` — physical erasure in the D3 order: messages →
    sessions → prefs (the store's single transaction) → `agent.runs`
    (capturing the deleted run ids) → `checkpointer.adelete_thread` per
    thread id (run ids ∪ the session ids the chat store returned) →
    audit row. A per-thread failure is warn-logged, counted as
    NOT deleted, and never aborts the remaining ids or the audit row
    (D3: the erase request was served — the response counts tell the
    operator exactly what each stage deleted). Idempotent: a second call
    deletes nothing and returns zero counts with 200 (with its own audit
    row).

The stores own all SQL (D7); this module only composes store calls.
Serialization is explicit: asyncpg hands back `uuid.UUID` / `datetime`
objects, so every response field is converted to `str` / ISO-8601 here.

Each endpoint opens one OTel span on the shared `nyx.agent` tracer
(`agent.gdpr.export` / `agent.gdpr.erase`) with `endpoint` + `user_id`
attributes and the per-store counts; the erase span additionally carries
`thread_failures` so a contained checkpoint-GC failure stays visible
without an error status on the span.

Kill switch (design D6): when `PYTHON_AGENT_GDPR_ENABLED=false` both
endpoints return 503 with the standard error envelope and touch nothing —
no store read, no store write, no audit row ("the forbidden operation
never happened"). The check runs FIRST, before any header parsing, so a
disabled service has exactly one observable behavior on these routes.
"""

from __future__ import annotations

import logging
from datetime import UTC, datetime

from fastapi import APIRouter, HTTPException, Request
from fastapi.responses import JSONResponse
from opentelemetry import trace
from prometheus_client import Counter

from ..audit import AuditLog
from ..chat_store import ChatSession, ChatStore, MessageRow
from ..config import Settings, get_settings
from ..prefs import Pref, PrefsStore
from ..run_metadata import RunMetadata, RunSummary
from .deps import require_user_id as _deps_require_user_id

log = logging.getLogger("nyx.agent.api.gdpr")

# Review M1 (2026-10-14): an erasure that cannot write its audit row
# still returns 200 (the data IS gone; a 5xx would misreport and
# invite a misleading retry) — but the lost evidence must be
# observable. A nonzero rate here means the operator must reconcile
# the affected erasures from the ERROR log lines.
agent_gdpr_audit_write_failed_total = Counter(
    "agent_gdpr_audit_write_failed_total",
    "GDPR operations that completed but whose audit row could not be "
    "written (endpoint label: export|erase). The operation itself was "
    "served; the compliance trail for it is missing — reconcile from logs.",
    ["endpoint"],
)

router = APIRouter()

# Same tracer name as the graph nodes, retention cron and sweeper; the
# export destination is owned by `setup_observability` in the lifespan.
_tracer = trace.get_tracer("nyx.agent")


# `response_model=None`: both handlers return `dict | JSONResponse`
# (503 uses a JSONResponse with the standard error envelope), which is
# not a valid Pydantic response model — the payloads are serialized
# explicitly by the `_*_json` helpers, so no model generation is wanted.
@router.get("/agent/me/export", response_model=None)
async def export_me(request: Request) -> dict | JSONResponse:
    """Full data dump for the `X-Nyx-User-Id` subject.

    Response: `{"user_id", "exported_at", "sessions", "messages",
    "prefs", "runs", "checkpoint_thread_ids", "audit_entries"}` — all
    arrays, all rows
    for the header user (soft-deleted session rows included). 503 when
    the GDPR kill switch is off; 400 on a missing / non-integer header.
    """
    with _tracer.start_as_current_span("agent.gdpr.export") as span:
        span.set_attribute("endpoint", "export_me")
        if not _settings(request).gdpr_enabled:
            span.set_attribute("gdpr_disabled", True)
            return _disabled_response()
        user_id = _require_user_id(request)
        span.set_attribute("user_id", user_id)

        chat_store = _chat_store(request)
        prefs_store = _prefs_store(request)
        run_metadata = _run_metadata(request)
        audit = _audit_log(request)

        # Read order mirrors the response composition; each store returns
        # every row for the user (no pagination — D1: the data volume per
        # subject is bounded by retention and a single JSON body keeps the
        # surface inspectable and testable).
        sessions = await chat_store.list_all_sessions_for_user(user_id)
        messages = await chat_store.list_all_messages_for_user(user_id)
        prefs = await prefs_store.list_all_prefs_for_user(user_id)
        runs = await run_metadata.list_all_runs_for_user(user_id)
        # The subject's own audit records (review N2, decision: INCLUDE —
        # they are counts-only by construction, so there is no privacy
        # risk, and a subject-access request covers "records about the
        # data subject"; the log stays operator-internal for everyone else).
        audit_entries = await audit.list_for_user(user_id)
        # langGraph threads are keyed by BOTH id kinds: single-turn runs
        # bind the thread to the run id, and continuation turns (Slice 2)
        # bind it to the SESSION id with no agent.runs row at all — so the
        # complete thread list is run ids ∪ session ids (review 2026-10-14
        # compliance fix; deduped because a single-turn session's id IS
        # its run id).
        thread_ids = list(
            dict.fromkeys(
                [str(run.id) for run in runs] + [str(s.id) for s in sessions]
            )
        )

        span.set_attribute("sessions", len(sessions))
        span.set_attribute("messages", len(messages))
        span.set_attribute("prefs", len(prefs))
        span.set_attribute("runs", len(runs))
        span.set_attribute("threads", len(thread_ids))
        span.set_attribute("audit_entries", len(audit_entries))

        # Audit AFTER the reads succeed: the export has already been
        # served at this point, so an audit-store failure must not fail
        # the subject's request — it is warn-logged instead. Counts only,
        # never content (D4): the audit log must not become a shadow copy
        # of the data it audits.
        try:
            await audit.record(
                user_id,
                "export",
                {
                    "sessions": len(sessions),
                    "messages": len(messages),
                    "prefs": len(prefs),
                    "runs": len(runs),
                    "threads": len(thread_ids),
                    "audit_entries": len(audit_entries),
                },
            )
        except Exception as exc:  # noqa: BLE001 — warn-only by contract
            agent_gdpr_audit_write_failed_total.labels(endpoint="export").inc()
            log.warning(
                "gdpr export: audit record failed (non-fatal): user_id=%s err=%r",
                user_id,
                exc,
            )

        return {
            "user_id": user_id,
            "exported_at": datetime.now(UTC).isoformat(),
            "sessions": [_session_json(s) for s in sessions],
            "messages": [_message_json(m) for m in messages],
            "prefs": [_pref_json(p) for p in prefs],
            "runs": [_run_json(r) for r in runs],
            "checkpoint_thread_ids": thread_ids,
            "audit_entries": [
                {
                    "action": row.action,
                    "payload_summary": row.payload_summary,
                    "requested_at": row.requested_at.isoformat(),
                }
                for row in audit_entries
            ],
        }


@router.delete("/agent/me", response_model=None)
async def erase_me(request: Request) -> dict | JSONResponse:
    """Right to erasure: physically delete every row of the header user.

    Runs stages 1–3 (see module docstring), then appends the erase audit
    row and responds `{"deleted": {"sessions", "messages", "prefs",
    "runs", "threads"}}`. 503 when the GDPR kill switch is off; 400 on a
    missing / non-integer header. Idempotent: a second call is 200 with
    all-zero counts.
    """
    with _tracer.start_as_current_span("agent.gdpr.erase") as span:
        span.set_attribute("endpoint", "erase_me")
        if not _settings(request).gdpr_enabled:
            span.set_attribute("gdpr_disabled", True)
            return _disabled_response()
        user_id = _require_user_id(request)
        span.set_attribute("user_id", user_id)

        chat_store = _chat_store(request)
        run_metadata = _run_metadata(request)
        audit = _audit_log(request)

        # Stage 1 (D3): messages -> sessions -> prefs, all inside the
        # store's single transaction. Children before parents — explicit
        # statements (not FK CASCADE side effects) so the counts are
        # exact. Soft-deleted sessions are matched too: retention is not
        # a precondition for erasure.
        chat_counts = await chat_store.delete_all_for_user(user_id)
        sessions_deleted = int(chat_counts.get("sessions", 0))
        messages_deleted = int(chat_counts.get("messages", 0))
        prefs_deleted = int(chat_counts.get("prefs", 0))

        # Stage 2: agent.runs. The deleted ids are captured first — each
        # one is a langGraph thread id stage 3 collects.
        run_ids = await run_metadata.delete_all_for_user(user_id)

        # Stage 3: checkpoint threads. Threads are keyed by run id
        # (single-turn) AND by session id (continuations — Slice 2 binds
        # their graph thread to the session, with no agent.runs row), so
        # the erase loop covers the union, deduped (a single-turn
        # session's id IS its run id — stage 1 returned it as
        # `session_ids`). Without the session half, a continuation's
        # checkpoint content would survive the erasure (review
        # 2026-10-14 compliance fix). This is the only non-SQL step, so
        # a per-thread failure is contained (D3): warn-logged, counted
        # as NOT deleted, remaining ids still attempted. No checkpointer
        # wired (metadata-only dev) → zero threads, not an error.
        # Direct index on purpose (review L1): a missing key must fail
        # loudly (500) — silently falling back to [] would delete the
        # sessions but GC no session-keyed threads and still return 200.
        session_ids = [str(sid) for sid in chat_counts["session_ids"]]
        thread_material = list(dict.fromkeys(run_ids + session_ids))
        # Breadcrumb BEFORE the only non-transactional step (review M3):
        # if the process dies between the row deletes and this GC, the
        # id work-list survives in the logs and the erase is
        # reconstructible (re-run adelete_thread for each id).
        log.info(
            "gdpr erase: user_id=%s rows deleted (sessions=%d messages=%d "
            "prefs=%d runs=%d); GC work-list=%s",
            user_id,
            sessions_deleted,
            messages_deleted,
            prefs_deleted,
            len(run_ids),
            thread_material,
        )
        checkpointer = getattr(request.app.state, "checkpointer", None)
        threads_deleted = 0
        thread_failures = 0
        if checkpointer is not None:
            for thread_id in thread_material:
                try:
                    await checkpointer.adelete_thread(thread_id)
                except Exception as exc:  # noqa: BLE001 — contained per D3
                    thread_failures += 1
                    log.warning(
                        "gdpr erase: adelete_thread failed for thread_id=%s "
                        "(counted as not deleted; remaining ids still "
                        "attempted): %r",
                        thread_id,
                        exc,
                    )
                else:
                    threads_deleted += 1

        counts = {
            "sessions": sessions_deleted,
            "messages": messages_deleted,
            "prefs": prefs_deleted,
            "runs": len(run_ids),
            "threads": threads_deleted,
            # Review L3: the trail must distinguish "all threads gone"
            # from a partial GC months later — the span carried this
            # but the durable evidence did not.
            "thread_failures": thread_failures,
        }
        span.set_attribute("sessions", sessions_deleted)
        span.set_attribute("messages", messages_deleted)
        span.set_attribute("prefs", prefs_deleted)
        span.set_attribute("runs", len(run_ids))
        span.set_attribute("threads", threads_deleted)
        span.set_attribute("thread_failures", thread_failures)

        # Audit LAST — after every deletion stage, so the row reflects
        # reality (including thread failures). The data is already gone
        # when this runs: an audit-store failure is logged loudly but must
        # not turn a served erasure into a 5xx — a 5xx would both misreport
        # the outcome and be misleading to retry, since a retry is
        # idempotent and would only append a zero-count row.
        try:
            await audit.record(user_id, "erase", counts)
        except Exception as exc:  # noqa: BLE001 — see comment above
            log.error(
                "gdpr erase: audit record failed (non-fatal; data already "
                "deleted): user_id=%s counts=%s err=%r",
                user_id,
                counts,
                exc,
            )

        return {"deleted": counts}


# ---------------------------------------------------------------------------
# Request helpers
# ---------------------------------------------------------------------------


def _disabled_response() -> JSONResponse:
    """503 "feature disabled" with the standard error envelope (D6).

    Static strings only: no store access happened, no audit row was
    written, and the body carries nothing subject-derived.
    """
    return JSONResponse(
        status_code=503,
        content={
            "error": "GDPR endpoints disabled",
            "details": (
                "data export and erasure are disabled by the operator "
                "(PYTHON_AGENT_GDPR_ENABLED=false); no data was read or written"
            ),
        },
    )


def _require_user_id(request: Request) -> int:
    """Read the Go-verified user id from `X-Nyx-User-Id`.

    Shared implementation (`api/deps.require_user_id`, review N4
    2026-10-14): the 400 is not an auth check (this hop has no auth —
    the network is the trust boundary) but a loud failure for a
    misconfigured caller, which would otherwise silently export or erase
    user 0's data.
    """
    return _deps_require_user_id(request)


def _chat_store(request: Request) -> ChatStore:
    """The `ChatStore` wired by the lifespan; absent when no DB is configured."""
    store = getattr(request.app.state, "chat_store", None)
    if store is None:
        raise HTTPException(status_code=503, detail="chat store unavailable")
    return store


def _prefs_store(request: Request) -> PrefsStore:
    """The `PrefsStore` wired by the lifespan; absent when no DB is configured."""
    store = getattr(request.app.state, "prefs_store", None)
    if store is None:
        raise HTTPException(status_code=503, detail="prefs store unavailable")
    return store


def _run_metadata(request: Request) -> RunMetadata:
    """The `RunMetadata` store wired by the lifespan; absent without a DB."""
    store = getattr(request.app.state, "run_metadata", None)
    if store is None:
        raise HTTPException(status_code=503, detail="run metadata unavailable")
    return store


def _audit_log(request: Request) -> AuditLog:
    """The `AuditLog` wired by the lifespan; absent without a DB.

    Required (not warn-only) for the *store* to be missing: both
    endpoints SHALL append an audit row, and the DB-branch wiring in
    `main.py` constructs all stores together — a missing audit store
    means the service is misconfigured, so fail loudly rather than serve
    an unaudited export/erase.
    """
    store = getattr(request.app.state, "audit_log", None)
    if store is None:
        raise HTTPException(status_code=503, detail="audit store unavailable")
    return store


def _settings(request: Request) -> Settings:
    """App-scoped settings (set by the lifespan); process singleton as fallback."""
    settings = getattr(request.app.state, "settings", None)
    return settings if settings is not None else get_settings()


# ---------------------------------------------------------------------------
# Serialization — asyncpg objects → JSON-safe primitives
# ---------------------------------------------------------------------------


def _session_json(session: ChatSession) -> dict:
    return {
        "id": str(session.id),
        "user_id": session.user_id,
        "title": session.title,
        "started_at": session.started_at.isoformat(),
        "last_active_at": session.last_active_at.isoformat(),
        "ended_at": session.ended_at.isoformat() if session.ended_at else None,
        # Surfaced verbatim (unlike the sidebar reads, which hide it):
        # the export must show soft-deleted rows AND that they are
        # soft-deleted.
        "deleted_at": session.deleted_at.isoformat() if session.deleted_at else None,
    }


def _message_json(message: MessageRow) -> dict:
    """One exported message — the replay row shape plus `session_id`.

    `session_id` links each message to its conversation inside the dump
    (review 2026-10-14: without it the exported rows were not
    interpretable without cross-referencing the session list). Additive
    on the replay wire too (`MessageRow` carries it now).
    """
    return {
        "id": message.id,
        "session_id": message.session_id,
        "role": message.role,
        "content": message.content,
        "tool_calls": message.tool_calls,  # list | None, never a JSON string
        "tokens": message.tokens,
        "prompt_tokens": message.prompt_tokens,
        "completion_tokens": message.completion_tokens,
        "created_at": message.created_at.isoformat(),
    }


def _pref_json(pref: Pref) -> dict:
    return {
        "key": pref.key,
        "value": pref.value,  # JSON-decoded by the store, never a JSON string
        # No `confidence`: the store keeps one current truth per key and
        # has no confidence column (PrefsStore's docstring) — it would be
        # a constant 1.0, not data.
        "source_session_id": (
            str(pref.source_session_id) if pref.source_session_id else None
        ),
        "updated_at": pref.updated_at.isoformat() if pref.updated_at else None,
    }


def _run_json(run: RunSummary) -> dict:
    return {
        "id": str(run.id),
        "user_id": run.user_id,
        "status": run.status,
        "total_tokens": run.total_tokens,
        "error_code": run.error_code,
        "started_at": run.started_at.isoformat(),
        "ended_at": run.ended_at.isoformat() if run.ended_at else None,
    }
