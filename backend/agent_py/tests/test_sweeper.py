"""Stale-run sweeper tests (openspec/changes/agent-stale-run-sweeper).

Covers:

  1. `StaleRunSweeper._run_once` — reaps via `RunMetadata.sweep_stale_runs`,
     increments `agent_runs_swept_total` by the reaped count, leaves the
     counter unchanged on a zero-reap iteration.
  2. `StaleRunSweeper._loop` — a failed iteration is logged and the next
     interval still runs (never-die posture, mirrors the retention cron).
  3. The checkpoint-GC policy split, against a REAL langGraph checkpoint
     thread: a reaped run's `langgraph.checkpoint_*` rows are preserved;
     the retention cron then deletes them together with the metadata row.
  4. Retention GC failure containment — one failing `adelete_thread` does
     not abort the remaining deletes.

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""
from __future__ import annotations

import asyncio
import json
import os
import uuid
from typing import Any, ClassVar

import asyncpg
import httpx
import pytest
from fakeredis import aioredis as fakeaioredis

from agent.budget import BudgetGuard
from agent.sweeper import StaleRunSweeper, agent_runs_swept_total
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


class _PlanLlm:
    """Planner → 1-step llm plan; executor → text. Enough to write real
    langGraph checkpoints via `graph.astream`."""

    call_count = 0
    _PLAN: ClassVar[dict[str, Any]] = {
        "steps": [{"id": "step_1", "kind": "llm", "prompt": "answer"}]
    }

    def __init__(self, *args, **kwargs):
        pass

    def bind(self, **kwargs):
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)

    async def ainvoke(self, messages, config=None):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        return AIMessage(content="ok")


class _GatedLlm:
    """Planner → 1-step llm plan; the executor's llm call parks on a gate so
    the test can cancel mid-run (the crash trick from test_run_resume).
    Resumed / subsequent calls return text immediately."""

    call_count = 0
    bind_count = 0  # planner invocations — pinned across resumes
    gate: asyncio.Event | None = None
    _PLAN: ClassVar[dict[str, Any]] = {
        "steps": [{"id": "step_1", "kind": "llm", "prompt": "answer"}]
    }

    def __init__(self, *args, **kwargs):
        pass

    def bind(self, **kwargs):
        type(self).bind_count += 1
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)

    async def ainvoke(self, messages, config=None):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        if type(self).call_count == 2 and type(self).gate is not None:
            await type(self).gate.wait()
        return AIMessage(content="ok")


class _StubBudget:
    class _D:
        decision = "ok"

    async def check_and_increment(self, user_id, tokens, tracer):
        return self._D()


class _StubRag:
    async def retrieve(self, query):
        from agent.rag_client import RagResult

        return RagResult(context_block="", citations=[])


class _StubTools:
    call_count = 0  # dispatch count — pinned across resumes

    async def call(self, name, arguments):
        type(self).call_count += 1
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


class _FlakySaver:
    """Fake checkpointer: first adelete_thread raises, rest succeed."""

    def __init__(self):
        self.attempted: list[str] = []

    async def adelete_thread(self, thread_id: str) -> None:
        self.attempted.append(thread_id)
        if len(self.attempted) == 1:
            raise RuntimeError("simulated saver failure")


async def _drop_schemas() -> None:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
    finally:
        await conn.close()


async def _count_thread_rows(thread_id: str) -> dict[str, int]:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        out = {}
        for table in ("checkpoints", "checkpoint_blobs", "checkpoint_writes"):
            out[table] = await conn.fetchval(
                f"SELECT count(*) FROM langgraph.{table} WHERE thread_id=$1",
                thread_id,
            )
        return out
    finally:
        await conn.close()


@pytest.fixture
async def app_ctx(monkeypatch):
    """Live app: real lifespan, real DB, migrations applied, checkpointer up.

    Crons are disabled in settings — tests drive `_run_once` explicitly so
    timing never depends on a background loop.
    """
    await _drop_schemas()

    import agent.config as config_module

    settings = config_module.Settings(
        _env_file=None,  # hermetic: never read the developer's .env
        database_url=_DSN,
        llm_base_url="http://localhost:9999/v1",
        metrics_port=0,
        retention_enabled=False,
        sweep_enabled=False,
    )
    monkeypatch.setattr(config_module, "_settings", settings)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    import agent.main as main_module

    monkeypatch.setattr(main_module, "get_settings", lambda: settings)

    from agent.main import create_app, lifespan

    app = create_app()
    async with lifespan(app):
        # Route budget checks through fakeredis: deterministic, and keeps the
        # operator's dev Redis untouched (test-review nit, 2026-10-14).
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=1_000_000)
        try:
            yield app
        finally:
            await redis.aclose()


def _patch_llm(monkeypatch, llm_cls=_PlanLlm):
    import agent.executor as executor_module
    import agent.observability as obs_module
    import agent.planner as planner_module

    llm_cls.call_count = 0
    if hasattr(llm_cls, "bind_count"):
        llm_cls.bind_count = 0
    _StubTools.call_count = 0
    monkeypatch.setattr(planner_module, "ChatOpenAI", llm_cls)
    monkeypatch.setattr(executor_module, "ChatOpenAI", llm_cls)

    def _noop_setup(settings):
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider

        provider = TracerProvider()
        trace.set_tracer_provider(provider)
        return trace.get_tracer("nyx-agent-test")

    monkeypatch.setattr(obs_module, "setup_observability", _noop_setup)


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


async def _drain(graph, state, config):
    async for _ in graph.astream(state, config=config, stream_mode="values"):
        pass


async def _crash_mid_run(app, thread_id: str) -> None:
    """Drive a graph until the executor's llm call is in flight, then cancel —
    leaving a mid-plan checkpoint thread, exactly like a killed process."""
    from agent.graph import build_graph
    from agent.state import new_state

    graph = build_graph(
        settings=app.state.settings,
        budget=_StubBudget(),
        rag=_StubRag(),
        tools=_StubTools(),
        checkpointer=app.state.checkpointer,
    )
    config = {"configurable": {"thread_id": thread_id}}
    state = new_state(
        user_id=1,
        run_id=thread_id,
        messages=[ChatMessage(role="user", content="hi")],
        model="stub-model",
    )

    _GatedLlm.gate = asyncio.Event()
    task = asyncio.create_task(_drain(graph, state, config))
    for _ in range(200):
        if _GatedLlm.call_count == 2:
            break
        await asyncio.sleep(0.01)
    assert _GatedLlm.call_count == 2, (
        "executor llm must be in flight before the simulated crash"
    )
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    _GatedLlm.gate = None


async def _write_checkpoint_thread(app, thread_id: str) -> None:
    """Drive a real graph run so langGraph writes checkpoints for `thread_id`."""
    from agent.graph import build_graph
    from agent.state import new_state
    from agent.types import ChatMessage

    graph = build_graph(
        settings=app.state.settings,
        budget=_StubBudget(),
        rag=_StubRag(),
        tools=_StubTools(),
        checkpointer=app.state.checkpointer,
    )
    config = {"configurable": {"thread_id": thread_id}}
    state = new_state(
        user_id=1,
        run_id=thread_id,
        messages=[ChatMessage(role="user", content="hi")],
        model="stub-model",
    )
    async for _ in graph.astream(state, config=config, stream_mode="values"):
        pass


# ---- _run_once reaps + counts -------------------------------------------


async def test_sweeper_run_once_reaps_and_counts(app_ctx):
    """A stale `running` row is reaped by one `_run_once`; the counter
    increases by 1; a second (zero-reap) iteration leaves it unchanged
    (spec `agent-run-sweeper` — "Reaped rows are counted" / "Zero-reap
    iterations are visible")."""
    rm = app_ctx.state.run_metadata
    settings = app_ctx.state.settings.model_copy(
        update={"stale_run_timeout_seconds": 30}
    )
    sweeper = StaleRunSweeper(rm, settings)

    stale_id = str(uuid.uuid4())
    async with rm._pool.acquire() as conn:
        await conn.execute("DELETE FROM runs")
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, last_heartbeat_at) "
            "VALUES ($1, 1, 'running', now() - interval '60 seconds')",
            stale_id,
        )

    before = agent_runs_swept_total._value.get()
    await sweeper._run_once()
    after = agent_runs_swept_total._value.get()
    assert after == before + 1, (
        f"counter must increase by the reaped count; before={before} after={after}"
    )

    async with rm._pool.acquire() as conn:
        row = await conn.fetchrow(
            "SELECT status, error_code FROM runs WHERE id=$1", stale_id
        )
    assert row["status"] == "failed"
    assert row["error_code"] == "stale_run"

    # Zero-reap iteration: span still emitted, counter unchanged.
    await sweeper._run_once()
    assert agent_runs_swept_total._value.get() == after


# ---- _loop never dies ----------------------------------------------------


class _FlakyRM:
    def __init__(self):
        self.calls = 0

    async def sweep_stale_runs(self, timeout_seconds):
        self.calls += 1
        if self.calls == 1:
            raise RuntimeError("simulated db blip")
        return []


async def test_sweeper_loop_survives_failed_iteration(monkeypatch):
    """A failing iteration is logged and the next interval still runs
    (spec `agent-run-sweeper` — "Sweep iteration failure is contained")."""
    import agent.config as config_module

    settings = config_module.Settings(
        _env_file=None,  # hermetic: never read the developer's .env
        llm_base_url="http://vllm:8000/v1",
        metrics_port=0,
        sweep_interval_seconds=1,
        stale_run_timeout_seconds=60,
    )
    flaky = _FlakyRM()
    sweeper = StaleRunSweeper(flaky, settings)
    sweeper.start()
    try:
        deadline = asyncio.get_event_loop().time() + 3.0
        while flaky.calls < 2:
            assert asyncio.get_event_loop().time() < deadline, (
                f"sweeper loop stopped after a failed iteration; calls={flaky.calls}"
            )
            await asyncio.sleep(0.05)
    finally:
        await sweeper.stop()
    assert flaky.calls >= 2


# ---- lifespan wiring ------------------------------------------------------


async def test_lifespan_wires_sweeper_when_enabled(monkeypatch):
    """Spec contract: with a DB configured and `sweep_enabled`, the lifespan
    starts the sweeper task and stops it cleanly on shutdown."""
    await _drop_schemas()

    import agent.config as config_module

    settings = config_module.Settings(
        _env_file=None,  # hermetic: never read the developer's .env
        database_url=_DSN,
        llm_base_url="http://localhost:9999/v1",
        metrics_port=0,
        retention_enabled=False,
        sweep_enabled=True,
        sweep_interval_seconds=60,
        stale_run_timeout_seconds=180,
    )
    monkeypatch.setattr(config_module, "_settings", settings)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    import agent.main as main_module

    monkeypatch.setattr(main_module, "get_settings", lambda: settings)

    from agent.main import create_app, lifespan

    app = create_app()
    async with lifespan(app):
        sweeper = app.state.sweeper
        assert sweeper is not None, "sweeper must be wired when sweep_enabled"
        names = {t.get_name() for t in asyncio.all_tasks()}
        assert "agent-stale-run-sweeper" in names, (
            f"sweeper task must be running; live tasks: {sorted(names)}"
        )
    assert sweeper._task is None, "sweeper must stop cleanly on shutdown"


async def test_lifespan_omits_sweeper_when_disabled(app_ctx):
    """`sweep_enabled=False` means no sweeper task at all (heartbeats are
    unaffected — they live in the stream generator, not the cron)."""
    assert app_ctx.state.sweeper is None


# ---- checkpoint-GC policy split (real langGraph thread) ------------------


async def test_reap_preserves_checkpoints_and_retention_deletes_them(
    app_ctx, monkeypatch
):
    """Spec contract: reap MUST preserve the run's `langgraph.checkpoint_*`
    rows; the retention cron then deletes them together with the row
    (spec `agent-run-sweeper` — "Reap preserves the checkpoint thread" /
    "Reaped rows are collected at retention like any other terminal run";
    spec `agent-run-store-py` — "Retention deletes the deleted runs'
    checkpoint threads")."""
    _patch_llm(monkeypatch)

    rm = app_ctx.state.run_metadata
    thread_id = str(uuid.uuid4())
    await _write_checkpoint_thread(app_ctx, thread_id)

    counts = await _count_thread_rows(thread_id)
    assert counts["checkpoints"] > 0, (
        "test setup must produce real checkpoints for the thread"
    )

    # The run's metadata row: zombie (stale heartbeat).
    async with rm._pool.acquire() as conn:
        await conn.execute("DELETE FROM runs")
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, last_heartbeat_at) "
            "VALUES ($1, 1, 'running', now() - interval '60 seconds')",
            thread_id,
        )

    # Reap: metadata flips, checkpoints stay.
    settings = app_ctx.state.settings.model_copy(
        update={"stale_run_timeout_seconds": 30}
    )
    reaped = await rm.sweep_stale_runs(settings.stale_run_timeout_seconds)
    assert reaped == [thread_id]

    counts = await _count_thread_rows(thread_id)
    assert counts["checkpoints"] > 0, "reap MUST NOT delete checkpoint data"

    # Age the reaped row past the retention window; run one retention pass
    # with the app's REAL checkpointer.
    async with rm._pool.acquire() as conn:
        await conn.execute(
            "UPDATE runs SET ended_at = now() - interval '60 days' WHERE id=$1",
            thread_id,
        )

    from agent.retention import RetentionCron

    cron_settings = app_ctx.state.settings.model_copy(update={"retention_days": 30})
    # `chat_store=None` (chat retention is exercised in its own test
    # suite; this suite focuses on run-state retention GC).
    cron = RetentionCron(rm, None, app_ctx.state.checkpointer, settings=cron_settings)
    await cron._run_once()

    async with rm._pool.acquire() as conn:
        gone = await conn.fetchval("SELECT count(*) FROM runs WHERE id=$1", thread_id)
    assert gone == 0

    counts = await _count_thread_rows(thread_id)
    assert counts == {"checkpoints": 0, "checkpoint_blobs": 0, "checkpoint_writes": 0}, (
        f"retention must GC every checkpoint table for the thread; got {counts}"
    )


# ---- resume-window semantics at the endpoint -----------------------------


async def test_resume_inside_window_still_resumes(app_ctx, monkeypatch):
    """Spec `agent-run-sweeper` — "Resume inside the window still
    rehydrates": a crashed run's `running` row (stale heartbeat, not yet
    reaped) can be resumed by echoing its run_id — the endpoint reuses the
    id and emits note{kind:"resumed"}."""
    _patch_llm(monkeypatch, _GatedLlm)

    rm = app_ctx.state.run_metadata
    thread_id = str(uuid.uuid4())
    await _crash_mid_run(app_ctx, thread_id)

    async with rm._pool.acquire() as conn:
        await conn.execute("DELETE FROM runs")
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, last_heartbeat_at) "
            "VALUES ($1, 1, 'running', now() - interval '60 seconds')",
            thread_id,
        )

    binds_before = _GatedLlm.bind_count
    tools_before = _StubTools.call_count
    llm_before = _GatedLlm.call_count

    body = ChatRequest(
        messages=[ChatMessage(role="user", content="hi")], run_id=thread_id
    ).model_dump()
    resp = await _post_chat(app_ctx, body, user_id=1)
    assert resp.status_code == 200, resp.text
    frames = _parsed_frames(resp.text)
    kinds = [f.get("kind") for f in frames]
    assert frames[0]["run_id"] == thread_id, "the echoed run_id must be reused"
    assert "resumed" in kinds, f"expected note{{kind:'resumed'}}; kinds={kinds}"
    assert "fresh_run" not in kinds

    # Regression pin for the graph.py pure-resume fix (test-review
    # 2026-10-14): reverting `stream_input = None` re-runs the planner, and
    # these deltas are the only assertions that fail if it does.
    assert _GatedLlm.bind_count == binds_before, (
        "the planner MUST NOT re-run on an endpoint resume"
    )
    assert _StubTools.call_count == tools_before, (
        "completed step_1 MUST be skipped on resume"
    )
    assert _GatedLlm.call_count == llm_before + 1, (
        "exactly the one remaining step (step_2's llm) may run on resume"
    )


async def test_resume_after_reap_starts_fresh(app_ctx, monkeypatch):
    """Spec `agent-run-sweeper` — "Resume after a reap is treated as
    terminal": the same echo after the sweeper flipped the row yields a
    fresh run (different run_id + note{kind:"fresh_run"}), and the reaped
    thread's checkpoint data is not touched."""
    _patch_llm(monkeypatch, _GatedLlm)

    rm = app_ctx.state.run_metadata
    thread_id = str(uuid.uuid4())
    await _crash_mid_run(app_ctx, thread_id)

    async with rm._pool.acquire() as conn:
        await conn.execute("DELETE FROM runs")
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, last_heartbeat_at) "
            "VALUES ($1, 1, 'running', now() - interval '60 seconds')",
            thread_id,
        )

    reaped = await rm.sweep_stale_runs(timeout_seconds=30)
    assert reaped == [thread_id]
    before = await _count_thread_rows(thread_id)
    assert before["checkpoints"] > 0

    body = ChatRequest(
        messages=[ChatMessage(role="user", content="hi")], run_id=thread_id
    ).model_dump()
    resp = await _post_chat(app_ctx, body, user_id=1)
    assert resp.status_code == 200, resp.text
    frames = _parsed_frames(resp.text)
    kinds = [f.get("kind") for f in frames]
    assert frames[0]["run_id"] != thread_id, (
        "a reaped run must mint a fresh run_id"
    )
    assert "fresh_run" in kinds, f"expected note{{kind:'fresh_run'}}; kinds={kinds}"
    assert "resumed" not in kinds

    # The dead thread's checkpoints are untouched by the fresh run.
    after = await _count_thread_rows(thread_id)
    assert after == before


async def test_resume_of_graph_at_end_completes_without_rerunning(
    app_ctx, monkeypatch
):
    """Pins the already-END fallback in `run_agent_stream` (test-review nit):
    a row left `running` after the graph reached END (crash between the last
    node and the terminal write) resumes via `astream(None)`, replays the
    final snapshot, and completes — no planner or step re-execution."""
    _patch_llm(monkeypatch, _GatedLlm)

    rm = app_ctx.state.run_metadata
    thread_id = str(uuid.uuid4())
    await _write_checkpoint_thread(app_ctx, thread_id)  # driven to END

    async with rm._pool.acquire() as conn:
        await conn.execute("DELETE FROM runs")
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, last_heartbeat_at) "
            "VALUES ($1, 1, 'running', now() - interval '60 seconds')",
            thread_id,
        )

    binds_before = _GatedLlm.bind_count
    llm_before = _GatedLlm.call_count
    body = ChatRequest(
        messages=[ChatMessage(role="user", content="hi")], run_id=thread_id
    ).model_dump()
    resp = await _post_chat(app_ctx, body, user_id=1)
    assert resp.status_code == 200, resp.text
    assert "event: done\n" in resp.text
    assert "data: [DONE]\n\n" in resp.text
    assert _GatedLlm.bind_count == binds_before, "planner must not re-run at END"
    assert _GatedLlm.call_count == llm_before, "no step may re-execute at END"

    async with rm._pool.acquire() as conn:
        row = await conn.fetchrow("SELECT status FROM runs WHERE id=$1", thread_id)
    assert row["status"] == "completed", (
        "the resumed stream must close the row even though the graph was done"
    )


async def test_resume_with_no_checkpoint_falls_back_to_fresh_state(
    app_ctx, monkeypatch
):
    """Pins the no-checkpoint fallback: a `running` row whose thread never
    got a checkpoint (crash before the planner finished) resumes with the
    fresh state — the planner runs, the stream completes, and the row is
    marked `completed` rather than hanging in `running`."""
    _patch_llm(monkeypatch, _GatedLlm)

    rm = app_ctx.state.run_metadata
    thread_id = str(uuid.uuid4())  # no checkpoint rows for this thread
    async with rm._pool.acquire() as conn:
        await conn.execute("DELETE FROM runs")
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, last_heartbeat_at) "
            "VALUES ($1, 1, 'running', now() - interval '60 seconds')",
            thread_id,
        )

    binds_before = _GatedLlm.bind_count
    body = ChatRequest(
        messages=[ChatMessage(role="user", content="hi")], run_id=thread_id
    ).model_dump()
    resp = await _post_chat(app_ctx, body, user_id=1)
    assert resp.status_code == 200, resp.text
    assert "event: done\n" in resp.text
    assert _GatedLlm.bind_count == binds_before + 1, (
        "with no checkpoint the planner must run on the fresh fallback state"
    )

    async with rm._pool.acquire() as conn:
        row = await conn.fetchrow("SELECT status FROM runs WHERE id=$1", thread_id)
    assert row["status"] == "completed"


async def test_disconnect_before_done_leaves_row_running(app_ctx, monkeypatch):
    """Pins the disconnect gate (test-review round 2, 2026-10-14): a stream
    closed before `done` must NOT mark the row `completed` — it stays
    `running`, resumable inside the window (spec `agent-run-sweeper` — "A
    stream closed before `done` stays resumable"). The kill-9 rollout smoke
    cannot cover this: SIGKILL skips the generator's `finally` entirely, so
    it can't distinguish the `saw_done` gate from the old unconditional
    write."""
    _patch_llm(monkeypatch, _GatedLlm)

    rm = app_ctx.state.run_metadata
    thread_id = str(uuid.uuid4())
    async with rm._pool.acquire() as conn:
        await conn.execute("DELETE FROM runs")
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, last_heartbeat_at) "
            "VALUES ($1, 1, 'running', now() - interval '60 seconds')",
            thread_id,
        )

    import agent.main as main_module

    gen = main_module._stream_agent_run(
        ChatRequest(
            messages=[ChatMessage(role="user", content="hi")], run_id=thread_id
        ),
        user_id=1,
        bearer_token="stub-token",
        request_id=None,
        app=app_ctx,
        settings=app_ctx.state.settings,
    )
    try:
        # Consume the opening frames; the `plan` frame is yielded from
        # inside the stream's try-block, so closing at that point simulates
        # a real client disconnect mid-run (GeneratorExit at an in-try yield).
        for _ in range(50):
            frame = await gen.__anext__()
            if b"event: plan" in frame:
                break
        else:
            pytest.fail("plan frame never arrived")
    finally:
        await gen.aclose()

    async with rm._pool.acquire() as conn:
        row = await conn.fetchrow("SELECT status FROM runs WHERE id=$1", thread_id)
    assert row["status"] == "running", (
        "a stream closed before `done` must leave the row running, not completed"
    )

    # ...and the row still resumes inside the window.
    resp = await _post_chat(
        app_ctx,
        ChatRequest(
            messages=[ChatMessage(role="user", content="hi")], run_id=thread_id
        ).model_dump(),
        user_id=1,
    )
    assert resp.status_code == 200, resp.text
    kinds = [f.get("kind") for f in _parsed_frames(resp.text)]
    assert "resumed" in kinds

    async with rm._pool.acquire() as conn:
        row = await conn.fetchrow("SELECT status FROM runs WHERE id=$1", thread_id)
    assert row["status"] == "completed"


async def test_retention_gc_failure_is_contained(app_ctx):
    """One failing `adelete_thread` is logged, the metadata deletions stand,
    and the remaining ids are still processed (spec `agent-run-store-py` —
    "Checkpoint-deletion failure is contained")."""
    rm = app_ctx.state.run_metadata
    ids = [str(uuid.uuid4()) for _ in range(2)]
    async with rm._pool.acquire() as conn:
        await conn.execute("DELETE FROM runs")
        for rid in ids:
            await conn.execute(
                "INSERT INTO runs (id, user_id, status, ended_at) "
                "VALUES ($1, 1, 'failed', now() - interval '60 days')",
                rid,
            )

    from agent.retention import RetentionCron

    saver = _FlakySaver()
    settings = app_ctx.state.settings.model_copy(update={"retention_days": 30})
    cron = RetentionCron(rm, None, saver, settings=settings)
    await cron._run_once()  # must not raise

    assert sorted(saver.attempted) == sorted(ids), (
        f"both ids must be attempted; got {saver.attempted}"
    )
    async with rm._pool.acquire() as conn:
        left = await conn.fetchval("SELECT count(*) FROM runs")
    assert left == 0, "metadata deletions must stand despite the GC failure"
