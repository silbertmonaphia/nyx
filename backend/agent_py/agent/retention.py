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

OTel span `agent.run.retention` per iteration with `deleted_count` attribute.
OTel span `agent.chat.retention` per iteration with `chat_deleted_count`
attribute. Prometheus counter `agent_run_retention_deleted_total` increments
by `deleted_count`; `agent_chat_retention_deleted_total` increments by the
chat hard-delete count.
"""
from __future__ import annotations

import asyncio
import logging
from typing import TYPE_CHECKING

from opentelemetry import trace
from prometheus_client import Counter

if TYPE_CHECKING:
    from .chat_store import ChatStore
    from .config import Settings
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


class RetentionCron:
    """Single asyncio task that fires every `retention_interval_seconds`."""

    def __init__(
        self,
        run_metadata: RunMetadata,
        chat_store: ChatStore | None,
        checkpointer=None,
        *,
        settings: Settings,
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
        log.info(
            "retention cron started: every %ds, deletes runs older than %d days%s",
            self._settings.retention_interval_seconds,
            self._settings.retention_days,
            chat_part,
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
                hard_deleted = await self._chat_store.hard_delete_soft_deleted(
                    grace_days=self._settings.chat_retention_grace_days
                )
            except Exception as exc:  # noqa: BLE001
                log.warning(
                    "retention: chat hard-delete failed (non-fatal): %r", exc
                )
                hard_deleted = 0
            span.set_attribute("chat_deleted_count", hard_deleted)
            agent_chat_retention_deleted_total.inc(hard_deleted)

            if hard_deleted:
                log.info(
                    "retention cron: hard-deleted %d chat sessions past the "
                    "%d-day grace window",
                    hard_deleted,
                    self._settings.chat_retention_grace_days,
                )
