"""Hourly cron that hard-deletes terminal `agent.runs` rows.

Targets the `agent.runs` metadata table only — langGraph's checkpoint tables
are pruned via langGraph's own retention story and are NOT touched here.

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

    def __init__(self, run_metadata: "RunMetadata", settings: "Settings") -> None:
        self._run_metadata = run_metadata
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
        except asyncio.TimeoutError:
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
            except asyncio.TimeoutError:
                continue

    async def _run_once(self) -> None:
        with _tracer.start_as_current_span("agent.run.retention") as span:
            deleted = await self._run_metadata.retention_delete_terminal(
                retention_days=self._settings.retention_days
            )
            span.set_attribute("deleted_count", deleted)
            agent_run_retention_deleted_total.inc(deleted)
            if deleted:
                log.info(
                    "retention cron: deleted %d terminal runs older than %d days",
                    deleted,
                    self._settings.retention_days,
                )
