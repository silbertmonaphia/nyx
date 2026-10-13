"""Hourly cron that hard-deletes terminal `agent.runs` rows.

For every deleted run the cron also deletes the run's langGraph checkpoint
thread via the checkpointer's `adelete_thread` — langGraph ships no retention
story of its own, so this is the only path that prunes
`langgraph.checkpoint_*` (the stale-run sweeper deliberately preserves
checkpoint data; see `openspec/changes/agent-stale-run-sweeper`). A per-run
GC failure is logged at WARN and does not abort the remaining deletes; the
documented manual fallback is a direct SQL sweep of `langgraph.checkpoint_*`.

OTel span `agent.run.retention` per iteration with `deleted_count` attribute.
Prometheus counter `agent_run_retention_deleted_total` increments by `deleted_count`.
"""
from __future__ import annotations

import asyncio
import logging
from typing import TYPE_CHECKING

from opentelemetry import trace
from prometheus_client import Counter

if TYPE_CHECKING:
    from .config import Settings
    from .run_metadata import RunMetadata

log = logging.getLogger("nyx.agent.retention")
_tracer = trace.get_tracer("nyx.agent")

agent_run_retention_deleted_total = Counter(
    "agent_run_retention_deleted_total",
    "Total number of terminal agent.runs rows deleted by the retention cron.",
)


class RetentionCron:
    """Single asyncio task that fires every `retention_interval_seconds`."""

    def __init__(
        self,
        run_metadata: RunMetadata,
        checkpointer=None,
        *,
        settings: Settings,
    ) -> None:
        self._run_metadata = run_metadata
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
        log.info(
            "retention cron started: every %ds, deletes runs older than %d days",
            self._settings.retention_interval_seconds,
            self._settings.retention_days,
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
