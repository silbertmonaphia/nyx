"""Hourly cron that hard-deletes terminal `agent.runs` rows.

For every deleted run the cron also deletes the run's langGraph checkpoint
thread via the checkpointer's `adelete_thread` — langGraph ships no retention
story of its own, so this is the only path that prunes
`langgraph.checkpoint_*` (the stale-run sweeper deliberately preserves
checkpoint data; see `openspec/changes/agent-stale-run-sweeper`). A per-run
GC failure is logged at WARN and does not abort the remaining deletes; the
documented manual fallback is a direct SQL sweep of `langgraph.checkpoint_*`.

Also runs the chat-history retention iteration
(`openspec/changes/agent-chat-history-storage`) on the same tick: a
two-phase soft-delete + grace + hard-delete cycle for
`agent.chat_sessions` (and `agent.chat_messages` via FK ON DELETE
CASCADE). Failure in one iteration is logged at WARN and the other still
runs — the two surfaces have independent observability counters and OTel
spans (`agent.run.retention` + `agent.chat.retention`).

The chat iteration also collects the derived memory of the hard-deleted
sessions (`openspec/changes/agent-user-preferences` D6): after a
successful hard-delete the deleted ids go to
`PrefsStore.delete_prefs_for_sessions`, so a session's preferences die
with the conversation they were extracted from. Prefs cleanup failure is
warn-logged and never aborts the iteration; prefs are NOT counted toward
`agent_chat_retention_deleted_total`. Each hard-deleted session's
langGraph checkpoint thread is GC'd in the same iteration
(`adelete_thread` per id — continuation turns bind their graph thread to
the session id and carry no `agent.runs` row, so the run-retention GC
never sees them; review 2026-10-14 compliance fix). Per-id GC failure is
warn-logged and contained.

The audit iteration (`openspec/changes/agent-gdpr-compliance` D5) runs
last on the same tick: it reaps `agent.data_audit_log` rows past
`PYTHON_AGENT_AUDIT_RETENTION_DAYS` (default 1095 = 3 years). The audit
window deliberately outlives the 1-year data retention — erase/export
evidence stays verifiable after the underlying data would have been
reaped anyway. It runs INDEPENDENTLY of `PYTHON_AGENT_GDPR_ENABLED`: the
kill switch freezes the export/erase endpoints only; an audit trail that
stopped being pruned while the endpoints were disabled would grow
unbounded, and the evidence for erasures performed before the freeze
must still expire on schedule. Failure isolation is the same as the
other two iterations.

OTel span `agent.run.retention` per iteration with `deleted_count` attribute.
OTel span `agent.chat.retention` per iteration with `chat_deleted_count`
attribute. OTel span `agent.audit.retention` per iteration with
`deleted_count` attribute. Prometheus counter
`agent_run_retention_deleted_total` increments by `deleted_count`;
`agent_chat_retention_deleted_total` increments by the chat hard-delete
count; `agent_audit_retention_deleted_total` is store-owned — it lives
in `audit.py` and increments inside `AuditLog.retention_delete`, so the
cron must NOT increment it again (double-count).
"""
from __future__ import annotations

import asyncio
import logging
from typing import TYPE_CHECKING

from opentelemetry import trace
from prometheus_client import Counter

if TYPE_CHECKING:
    from .audit import AuditLog
    from .chat_store import ChatStore
    from .config import Settings
    from .prefs import PrefsStore
    from .run_metadata import RunMetadata

log = logging.getLogger("nyx.agent.retention")
_tracer = trace.get_tracer("nyx.agent")

agent_run_retention_deleted_total = Counter(
    "agent_run_retention_deleted_total",
    "Total number of terminal agent.runs rows deleted by the retention cron.",
)

agent_chat_retention_deleted_total = Counter(
    "agent_chat_retention_deleted_total",
    "Total number of agent.chat_sessions rows hard-deleted by the retention cron. "
    "Soft-deletes are NOT counted (only rows that pass through both the soft-delete "
    "and grace phases and are physically removed).",
)

agent_chat_prefs_deleted_total = Counter(
    "agent_chat_prefs_deleted_total",
    "Total number of agent.chat_user_prefs rows deleted by the retention cron's "
    "orphan sweep (prefs whose source session no longer exists). Increments when a "
    "cleanup failure or a crash between the session delete and the prefs delete is "
    "recovered on a later tick — a nonzero rate is the signal that the hot-path "
    "cleanup is failing.",
)


class RetentionCron:
    """Single asyncio task that fires every `retention_interval_seconds`."""

    def __init__(
        self,
        run_metadata: RunMetadata,
        chat_store: ChatStore | None,
        checkpointer=None,
        *,
        settings: Settings,
        prefs_store: PrefsStore | None = None,
        audit_log: AuditLog | None = None,
    ) -> None:
        self._run_metadata = run_metadata
        # Chat-history retention surface (openspec/changes/agent-chat-history-storage).
        # Required for the chat retention iteration; when None, the chat iteration
        # is skipped (matches the metadata-only dev posture for `checkpointer`).
        self._chat_store = chat_store
        # langGraph `AsyncPostgresSaver` (or None in metadata-only dev, where
        # no checkpoint threads exist). Duck-typed: only `adelete_thread` is
        # used, so tests can inject a fake without importing the framework.
        self._checkpointer = checkpointer
        # Derived-memory cleanup (openspec/changes/agent-user-preferences D6):
        # after the chat hard-delete, the deleted session ids go to
        # `delete_prefs_for_sessions` so a session's prefs do not outlive it.
        # Keyword-only with a None default — pre-existing positional
        # constructions `RetentionCron(rm, chat_store, checkpointer, ...)`
        # keep binding the same parameters, and when None the cleanup is
        # skipped (metadata-only dev; nothing to collect).
        self._prefs_store = prefs_store
        # GDPR audit retention (openspec/changes/agent-gdpr-compliance D5):
        # reaps `agent.data_audit_log` rows past `audit_retention_days`.
        # Keyword-only with a None default, same posture as `prefs_store`:
        # pre-existing positional constructions keep binding the same
        # parameters, and when None the iteration is skipped
        # (metadata-only dev; no audit store wired).
        self._audit_log = audit_log
        # Keyword-only on purpose: `checkpointer` was inserted before
        # `settings`, so a stale positional call `RetentionCron(rm, settings)`
        # would silently bind settings to the checkpointer (test-review
        # 2026-10-14).
        self._settings = settings
        self._task: asyncio.Task[None] | None = None
        self._stop = asyncio.Event()

    def start(self) -> None:
        if self._task is not None:
            return
        self._stop.clear()
        self._task = asyncio.create_task(self._loop(), name="agent-run-retention")
        chat_part = (
            f" + chat (days={self._settings.chat_retention_days}, "
            f"grace={self._settings.chat_retention_grace_days})"
            if self._chat_store is not None and self._settings.chat_retention_enabled
            else ""
        )
        audit_part = (
            f" + audit (days={self._settings.audit_retention_days})"
            if self._audit_log is not None
            else ""
        )
        log.info(
            "retention cron started: every %ds, deletes runs older than %d days%s%s",
            self._settings.retention_interval_seconds,
            self._settings.retention_days,
            chat_part,
            audit_part,
        )

    async def stop(self) -> None:
        if self._task is None:
            return
        self._stop.set()
        try:
            await asyncio.wait_for(self._task, timeout=10.0)
        except TimeoutError:
            log.warning("retention cron did not stop within 10s; cancelling")
            self._task.cancel()
        finally:
            self._task = None

    async def _loop(self) -> None:
        interval = self._settings.retention_interval_seconds
        while not self._stop.is_set():
            try:
                await self._run_once()
            except Exception as exc:  # noqa: BLE001 — never let cron die
                log.warning("retention cron iteration failed: %r", exc)
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=interval)
            except TimeoutError:
                continue

    async def _run_once(self) -> None:
        # Run-retention iteration. Failure of this block is isolated from
        # the chat iteration below — a poison run-retention query can
        # never block chat-history GC.
        try:
            await self._run_iteration()
        except Exception as exc:  # noqa: BLE001 — one iteration never kills the cron
            log.warning("retention: run iteration failed: %r", exc)

        # Chat-retention iteration (openspec/changes/agent-chat-history-storage).
        # Skipped when chat retention is disabled or no ChatStore is wired
        # (metadata-only dev). Failures here are logged at WARN and do not
        # propagate to the next tick.
        if (
            self._chat_store is not None
            and self._settings.chat_retention_enabled
        ):
            try:
                await self._chat_iteration()
            except Exception as exc:  # noqa: BLE001
                log.warning("retention: chat iteration failed: %r", exc)

        # Audit-retention iteration (openspec/changes/agent-gdpr-compliance
        # D5). Skipped only when no AuditLog is wired (metadata-only dev).
        # Deliberately INDEPENDENT of `gdpr_enabled`: the kill switch
        # freezes the export/erase endpoints, not the audit log's own
        # retention — the trail must keep expiring on schedule either way
        # (spec "GDPR kill switch disturbs nothing at startup"). Same
        # failure isolation as the iterations above.
        if self._audit_log is not None:
            try:
                await self._audit_iteration()
            except Exception as exc:  # noqa: BLE001
                log.warning("retention: audit iteration failed: %r", exc)

    async def _run_iteration(self) -> None:
        with _tracer.start_as_current_span("agent.run.retention") as span:
            deleted_ids = await self._run_metadata.retention_delete_terminal(
                retention_days=self._settings.retention_days
            )
            deleted = len(deleted_ids)
            span.set_attribute("deleted_count", deleted)
            agent_run_retention_deleted_total.inc(deleted)

            # GC each deleted run's langGraph checkpoint thread. Failure for
            # one run is logged (non-fatal): the metadata deletion stands and
            # the remaining ids are still processed.
            if self._checkpointer is not None:
                for run_id in deleted_ids:
                    try:
                        await self._checkpointer.adelete_thread(run_id)
                    except Exception as exc:  # noqa: BLE001
                        log.warning(
                            "retention: adelete_thread failed for run_id=%s "
                            "(non-fatal): %r",
                            run_id,
                            exc,
                        )

            if deleted:
                log.info(
                    "retention cron: deleted %d terminal runs older than %d days",
                    deleted,
                    self._settings.retention_days,
                )

    async def _chat_iteration(self) -> None:
        """Two-phase chat retention: soft-delete expired, then hard-delete the grace-expired.

        Opens an OTel span `agent.chat.retention` carrying `chat_deleted_count`
        for the hard-delete phase. Soft-deletes are NOT counted toward the
        counter — only rows that pass through both phases and are physically
        removed increment `agent_chat_retention_deleted_total`. The soft-delete
        pass itself can fail without aborting the hard-delete pass (separate
        try/except inside the span), matching the run-iteration's per-id
        error isolation.
        """
        with _tracer.start_as_current_span("agent.chat.retention") as span:
            # Phase 1: soft-delete. Soft-deletes are not counted.
            try:
                soft_ids = await self._chat_store.soft_delete_expired(
                    retention_days=self._settings.chat_retention_days
                )
            except Exception as exc:  # noqa: BLE001
                log.warning(
                    "retention: chat soft-delete failed (non-fatal): %r", exc
                )
                soft_ids = []
            span.set_attribute("soft_deleted_count", len(soft_ids))

            # Phase 2: hard-delete. Only the rows that survive both phases
            # count toward the metric.
            try:
                hard_deleted_ids = await self._chat_store.hard_delete_soft_deleted(
                    grace_days=self._settings.chat_retention_grace_days
                )
            except Exception as exc:  # noqa: BLE001
                log.warning(
                    "retention: chat hard-delete failed (non-fatal): %r", exc
                )
                hard_deleted_ids = []
            hard_deleted = len(hard_deleted_ids)
            span.set_attribute("chat_deleted_count", hard_deleted)
            agent_chat_retention_deleted_total.inc(hard_deleted)

            if hard_deleted:
                log.info(
                    "retention cron: hard-deleted %d chat sessions past the "
                    "%d-day grace window",
                    hard_deleted,
                    self._settings.chat_retention_grace_days,
                )

            # Derived-memory cleanup (openspec/changes/agent-user-preferences
            # D6): a hard-deleted session takes its extracted preferences
            # with it — derived memory must not outlive its conversation.
            # Runs only when the hard-delete succeeded (a failed delete
            # returns no ids, so prefs stay intact for the retry next tick).
            # Failure isolation: a cleanup fault is warn-logged and never
            # aborts the iteration; prefs are NOT counted as sessions
            # (the metric above took `len(ids)` already).
            if hard_deleted_ids and self._prefs_store is not None:
                try:
                    prefs_deleted = await self._prefs_store.delete_prefs_for_sessions(
                        hard_deleted_ids
                    )
                except Exception as exc:  # noqa: BLE001
                    log.warning(
                        "retention: prefs cleanup failed (non-fatal): %r", exc
                    )
                else:
                    if prefs_deleted:
                        log.info(
                            "retention cron: deleted %d preference rows sourced "
                            "from %d hard-deleted chat sessions",
                            prefs_deleted,
                            hard_deleted,
                        )

            # Orphan sweep backstop (review 2026-10-14 / H1): the cleanup
            # above is a separate transaction on a separate connection —
            # a transient failure or a crash between the two deletes
            # would strand those prefs forever (their session ids can
            # never be matched again). This sweep runs every tick, so
            # stranded rows are collected on the next pass, and the
            # counter makes the gap observable instead of silent. NULL-
            # sourced prefs are deliberately kept (slice-4 erasure path).
            if self._prefs_store is not None:
                try:
                    orphaned = await self._prefs_store.delete_orphaned()
                except Exception as exc:  # noqa: BLE001
                    log.warning(
                        "retention: prefs orphan sweep failed (non-fatal): %r",
                        exc,
                    )
                else:
                    span.set_attribute("chat_prefs_orphans_deleted", orphaned)
                    agent_chat_prefs_deleted_total.inc(orphaned)
                    if orphaned:
                        log.info(
                            "retention cron: orphan sweep deleted %d preference "
                            "rows whose source sessions no longer exist",
                            orphaned,
                        )

            # langGraph checkpoint GC for session-keyed threads (review
            # 2026-10-14 compliance fix): continuation turns bind their
            # graph thread to the SESSION id with no agent.runs row, so
            # the run-retention GC above never sees them — without this,
            # a hard-deleted session's checkpoint content would live
            # forever. Single-turn sessions are no-ops here (their thread
            # was already collected with the run row; adelete_thread on an
            # absent thread is harmless). Per-id failure is contained,
            # mirroring the run iteration's GC.
            if hard_deleted_ids and self._checkpointer is not None:
                for session_id in hard_deleted_ids:
                    try:
                        await self._checkpointer.adelete_thread(session_id)
                    except Exception as exc:  # noqa: BLE001
                        log.warning(
                            "retention: adelete_thread failed for session=%s "
                            "(non-fatal): %r",
                            session_id,
                            exc,
                        )

    async def _audit_iteration(self) -> None:
        """Reap `agent.data_audit_log` rows past the audit retention window.

        Opens an OTel span `agent.audit.retention` carrying `deleted_count`.
        The counter (`agent_audit_retention_deleted_total`) is store-owned:
        `AuditLog.retention_delete` increments it, so this iteration only
        reports (span + log) and must NOT increment it again. The 3-year
        default window (D5) outlives the 1-year data retention on purpose:
        erase/export evidence must stay verifiable after the underlying
        data would have been reaped anyway. The caller (`_run_once`) runs
        this independently of `gdpr_enabled` — see the comment there.
        """
        with _tracer.start_as_current_span("agent.audit.retention") as span:
            deleted = await self._audit_log.retention_delete(
                self._settings.audit_retention_days
            )
            span.set_attribute("deleted_count", deleted)
            if deleted:
                log.info(
                    "retention cron: deleted %d audit rows older than %d days",
                    deleted,
                    self._settings.audit_retention_days,
                )
