"""Stale-run sweeper — reaps zombie `running` rows.

A crashed or killed agent process leaves its `agent.runs` row stuck in
`running`, indistinguishable from a live run (openspec/changes/
agent-stale-run-sweeper). This cron polls every
`PYTHON_AGENT_SWEEP_INTERVAL_SECONDS` and transitions rows whose
`last_heartbeat_at` is older than `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS`
to `status='failed'`, `error_code='stale_run'`, `ended_at=now()`.

The sweeper deliberately does NOT delete langGraph checkpoint data: a reaped
run's checkpoint thread is collected by the retention cron when it later
deletes the terminal row, so a crashed run's state survives the same
retention window as any other run.

OTel span `agent.run.sweep` per iteration with `swept_count` attribute.
Prometheus counter `agent_runs_swept_total` increments by `swept_count`.
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

log = logging.getLogger("nyx.agent.sweeper")
_tracer = trace.get_tracer("nyx.agent")

agent_runs_swept_total = Counter(
    "agent_runs_swept_total",
    "Total number of stale `running` agent.runs rows reaped by the sweeper cron.",
)


class StaleRunSweeper:
    """Single asyncio task that fires every `sweep_interval_seconds`."""

    def __init__(self, run_metadata: RunMetadata, settings: Settings) -> None:
        self._run_metadata = run_metadata
        self._settings = settings
        self._task: asyncio.Task[None] | None = None
        self._stop = asyncio.Event()

    def start(self) -> None:
        if self._task is not None:
            return
        self._stop.clear()
        self._task = asyncio.create_task(self._loop(), name="agent-stale-run-sweeper")
        log.info(
            "stale-run sweeper started: every %ds, reaps runs with a heartbeat "
            "older than %ds",
            self._settings.sweep_interval_seconds,
            self._settings.stale_run_timeout_seconds,
        )

    async def stop(self) -> None:
        if self._task is None:
            return
        self._stop.set()
        try:
            await asyncio.wait_for(self._task, timeout=10.0)
        except TimeoutError:
            log.warning("stale-run sweeper did not stop within 10s; cancelling")
            self._task.cancel()
        finally:
            self._task = None

    async def _loop(self) -> None:
        interval = self._settings.sweep_interval_seconds
        while not self._stop.is_set():
            try:
                await self._run_once()
            except Exception as exc:  # noqa: BLE001 — never let cron die
                log.warning("stale-run sweeper iteration failed: %r", exc)
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=interval)
            except TimeoutError:
                continue

    async def _run_once(self) -> None:
        with _tracer.start_as_current_span("agent.run.sweep") as span:
            reaped = await self._run_metadata.sweep_stale_runs(
                timeout_seconds=self._settings.stale_run_timeout_seconds
            )
            span.set_attribute("swept_count", len(reaped))
            agent_runs_swept_total.inc(len(reaped))
            if reaped:
                log.info(
                    "stale-run sweeper: reaped %d zombie run(s): %s",
                    len(reaped),
                    ", ".join(reaped),
                )
