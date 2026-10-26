"""Heartbeat-during-stream tests (openspec/changes/agent-stale-run-sweeper).

Drives the real `/agent/run` SSE endpoint (FastAPI app + real lifespan +
live Postgres) with a stub LLM whose executor step is slower than the
heartbeat interval, and asserts:

  1. `last_heartbeat_at` is refreshed at least twice during a run that
     outlives the (1 s) interval, and the heartbeat task is gone when the
     stream ends (no pending task leaks).
  2. A failing heartbeat write is non-fatal: no SSE `error` frame, the
     run still completes (`done` + `[DONE]`), and the metadata row still
     lands `completed`.

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""
from __future__ import annotations

import asyncio
import json
import os
import time
from typing import Any, ClassVar

import asyncpg
import httpx
import pytest
from fakeredis import aioredis as fakeaioredis

from agent.types import ChatMessage, ChatRequest
from tests.service_auth_fixtures import SERVICE_AUTH_HEADERS

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

pytestmark = pytest.mark.skipif(
    not _DSN,
    reason="TEST_ASYNC_PG_DSN not set; needs a live Postgres (see Makefile / CI)",
)


class _StubStructuredChain:
    """See test_run.py — chainable shim whose `ainvoke` yields the canned Plan."""

    def __init__(self, plan_payload: dict):
        self._payload = plan_payload

    def __or__(self, other):
        return self

    async def ainvoke(self, messages, config=None):
        from agent.types import Plan

        return Plan.model_validate(self._payload)


class _SlowLlm:
    """Planner returns a 1-step `llm` plan; the executor's llm step sleeps
    `EXECUTOR_DELAY_S` — long enough that the run outlives the 1 s
    heartbeat interval and the loop must tick at least twice."""

    call_count = 0  # class-level; shared across instances
    EXECUTOR_DELAY_S: ClassVar[float] = 1.5
    _PLAN: ClassVar[dict[str, Any]] = {
        "steps": [{"id": "step_1", "kind": "llm", "prompt": "sleep then answer"}]
    }

    def __init__(self, *args, **kwargs):
        pass

    def bind(self, **kwargs):
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)

    async def ainvoke(self, messages, config=None):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        if type(self).call_count == 1:
            return AIMessage(content=json.dumps(self._PLAN))
        await asyncio.sleep(self.EXECUTOR_DELAY_S)
        return AIMessage(content="slow answer")


class _StubRag:
    async def retrieve(self, query):
        from agent.rag_client import RagResult

        return RagResult(context_block="stub context", citations=[])


class _StubTools:
    async def call(self, name, arguments):
        return {"items": [1, 2, 3]}

    async def list_tools(self):
        return [
            {
                "type": "function",
                "function": {
                    "name": "list_feeds",
                    "description": "List user's feeds",
                    "parameters": {"type": "object", "properties": {}},
                },
            }
        ]


async def _drop_schemas() -> None:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
    finally:
        await conn.close()


def _patch_common(monkeypatch):
    import agent.executor as executor_module
    import agent.observability as obs_module
    import agent.planner as planner_module
    import agent.rag_client as rag_module
    import agent.tools_client as tools_module

    _SlowLlm.call_count = 0
    monkeypatch.setattr(planner_module, "ChatOpenAI", _SlowLlm)
    monkeypatch.setattr(executor_module, "ChatOpenAI", _SlowLlm)
    monkeypatch.setattr(rag_module, "RagClient", lambda *a, **kw: _StubRag())
    monkeypatch.setattr(tools_module, "ToolsClient", lambda *a, **kw: _StubTools())

    def _noop_setup(settings):
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider

        provider = TracerProvider()
        trace.set_tracer_provider(provider)
        return trace.get_tracer("nyx-agent-test")

    monkeypatch.setattr(obs_module, "setup_observability", _noop_setup)


def _settings():
    import agent.config as config_module

    return config_module.Settings(
        _env_file=None,  # hermetic: never read the developer's .env
        database_url=_DSN,
        llm_base_url="http://localhost:9999/v1",
        metrics_port=0,
        retention_enabled=False,
        heartbeat_interval_seconds=1,  # short interval; the stub run lasts ~1.5s
    )


def _patch_settings(monkeypatch, settings):
    """Point config AND agent.main at the test's Settings.

    `agent.main` does `from .config import get_settings`, which copies the
    function *reference* at import time. If any earlier test in the same
    process imported agent.main while their own monkeypatch of
    `config.get_settings` was active, main permanently holds that test's
    lambda — patching `config.get_settings` alone does not rebind it.
    Patch main's reference explicitly (importing it here so the attribute
    exists to patch).
    """
    import agent.config as config_module
    import agent.main as main_module

    monkeypatch.setattr(config_module, "_settings", settings)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    monkeypatch.setattr(main_module, "get_settings", lambda: settings)


async def _post_chat(app, body, user_id: int = 42):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test", headers=SERVICE_AUTH_HEADERS
    ) as client:
        return await client.post(
            "/agent/run",
            json=body,
            headers={
                "X-Nyx-User-Id": str(user_id),
                "Authorization": "Bearer stub-token-not-verified",
            },
        )


def _parsed_frames(body: str):
    out = []
    for ln in body.splitlines():
        if not ln.startswith("data: ") or ln == "data: [DONE]":
            continue
        try:
            out.append(json.loads(ln.removeprefix("data: ")))
        except json.JSONDecodeError:
            pass
    return out


async def test_heartbeat_ticks_during_run_and_stops_cleanly(monkeypatch):
    """A run longer than the heartbeat interval refreshes
    `last_heartbeat_at` at least twice (immediate + one periodic touch),
    and no heartbeat task survives the stream."""
    from agent.budget import BudgetGuard
    from agent.main import create_app, lifespan
    from agent.run_metadata import RunMetadata

    _patch_common(monkeypatch)
    await _drop_schemas()
    settings = _settings()
    _patch_settings(monkeypatch, settings)

    calls: list[str] = []
    tick_offsets: list[float] = []
    start: dict[str, float] = {}
    orig_touch = RunMetadata.touch_heartbeat

    async def counting_touch(self, run_id):
        calls.append(run_id)
        if "t0" in start:
            tick_offsets.append(round(time.monotonic() - start["t0"], 2))
        return await orig_touch(self, run_id)

    monkeypatch.setattr(RunMetadata, "touch_heartbeat", counting_touch)

    app = create_app()
    async with lifespan(app):
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=1000000)
        try:
            body = ChatRequest(
                messages=[ChatMessage(role="user", content="hi")]
            ).model_dump()
            start["t0"] = time.monotonic()
            resp = await _post_chat(app, body)
            elapsed = time.monotonic() - start["t0"]
            assert resp.status_code == 200
            text = resp.text
        finally:
            await redis.aclose()

    assert "event: done\n" in text
    assert "data: [DONE]\n\n" in text
    assert "event: error\n" not in text, f"no error frame expected; got {text!r}"

    frames = _parsed_frames(text)
    run_id = frames[0]["run_id"]
    assert len(calls) >= 2, (
        f"heartbeat must tick at least twice in a ~1.5s run with a 1s "
        f"interval; got {len(calls)} ticks at {tick_offsets}s over "
        f"{elapsed:.2f}s (llm_call_count={_SlowLlm.call_count})"
    )
    assert all(rid == run_id for rid in calls)

    leaked = [
        t
        for t in asyncio.all_tasks()
        if t.get_name() == f"agent-heartbeat-{run_id}"
    ]
    assert not leaked, "heartbeat task must be cancelled when the stream ends"

    conn = await asyncpg.connect(dsn=_DSN)
    try:
        row = await conn.fetchrow(
            "SELECT status, ended_at FROM agent.runs WHERE id=$1", run_id
        )
    finally:
        await conn.close()
    assert row is not None
    assert row["status"] == "completed"
    assert row["ended_at"] is not None


async def test_failing_heartbeat_is_non_fatal(monkeypatch):
    """A heartbeat write that raises must not emit an SSE error frame,
    must not stop the run, and must not block the terminal metadata
    write: the stream still ends with `done` + `[DONE]` and the row
    lands `completed`."""
    from agent.budget import BudgetGuard
    from agent.main import create_app, lifespan
    from agent.run_metadata import RunMetadata, RunMetadataUnavailable

    _patch_common(monkeypatch)
    await _drop_schemas()
    settings = _settings()
    _patch_settings(monkeypatch, settings)

    attempts: list[str] = []

    async def broken_touch(self, run_id):
        attempts.append(run_id)
        raise RunMetadataUnavailable("simulated heartbeat failure")

    monkeypatch.setattr(RunMetadata, "touch_heartbeat", broken_touch)

    app = create_app()
    async with lifespan(app):
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=1000000)
        try:
            body = ChatRequest(
                messages=[ChatMessage(role="user", content="hi")]
            ).model_dump()
            resp = await _post_chat(app, body)
            assert resp.status_code == 200
            text = resp.text
        finally:
            await redis.aclose()

    assert attempts, "the heartbeat loop must have attempted at least one touch"
    assert "event: error\n" not in text, (
        f"heartbeat failure must be log-only, never an SSE error; got {text!r}"
    )
    assert "event: done\n" in text
    assert "data: [DONE]\n\n" in text

    frames = _parsed_frames(text)
    run_id = frames[0]["run_id"]

    conn = await asyncpg.connect(dsn=_DSN)
    try:
        row = await conn.fetchrow(
            "SELECT status FROM agent.runs WHERE id=$1", run_id
        )
    finally:
        await conn.close()
    assert row is not None
    assert row["status"] == "completed", (
        "a failing heartbeat must not alter the run's terminal outcome"
    )


async def test_heartbeat_loop_stops_when_row_goes_terminal():
    """The loop's `touched=False` early return (test-review nit, 2026-10-14):
    once the row is terminal, heartbeating stops — the loop returns instead
    of ticking forever against a dead row."""
    from agent.main import _heartbeat_loop

    class _TerminalRM:
        def __init__(self):
            self.calls = 0

        async def touch_heartbeat(self, run_id):
            self.calls += 1
            return False

    rm = _TerminalRM()
    # Returns promptly (well inside the timeout) because the first touch
    # reports terminal.
    await asyncio.wait_for(_heartbeat_loop(rm, "run-1", 1), timeout=0.5)
    assert rm.calls == 1
