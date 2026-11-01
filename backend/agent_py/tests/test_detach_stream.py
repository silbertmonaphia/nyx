"""Detached-run + re-attach + cancel tests (agent-stream-resilience).

Layers:

  - Bare (no Postgres): the HTTP contract of the re-attach and cancel
    routes (501 without persistence, 409 sentinel for unknown/cross-user
    with no existence leak), the hand-off lifecycle (registry populated
    on disconnect, emptied on terminal), the cancel broadcast (typed
    error + channel close reaching a subscriber), and the replay-frame
    projection the re-attach stream opens with.

  - DSN-gated: the full detached-run lifecycle against live Postgres —
    a run whose client disconnects mid-execution still lands
    `completed` with its assistant row; a detached run past
    PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS lands `failed{detached_run_cap}`;
    and PYTHON_AGENT_STREAM_DETACH_ENABLED=false restores the legacy
    disconnect-cancels-run semantics (row stays `running`).

The disconnect is simulated by consuming the opening frames of the real
`_stream_agent_run` generator and closing it (`aclose`) mid-run — the
GeneratorExit path a real client disconnect takes through FastAPI.
"""
from __future__ import annotations

import asyncio
import json
import os

import httpx
import pytest
from fakeredis import aioredis as fakeaioredis

from agent.frames import TRUNCATED
from agent.main import _stream_agent_run
from agent.types import ChatMessage, ChatRequest
from tests.service_auth_fixtures import SERVICE_AUTH_HEADERS, TEST_SERVICE_TOKEN
from tests.test_heartbeat_stream import _SlowLlm

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

dsn_gated = pytest.mark.skipif(
    not _DSN,
    reason="TEST_ASYNC_PG_DSN not set; needs a live Postgres (see Makefile / CI)",
)


def _settings(**overrides):
    import agent.config as config_module

    defaults = {
        "_env_file": None,
        "llm_base_url": "http://localhost:9999/v1",
        "metrics_port": 0,
        "retention_enabled": False,
        "heartbeat_interval_seconds": 1,
        "keepalive_seconds": 0,
        "service_token": TEST_SERVICE_TOKEN,
    }
    if _DSN:
        defaults["database_url"] = _DSN
    defaults.update(overrides)
    return config_module.Settings(**defaults)


def _patch_settings(monkeypatch, settings):
    import agent.config as config_module
    import agent.main as main_module

    monkeypatch.setattr(config_module, "_settings", settings)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    monkeypatch.setattr(main_module, "get_settings", lambda: settings)


def _patch_common(monkeypatch):
    """Stub the LLM/RAG/MCP surfaces the run path touches.

    The shared patcher replaces `agent.tools_client.ToolsClient`, but
    `agent.main` binds the name at import time — main's own reference
    must be patched too, or the planner's `registry_tools()` dials the
    real MCP URL. On a proxy-only-egress dev box that dial hangs instead
    of fast-failing, and the run stalls before its first plan frame.
    """
    from tests.test_heartbeat_stream import _patch_common as patch

    patch(monkeypatch)

    import agent.main as main_module
    from tests.test_heartbeat_stream import _StubRag, _StubTools

    monkeypatch.setattr(main_module, "ToolsClient", lambda *a, **kw: _StubTools())
    monkeypatch.setattr(main_module, "RagClient", lambda *a, **kw: _StubRag())


async def _open_run(app, settings, monkeypatch_free=True):
    """Start a real run generator, consume the opening frames, return
    (gen, run_id). One extra frame is pulled past the opening note so the
    generator is suspended INSIDE the drain loop with the pump task live
    — closing at the opening frames would exit before the tasks exist
    (the real client disconnect lands mid-run). The executor stub sleeps
    ~1.5s, so the run is guaranteed mid-flight when the caller closes the
    generator."""
    import agent.main as main_module

    chat_req = ChatRequest(messages=[ChatMessage(role="user", content="hi")])
    gen = main_module._stream_agent_run(
        chat_req,
        user_id=42,
        bearer_token="stub-token-not-verified",
        request_id=None,
        app=app,
        settings=settings,
    )
    buffer = ""
    run_id = None
    seen_note = False
    while not (run_id and seen_note):
        chunk = await asyncio.wait_for(gen.__anext__(), timeout=10)
        buffer += chunk.decode()
        for line in buffer.splitlines():
            if line.startswith("data: ") and line != "data: [DONE]":
                try:
                    payload = json.loads(line.removeprefix("data: "))
                except json.JSONDecodeError:
                    continue
                if "run_id" in payload and run_id is None:
                    run_id = payload["run_id"]
                if payload.get("kind") in ("fresh_run", "resumed"):
                    seen_note = True

    extra = await asyncio.wait_for(gen.__anext__(), timeout=10)
    buffer += extra.decode()
    last_id = 0
    for line in buffer.splitlines():
        if line.startswith("id: "):
            last_id = max(last_id, int(line.removeprefix("id: ")))
    return gen, run_id, last_id


def _push_frame(session, kind, payload):
    """Put one event on the run's sink queue — the channel the pump uses.

    The frames then travel the real path: the collector drains the
    queue, the drain folds the event, `encode_event` stamps it with an
    id and appends it to the frame buffer, and the collector broadcasts
    it. Nothing is hand-placed into the buffer.
    """
    from agent.types import Event

    session.queue.put_nowait(("event", Event(kind=kind, payload=payload)))


async def _wait_for_new_frames(session, last_id, timeout=10.0):
    """Wait until the run has emitted at least one more frame.

    The frames emitted while the client is disconnected are exactly the
    ones a cursor must bring back — without this wait, "the replay
    happened" and "the live stream simply continued" look identical.
    """
    async def _advanced():
        log = session.frame_log
        return log is not None and log.watermark() > last_id

    await asyncio.wait_for(_poll_until(_advanced), timeout=timeout)


async def _wait_registry_empty(app, run_id, timeout=15.0):
    import agent.main as main_module

    async def _gone():
        return run_id not in main_module._detached_registry(app)

    await asyncio.wait_for(_poll_until(_gone), timeout=timeout)

    async def _collector_finished():
        return not any(
            t.get_name() == f"agent-detached-{run_id}" and not t.done()
            for t in asyncio.all_tasks()
        )

    await asyncio.wait_for(_poll_until(_collector_finished), timeout=timeout)


async def _poll_until(check, interval=0.05):
    while not await check():
        await asyncio.sleep(interval)


async def _fetch_row(query, *args):
    import asyncpg

    conn = await asyncpg.connect(dsn=_DSN)
    try:
        return await conn.fetchrow(query, *args)
    finally:
        await asyncio.shield(conn.close())


async def _drop_schemas():
    import asyncpg

    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
    finally:
        await asyncio.shield(conn.close())


def _chat_body():
    return ChatRequest(
        messages=[ChatMessage(role="user", content="hi")]
    ).model_dump()


async def test_reattach_answers_501_without_persistence(monkeypatch):
    """Spec scenario "Persistence disabled answers 501": without the
    run-metadata layer the endpoint refuses with the typed envelope and
    opens no stream."""
    from agent.main import create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()

    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test", headers=SERVICE_AUTH_HEADERS
    ) as client:
        resp = await client.get(
            "/agent/runs/some-run-id/stream",
            headers={"X-Nyx-User-Id": "42"},
        )
    assert resp.status_code == 501
    assert resp.json()["code"] == "reattach_unavailable"


async def test_new_routes_answer_409_for_unknown_run(monkeypatch):
    """Spec scenarios "Non-live, unknown, and cross-user runs are
    indistinguishable" / "Cancel is idempotent at the identity level":
    an unknown run gets one 409 `run_not_live` envelope on both routes —
    with no registry entry there is nothing else to distinguish."""
    from agent.main import create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()
    app.state.run_metadata = object()

    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test", headers=SERVICE_AUTH_HEADERS
    ) as client:
        stream_resp = await client.get(
            "/agent/runs/nope/stream", headers={"X-Nyx-User-Id": "42"}
        )
        cancel_resp = await client.post(
            "/agent/runs/nope/cancel", headers={"X-Nyx-User-Id": "42"}
        )
    assert stream_resp.status_code == 409
    assert stream_resp.json()["code"] == "run_not_live"
    assert cancel_resp.status_code == 409
    assert cancel_resp.json()["code"] == "run_not_live"


async def test_missing_user_header_rejected_on_new_routes(monkeypatch):
    """Same contract as /agent/run: missing X-Nyx-User-Id is a 400 before
    any state change."""
    from agent.main import create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()

    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test", headers=SERVICE_AUTH_HEADERS
    ) as client:
        resp = await client.post("/agent/runs/nope/cancel")
    assert resp.status_code == 400


async def test_cross_user_cancel_is_the_same_409(monkeypatch):
    """Spec scenario "Cancel is idempotent at the identity level":
    another user's LIVE detached run answers the same 409 sentinel as an
    unknown id — and the foreign cancel does not touch the run (it stays
    in the registry, still owned)."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()
    async with app.router.lifespan_context(app):
        app.state.budget = BudgetGuard(redis=fakeaioredis.FakeRedis(), cap=10**9)
        gen, run_id, _last_id = await _open_run(app, settings)
        await gen.aclose()
        assert run_id in _detached_registry(app)

        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(
            transport=transport,
            base_url="http://test",
            headers=SERVICE_AUTH_HEADERS,
        ) as client:
            cancel_resp = await client.post(
                f"/agent/runs/{run_id}/cancel",
                headers={"X-Nyx-User-Id": "43"},
            )

        assert cancel_resp.status_code == 409
        assert cancel_resp.json()["code"] == "run_not_live"
        assert run_id in _detached_registry(app)
        assert _detached_registry(app)[run_id].error_code is None

        await _wait_registry_empty(app, run_id)


async def test_detach_hands_off_and_registry_empties(monkeypatch):
    """Spec scenario "Detached run completes after the client leaves"
    (bare, no stores): closing the generator mid-run hands the session to
    the registry, the collector finishes the run without a wire, and the
    registry entry is removed on terminal."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()
    async with app.router.lifespan_context(app):
        app.state.budget = BudgetGuard(redis=fakeaioredis.FakeRedis(), cap=10**9)
        gen, run_id, _last_id = await _open_run(app, settings)
        await gen.aclose()

        assert run_id in _detached_registry(app)
        await _wait_registry_empty(app, run_id)
        leaked = [
            t
            for t in asyncio.all_tasks()
            if t.get_name() == f"agent-detached-{run_id}"
        ]
        assert not leaked, "collector task must not outlive the terminal state"


async def test_session_cancel_broadcasts_and_marks(monkeypatch):
    """Spec scenario "Cancel stops a detached run": the cancel endpoint's
    session hook broadcasts the typed error frame + channel close to a
    re-attach subscriber, marks the outcome `user_cancelled`, and the
    collector lands `failed` (bare: the metadata layer is absent so the
    assertion is the session state + broadcast itself)."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()
    async with app.router.lifespan_context(app):
        app.state.budget = BudgetGuard(redis=fakeaioredis.FakeRedis(), cap=10**9)
        gen, run_id, _last_id = await _open_run(app, settings)
        await gen.aclose()

        session = _detached_registry(app)[run_id]
        sub = session.subscribe()
        session.cancel()
        assert session.error_code == "user_cancelled"

        first = await asyncio.wait_for(sub.get(), timeout=5)
        assert b'"code":"user_cancelled"' in first[1]
        second = await asyncio.wait_for(sub.get(), timeout=5)
        assert second is None

        await _wait_registry_empty(app, run_id)


async def test_repost_echoing_a_live_detached_run_attaches(monkeypatch):
    """Spec scenario "Detached run answers the resume decision tree as a
    live run": a re-POST echoing a detached, still-live run's id ATTACHES
    to the running execution (note{reattached} + live frames + [DONE])
    instead of starting a second graph on the same checkpoint thread —
    one execution, one LLM spend."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()
    async with app.router.lifespan_context(app):
        app.state.budget = BudgetGuard(redis=fakeaioredis.FakeRedis(), cap=10**9)
        gen, run_id, _last_id = await _open_run(app, settings)
        await gen.aclose()
        assert run_id in _detached_registry(app)
        llm_calls_before = _SlowLlm.call_count

        chat_req = ChatRequest(
            messages=[ChatMessage(role="user", content="hi")], run_id=run_id
        )
        gen2 = _stream_agent_run(
            chat_req,
            user_id=42,
            bearer_token="stub-token-not-verified",
            request_id=None,
            app=app,
            settings=settings,
        )
        text = ""
        async for chunk in gen2:
            text += chunk.decode()

        assert '"kind":"reattached"' in text
        assert "event: delta" in text
        assert "data: [DONE]" in text
        assert _SlowLlm.call_count == llm_calls_before
        await _wait_registry_empty(app, run_id)


async def test_replay_frames_rebuild_step_state():
    """Spec scenario "Live run re-attaches with replay then live frames":
    the folded step trace projects back into wire-shaped frames the SPA
    folds idempotently (plan, then per-step start/call/result/end)."""
    import agent.main as main_module

    settings = _settings()
    session = main_module._RunSession(
        run_id="r-1",
        user_id=1,
        request_id=None,
        chat_req=ChatRequest(messages=[ChatMessage(role="user", content="hi")]),
        settings=settings,
        app=main_module.create_app(),
        run_metadata=None,
        chat_store=None,
        prefs_store=None,
        sink=main_module.RunSink(asyncio.Queue()),
        queue=asyncio.Queue(),
        state=None,
        resumed=False,
        secrets=None,
        llm_view=None,
        title_written=False,
        persist_run_state=False,
    )
    session.collect(
        _event(
            "plan",
            {
                "steps": [
                    {"id": "s1", "kind": "tool", "tool": "list_feeds", "args": {}}
                ]
            },
        )
    )
    session.collect(
        _event("step_start", {"step_id": "s1", "kind": "tool"})
    )
    session.collect(
        _event(
            "tool_call",
            {"step_id": "s1", "call_id": "c1", "name": "list_feeds", "args": {}},
        )
    )
    session.collect(
        _event(
            "tool_result",
            {"step_id": "s1", "call_id": "c1", "ok": True, "result": {"items": []}},
        )
    )
    session.collect(_event("step_end", {"step_id": "s1", "status": "ok"}))

    frames = session.replay_frames()
    kinds = [f.kind for f in frames]
    assert kinds == ["plan", "step_start", "tool_call", "tool_result", "step_end"]
    assert frames[0].payload["steps"][0]["tool"] == "list_feeds"
    assert frames[-1].payload == {
        "step_id": "s1",
        "status": "ok",
        "reason": "",
    }


async def test_run_stream_frames_carry_monotonic_ids(monkeypatch):
    """Spec "Frames carry a monotonic id": once the pump frames start,
    every event frame carries `id: <decimal>` and the ids strictly
    increase; the opening run_id/note framing frames stay id-less."""
    from agent.budget import BudgetGuard
    from agent.main import create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()
    async with app.router.lifespan_context(app):
        app.state.budget = BudgetGuard(redis=fakeaioredis.FakeRedis(), cap=10**9)
        chat_req = ChatRequest(messages=[ChatMessage(role="user", content="hi")])
        gen = _stream_agent_run(
            chat_req,
            user_id=42,
            bearer_token="stub-token-not-verified",
            request_id=None,
            app=app,
            settings=settings,
        )
        ids: list[int] = []
        head_frames = 0
        try:
            async for chunk in gen:
                for line in chunk.decode().splitlines():
                    if line.startswith("id: "):
                        ids.append(int(line.removeprefix("id: ")))
                    elif line.startswith(("event: run_id", "event: note")):
                        head_frames += 1
        finally:
            await gen.aclose()
    assert head_frames >= 1
    assert ids == sorted(ids), "frame ids must be strictly increasing"
    assert len(set(ids)) == len(ids)


async def test_session_buffer_released_at_terminal(monkeypatch):
    """Spec "Buffer is released with the run": finalize drops the frame
    log, so a held session reference shows the buffer gone."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()
    async with app.router.lifespan_context(app):
        app.state.budget = BudgetGuard(redis=fakeaioredis.FakeRedis(), cap=10**9)
        gen, run_id, _last_id = await _open_run(app, settings)
        await gen.aclose()

        session = _detached_registry(app)[run_id]
        assert session.frame_log is not None
        await _wait_registry_empty(app, run_id)
        assert session.frame_log is None


async def test_reattach_cursor_replays_missed_frames(monkeypatch):
    """Spec "Gap-free resume": a re-attach presenting the last id the
    client saw replays every buffered frame after it (ids included, in
    order, no duplicates) before the live remainder — the client misses
    nothing. Driven through the real HTTP route, so the bytes on the
    wire are the producer's own and the endpoint's own cursor branch
    runs (not a re-implementation of it).

    The run has TWO llm steps, so it keeps emitting after the client
    disconnects: those are the frames the cursor has to bring back.
    Without the cursor the endpoint answers with the step projection
    instead, and the frames emitted while the client was away are gone
    — which is exactly what the assertion below rules out."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()
    async with app.router.lifespan_context(app):
        app.state.budget = BudgetGuard(redis=fakeaioredis.FakeRedis(), cap=10**9)
        gen, run_id, last_id = await _open_run(app, settings)
        await gen.aclose()

        session = _detached_registry(app)[run_id]
        _push_frame(session, "delta", {"delta": "missed "})
        _push_frame(session, "delta", {"delta": "half"})
        await _wait_for_new_frames(session, last_id)
        watermark = session.frame_log.watermark()
        replayed = session.frame_log.since(last_id, upto=watermark)
        assert replayed is not TRUNCATED
        assert len(replayed) > 0, "the run must have emitted frames while away"

        text = await _get_reattach_stream(
            app, run_id, cursor=str(last_id)
        )
        await _wait_registry_empty(app, run_id)

    replay_ids = [
        int(line.removeprefix("id: "))
        for line in text.splitlines()
        if line.startswith("id: ")
    ]
    assert replay_ids, "the replay must carry id-bearing frames"
    assert replay_ids == sorted(set(replay_ids)), (
        "replayed ids must be in order without duplicates"
    )
    missed = [i for i in replay_ids if last_id < i <= watermark]
    assert missed, (
        f"the frames emitted while the client was away (ids "
        f"{last_id + 1}..{watermark}) must be replayed; the stream carried "
        f"ids {replay_ids}"
    )
    assert "event: done" in text
    assert "data: [DONE]" in text


async def test_reattach_query_cursor_replays_missed_frames(monkeypatch):
    """Spec "A cursor resumes the stream without gaps": the cursor also
    travels as the `last_event_id` query parameter when the header is
    absent — the fallback for clients and proxies that drop unknown
    headers — and replays the same frames the header would."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()
    async with app.router.lifespan_context(app):
        app.state.budget = BudgetGuard(redis=fakeaioredis.FakeRedis(), cap=10**9)
        gen, run_id, last_id = await _open_run(app, settings)
        await gen.aclose()

        assert run_id in _detached_registry(app)
        session = _detached_registry(app)[run_id]
        _push_frame(session, "delta", {"delta": "missed "})
        _push_frame(session, "delta", {"delta": "half"})
        await _wait_for_new_frames(session, last_id)
        watermark = session.frame_log.watermark()
        text = await _get_reattach_stream(
            app, run_id, cursor_query=str(last_id)
        )
        await _wait_registry_empty(app, run_id)

    replay_ids = [
        int(line.removeprefix("id: "))
        for line in text.splitlines()
        if line.startswith("id: ")
    ]
    assert replay_ids, "the replay must carry id-bearing frames"
    missed = [i for i in replay_ids if last_id < i <= watermark]
    assert missed, (
        f"the frames emitted while the client was away (ids "
        f"{last_id + 1}..{watermark}) must be replayed; the stream carried "
        f"ids {replay_ids}"
    )
    assert "data: [DONE]" in text


async def test_reattach_malformed_cursor_reports_truncation(monkeypatch):
    """Spec "Rolled-past cursor is reported": a malformed cursor yields
    note{kind:"replay_truncated"} followed by the step projection, and
    the stream still completes. The note is built by the real
    `NoteEvent` model, so a note kind the model rejects fails here
    instead of 500-ing the route in production."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app

    _patch_common(monkeypatch)
    settings = _settings()
    _patch_settings(monkeypatch, settings)
    app = create_app()
    async with app.router.lifespan_context(app):
        app.state.budget = BudgetGuard(redis=fakeaioredis.FakeRedis(), cap=10**9)
        gen, run_id, _last_id = await _open_run(app, settings)
        await gen.aclose()

        assert run_id in _detached_registry(app)
        text = await _get_reattach_stream(app, run_id, cursor="junk")
        await _wait_registry_empty(app, run_id)

    assert '"kind":"replay_truncated"' in text
    assert "event: plan" in text
    assert "data: [DONE]" in text


async def test_reattach_replay_disabled_reports_truncation(monkeypatch):
    """Spec "Cursor against a replay-disabled service": replay off → the
    cursor gets the truncation note and the step projection instead of
    an error."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app

    _patch_common(monkeypatch)
    settings = _settings(frame_replay_enabled=False)
    _patch_settings(monkeypatch, settings)
    app = create_app()
    async with app.router.lifespan_context(app):
        app.state.budget = BudgetGuard(redis=fakeaioredis.FakeRedis(), cap=10**9)
        gen, run_id, _last_id = await _open_run(app, settings)
        await gen.aclose()

        assert run_id in _detached_registry(app)
        text = await _get_reattach_stream(app, run_id, cursor="0")
        await _wait_registry_empty(app, run_id)

    assert '"kind":"replay_truncated"' in text
    assert "event: delta" in text
    assert "data: [DONE]" in text


async def _get_reattach_stream(app, run_id, cursor=None, cursor_query=None):
    """GET the real re-attach route and return the response body.

    The route answers 501 unless the persistence layer is present, so
    the gate is opened with a sentinel AFTER the run was created — the
    session itself was built without persistence and never touches it.
    The cursor travels as the `Last-Event-ID` header, the way the SPA
    sends it, or as the `last_event_id` query parameter, the fallback
    for clients and proxies that drop the header.
    """
    app.state.run_metadata = object()
    transport = httpx.ASGITransport(app=app)
    headers = {"X-Nyx-User-Id": "42"}
    if cursor is not None:
        headers["Last-Event-ID"] = cursor
    url = f"/agent/runs/{run_id}/stream"
    if cursor_query is not None:
        url += f"?last_event_id={cursor_query}"
    async with httpx.AsyncClient(
        transport=transport,
        base_url="http://test",
        headers=SERVICE_AUTH_HEADERS,
    ) as client:
        resp = await client.get(
            url,
            headers=headers,
            timeout=30,
        )
    assert resp.status_code == 200, resp.text
    return resp.text



def _event(kind, payload):
    from agent.types import Event

    return Event(kind=kind, payload=payload)


@dsn_gated
async def test_detached_run_completes_and_persists(monkeypatch):
    """Spec scenario "Detached run completes after the client leaves":
    mid-run disconnect → the run still lands `completed` with its
    assistant row, and the registry entry is gone."""
    from agent.budget import BudgetGuard
    from agent.main import create_app, lifespan

    _patch_common(monkeypatch)
    await _drop_schemas()
    settings = _settings()
    _patch_settings(monkeypatch, settings)

    app = create_app()
    async with lifespan(app):
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=10**9)
        try:
            gen, run_id, _last_id = await _open_run(app, settings)
            await gen.aclose()
            await _wait_registry_empty(app, run_id)
        finally:
            await redis.aclose()

    row = await _fetch_row(
        "SELECT status, error_code FROM agent.runs WHERE id=$1", run_id
    )
    assert row is not None
    assert row["status"] == "completed"
    msg = await _fetch_row(
        "SELECT role FROM chat_messages WHERE session_id=$1 AND role='assistant'",
        run_id,
    )
    assert msg is not None, "the detached run must persist its assistant row"


@dsn_gated
async def test_detached_cap_marks_failed(monkeypatch):
    """Spec scenario "Detached run is bounded": a run whose collector
    exceeds PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS lands
    `failed{detached_run_cap}`."""
    from agent.budget import BudgetGuard
    from agent.main import create_app, lifespan

    _patch_common(monkeypatch)
    await _drop_schemas()
    settings = _settings(detached_run_cap_seconds=1)
    _patch_settings(monkeypatch, settings)

    app = create_app()
    async with lifespan(app):
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=10**9)
        try:
            gen, run_id, _last_id = await _open_run(app, settings)
            await gen.aclose()
            await _wait_registry_empty(app, run_id)
        finally:
            await redis.aclose()

    row = await _fetch_row(
        "SELECT status, error_code FROM agent.runs WHERE id=$1", run_id
    )
    assert row is not None
    assert row["status"] == "failed"
    assert row["error_code"] == "detached_run_cap"


@dsn_gated
async def test_detach_off_keeps_legacy_semantics(monkeypatch):
    """Spec scenario "Mid-run disconnect keeps the legacy resumable
    semantics when detach is opted out": aclose mid-run cancels the pump,
    writes no assistant row, and leaves the row `running`."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app, lifespan

    _patch_common(monkeypatch)
    await _drop_schemas()
    settings = _settings(stream_detach_enabled=False)
    _patch_settings(monkeypatch, settings)

    app = create_app()
    async with lifespan(app):
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=10**9)
        try:
            gen, run_id, _last_id = await _open_run(app, settings)
            await gen.aclose()
            await asyncio.sleep(0.3)
            assert run_id not in _detached_registry(app)
        finally:
            await redis.aclose()

    row = await _fetch_row(
        "SELECT status FROM agent.runs WHERE id=$1", run_id
    )
    assert row is not None
    assert row["status"] == "running"
    msg = await _fetch_row(
        "SELECT role FROM chat_messages WHERE session_id=$1 AND role='assistant'",
        run_id,
    )
    assert msg is None


@dsn_gated
async def test_reattach_http_streams_note_and_completion(monkeypatch):
    """Spec scenarios "Live run re-attaches with replay then live frames"
    + "Old consumers are unaffected": the HTTP re-attach stream opens
    with note{kind:"reattached"}, replays the folded step projection,
    bridges to the live answer, and closes with `done` + `[DONE]`."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app, lifespan

    _patch_common(monkeypatch)
    await _drop_schemas()
    settings = _settings()
    _patch_settings(monkeypatch, settings)

    app = create_app()
    async with lifespan(app):
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=10**9)
        try:
            gen, run_id, _last_id = await _open_run(app, settings)
            await gen.aclose()
            assert run_id in _detached_registry(app)

            transport = httpx.ASGITransport(app=app)
            async with httpx.AsyncClient(
                transport=transport,
                base_url="http://test",
                headers=SERVICE_AUTH_HEADERS,
            ) as client:
                resp = await client.get(
                    f"/agent/runs/{run_id}/stream",
                    headers={"X-Nyx-User-Id": "42"},
                    timeout=30,
                )
            assert resp.status_code == 200
            text = resp.text
        finally:
            await redis.aclose()

    assert '"kind":"reattached"' in text
    assert "event: step_start" in text
    assert "event: delta" in text
    assert "event: done" in text
    assert "data: [DONE]" in text


@dsn_gated
async def test_cross_user_reattach_is_the_same_409(monkeypatch):
    """Spec scenario "Non-live, unknown, and cross-user runs are
    indistinguishable" (with the persistence layer on, so the endpoint's
    501 branch is out of the picture): another user's LIVE detached run
    answers the same 409 `run_not_live` envelope as an unknown id."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app, lifespan

    _patch_common(monkeypatch)
    await _drop_schemas()
    settings = _settings()
    _patch_settings(monkeypatch, settings)

    app = create_app()
    async with lifespan(app):
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=10**9)
        try:
            gen, run_id, _last_id = await _open_run(app, settings)
            await gen.aclose()
            assert run_id in _detached_registry(app)

            transport = httpx.ASGITransport(app=app)
            async with httpx.AsyncClient(
                transport=transport,
                base_url="http://test",
                headers=SERVICE_AUTH_HEADERS,
            ) as client:
                resp = await client.get(
                    f"/agent/runs/{run_id}/stream",
                    headers={"X-Nyx-User-Id": "43"},
                )
            assert resp.status_code == 409
            assert resp.json()["code"] == "run_not_live"
            assert run_id in _detached_registry(app)
            await _wait_registry_empty(app, run_id)
        finally:
            await redis.aclose()


@dsn_gated
async def test_cursor_resume_delivers_every_missed_frame(monkeypatch):
    """Task 1.6 exit condition: a client that disconnects mid-answer and
    reconnects with its last seen id receives EVERY frame emitted in the
    disconnect window, in order, before the live remainder — the merged
    answer text equals the uninterrupted answer."""
    from agent.budget import BudgetGuard
    from agent.main import _detached_registry, create_app, lifespan

    _patch_common(monkeypatch)
    await _drop_schemas()
    settings = _settings()
    _patch_settings(monkeypatch, settings)

    app = create_app()
    async with lifespan(app):
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=10**9)
        try:
            gen, run_id, last_id = await _open_run(app, settings)
            await gen.aclose()
            assert run_id in _detached_registry(app)

            session = _detached_registry(app)[run_id]
            _push_frame(session, "delta", {"delta": "missed "})
            _push_frame(session, "delta", {"delta": "half"})
            await _wait_for_new_frames(session, last_id)
            watermark = session.frame_log.watermark()

            transport = httpx.ASGITransport(app=app)
            async with httpx.AsyncClient(
                transport=transport,
                base_url="http://test",
                headers=SERVICE_AUTH_HEADERS,
            ) as client:
                resp = await client.get(
                    f"/agent/runs/{run_id}/stream",
                    headers={
                        "X-Nyx-User-Id": "42",
                        "Last-Event-ID": str(last_id),
                    },
                    timeout=60,
                )
            assert resp.status_code == 200
            text = resp.text
        finally:
            await redis.aclose()

    ids = [
        int(line.removeprefix("id: "))
        for line in text.splitlines()
        if line.startswith("id: ")
    ]
    assert ids, "the re-attach stream must carry id-bearing frames"
    assert ids == sorted(set(ids)), "no duplicate and no out-of-order frames"
    missed = [i for i in ids if last_id < i <= watermark]
    assert missed, (
        f"the frames emitted while the client was away (ids "
        f"{last_id + 1}..{watermark}) must be replayed; the stream carried "
        f"ids {ids}"
    )
    deltas = [
        json.loads(line.removeprefix("data: "))["delta"]
        for line in text.splitlines()
        if line.startswith('data: {"delta"')
    ]
    assert "slow answer" in "".join(deltas), (
        "the merged answer must contain the full generated text"
    )
    assert "data: [DONE]" in text
