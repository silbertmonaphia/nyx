"""HTTP-surface tests for the GDPR endpoints (openspec/changes/agent-gdpr-compliance).

Drives the REAL FastAPI app over httpx's ASGI transport, built through
the real lifespan (`_running_app`, mirroring `test_prefs_injection.py`)
against a real Postgres pool — so this file is also the wiring test for
`main.py`: the lifespan must construct the `AuditLog` beside the other
stores, register the gdpr router, and hand the audit store to the
retention cron.

checkpoint GC: the lifespan builds the real langGraph `AsyncPostgresSaver`
(database_url is set); every test replaces `app.state.checkpointer` with a
recorder AFTER the lifespan — the endpoint must read the checkpointer at
request time for that override to take effect.

Covers (spec `agent-gdpr-erasure`):
  1. Export completeness for one user, with a SECOND user's data present
     and provably excluded from the body
  2. Empty export is a well-formed object (never 404)
  3. Soft-deleted sessions (and their messages) appear in the export;
     erasure removes soft-deleted rows too
  4. Export + erase audit rows carry counts only — every payload value is
     an int, and no seeded content string appears anywhere in the table
  5. Erase walks every store: response counts match the seeded rows, all
     five tables read zero for the subject afterwards, `adelete_thread`
     is invoked exactly once per run id
  6. Double erase: 200 with all-zero counts + a second audit row
  7. One `adelete_thread` failure: contained, remaining ids still
     attempted, counts reflect reality, still 200 + audit row
  8. Cross-user isolation: user B's rows survive user A's erase
  9. Kill switch: both endpoints 503 with the standard envelope, no
     audit row, no data change
  10. Missing / non-integer `X-Nyx-User-Id` → 400
  11. Audit-retention iteration reaps a backdated row, ticks
     `agent_audit_retention_deleted_total`, and runs while
     `gdpr_enabled=false`
  12. Per-endpoint OTel spans (`agent.gdpr.export` / `agent.gdpr.erase`)
     with user_id + count attributes; `agent.audit.retention` with
     deleted_count

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""
from __future__ import annotations

import json as _json
import os
import uuid as _uuid
from contextlib import asynccontextmanager
from datetime import datetime

import asyncpg
import httpx
import pytest

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

pytestmark = pytest.mark.skipif(
    not _DSN,
    reason="TEST_ASYNC_PG_DSN not set; needs a live Postgres",
)


async def _drop_schemas() -> None:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
    finally:
        await conn.close()


async def _build_pool() -> asyncpg.Pool:
    # `await`: asyncpg's `create_pool` returns an awaitable pool; until it
    # is awaited (or entered as an async context manager) it is not
    # initialized and `acquire()` raises InterfaceError.
    return await asyncpg.create_pool(
        dsn=_DSN,
        min_size=1,
        max_size=5,
        server_settings={"search_path": "agent,langgraph"},
    )


class _RecordingCheckpointer:
    """Duck-typed stand-in for langGraph's `AsyncPostgresSaver`.

    Records every `adelete_thread` call; ids in `fail_for` raise, so the
    contained-failure path is drivable without a real checkpoint store.
    """

    def __init__(self) -> None:
        self.calls: list[str] = []
        self.fail_for: set[str] = set()

    async def adelete_thread(self, thread_id: str) -> None:
        self.calls.append(str(thread_id))
        if str(thread_id) in self.fail_for:
            raise RuntimeError("simulated checkpoint GC failure")


def _patch_observability(monkeypatch) -> None:
    """Noop the OTLP exporter setup (no Jaeger in tests)."""
    from agent import observability as obs_module

    def _noop_setup(settings):
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider

        provider = TracerProvider()
        trace.set_tracer_provider(provider)
        return trace.get_tracer("nyx-agent-test")

    monkeypatch.setattr(obs_module, "setup_observability", _noop_setup)


@asynccontextmanager
async def _running_app(monkeypatch, **settings_overrides):
    """Live app: real lifespan, real DB, migrations applied, crons off.

    Yields `(app, pool)` — `pool` is the seed/read handle (assertions go
    through HTTP; SQL is only used to seed rows and to verify the
    post-state).
    """
    await _drop_schemas()

    import agent.config as config_module

    settings = config_module.Settings(
        _env_file=None,
        database_url=_DSN,
        llm_base_url="http://localhost:9999/v1",
        metrics_port=0,
        # Poppable so the M5 wiring test can build the cron
        # (retention_enabled=True); default off elsewhere (no background
        # timers in tests).
        retention_enabled=settings_overrides.pop("retention_enabled", False),
        sweep_enabled=False,
        chat_retention_enabled=False,
        **settings_overrides,
    )
    monkeypatch.setattr(config_module, "_settings", settings)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    import agent.main as main_module

    monkeypatch.setattr(main_module, "get_settings", lambda: settings)

    _patch_observability(monkeypatch)

    from agent.main import create_app, lifespan

    app = create_app()
    async with lifespan(app):
        pool = await _build_pool()
        try:
            # Replace the real saver the lifespan built: the endpoint must
            # read `app.state.checkpointer` per request for this override
            # to take effect.
            app.state.checkpointer = _RecordingCheckpointer()
            yield app, pool
        finally:
            await pool.close()


@pytest.fixture
async def app_ctx(monkeypatch):
    async with _running_app(monkeypatch) as ctx:
        yield ctx


@pytest.fixture
async def app_ctx_gdpr_disabled(monkeypatch):
    """Kill switch on (`PYTHON_AGENT_GDPR_ENABLED=false`) + a 1-year audit
    window so the retention test's 400-day backdate is reapable."""
    async with _running_app(
        monkeypatch, gdpr_enabled=False, audit_retention_days=365
    ) as ctx:
        yield ctx


@pytest.fixture
async def app_ctx_audit_short(monkeypatch):
    """Live app with a 1-year audit window (a 400-day backdate is reapable)."""
    async with _running_app(monkeypatch, audit_retention_days=365) as ctx:
        yield ctx


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


async def _call(app, method: str, path: str, *, user_id: int | str | None = 1):
    """Drive the app over ASGI. `user_id=None` omits the header entirely."""
    headers: dict[str, str] = {}
    if user_id is not None:
        headers["X-Nyx-User-Id"] = str(user_id)
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        return await client.request(method, path, headers=headers)


async def _seed_user(
    pool: asyncpg.Pool,
    user_id: int,
    *,
    session_message_counts: tuple[int, ...] = (2, 3),
    prefs: int = 1,
    runs: int = 2,
    soft_deleted_sessions: int = 0,
) -> dict:
    """Seed one user's full footprint; returns the ids for assertions.

    Message content is user-tagged (`msg-{user}-...`) so a cross-user
    leak shows up as a substring in the raw response body.
    """
    session_ids: list[str] = []
    contents: list[str] = []
    pref_keys: list[str] = []
    run_ids: list[str] = []
    async with pool.acquire() as conn:
        for s_index, n_messages in enumerate(session_message_counts):
            sid = str(_uuid.uuid4())
            session_ids.append(sid)
            await conn.execute(
                "INSERT INTO agent.chat_sessions (id, user_id, title) "
                "VALUES ($1, $2, $3)",
                _uuid.UUID(sid),
                user_id,
                f"session-{user_id}-{s_index}",
            )
            for m_index in range(n_messages):
                content = f"msg-{user_id}-{s_index}-{m_index}"
                contents.append(content)
                await conn.execute(
                    "INSERT INTO agent.chat_messages "
                    "(session_id, role, content, tokens) VALUES ($1, $2, $3, $4)",
                    _uuid.UUID(sid),
                    "user" if m_index % 2 == 0 else "assistant",
                    content,
                    m_index,
                )
        for sid in session_ids[:soft_deleted_sessions]:
            await conn.execute(
                "UPDATE agent.chat_sessions SET deleted_at = now() WHERE id = $1",
                _uuid.UUID(sid),
            )
        for p_index in range(prefs):
            key = f"key-{user_id}-{p_index}"
            pref_keys.append(key)
            await conn.execute(
                "INSERT INTO agent.chat_user_prefs "
                "(user_id, key, value, source_session_id) "
                "VALUES ($1, $2, $3::jsonb, $4)",
                user_id,
                key,
                _json.dumps(f"val-{user_id}-{p_index}"),
                _uuid.UUID(session_ids[0]) if session_ids else None,
            )
        for r_index in range(runs):
            rid = str(_uuid.uuid4())
            run_ids.append(rid)
            await conn.execute(
                "INSERT INTO agent.runs "
                "(id, user_id, status, total_tokens, ended_at) "
                "VALUES ($1, $2, 'completed', $3, now())",
                _uuid.UUID(rid),
                user_id,
                10 + r_index,
            )
    return {
        "session_ids": session_ids,
        "contents": contents,
        "pref_keys": pref_keys,
        "run_ids": run_ids,
    }


async def _counts_for(pool: asyncpg.Pool, user_id: int) -> dict[str, int]:
    """Row counts per table for one user (runs via the metadata table)."""
    async with pool.acquire() as conn:
        sessions = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_sessions WHERE user_id=$1", user_id
        )
        messages = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_messages m "
            "JOIN agent.chat_sessions s ON s.id = m.session_id "
            "WHERE s.user_id=$1",
            user_id,
        )
        prefs = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_user_prefs WHERE user_id=$1", user_id
        )
        runs = await conn.fetchval(
            "SELECT count(*) FROM agent.runs WHERE user_id=$1", user_id
        )
    return {
        "sessions": sessions,
        "messages": messages,
        "prefs": prefs,
        "runs": runs,
    }


async def _audit_rows(pool: asyncpg.Pool, user_id: int | None = None) -> list[dict]:
    """Audit rows as plain dicts (payload decoded from jsonb)."""
    sql = (
        "SELECT user_id, action, payload_summary, requested_at "
        "FROM agent.data_audit_log"
    )
    args: tuple = ()
    if user_id is not None:
        sql += " WHERE user_id=$1"
        args = (user_id,)
    sql += " ORDER BY id ASC"
    async with pool.acquire() as conn:
        rows = await conn.fetch(sql, *args)
    out = []
    for row in rows:
        payload = row["payload_summary"]
        out.append(
            {
                "user_id": row["user_id"],
                "action": row["action"],
                "payload": _json.loads(payload) if isinstance(payload, str) else payload,
                "requested_at": row["requested_at"],
                "raw": str(payload),
            }
        )
    return out


def _export_body_users(body: dict) -> set[int]:
    """Every user id the export body exposes.

    Only the row groups that carry a `user_id` field: sessions and runs
    (prefs rows are keyed by their owner implicitly and carry no
    `user_id` column read; messages likewise — both are checked via
    their content markers instead)."""
    users = {body["user_id"]}
    for key in ("sessions", "runs"):
        users.update(row["user_id"] for row in body[key])
    return users


# ---------------------------------------------------------------------------
# Export
# ---------------------------------------------------------------------------


async def test_export_is_complete_for_one_user(app_ctx) -> None:
    """Spec scenario "Export is complete for one user" + cross-user exclusion."""
    app, pool = app_ctx
    mine = await _seed_user(pool, 1)
    # A second user's full footprint, present in every table.
    other = await _seed_user(pool, 2)

    resp = await _call(app, "GET", "/agent/me/export", user_id=1)
    assert resp.status_code == 200, resp.text
    body = resp.json()

    assert body["user_id"] == 1
    # `exported_at` is an ISO-8601 timestamp.
    datetime.fromisoformat(body["exported_at"])

    assert sorted(s["id"] for s in body["sessions"]) == sorted(mine["session_ids"])
    assert sorted(m["content"] for m in body["messages"]) == sorted(mine["contents"])
    assert all(
        m["prompt_tokens"] is None and m["completion_tokens"] is None
        for m in body["messages"]
    )
    assert all(m["steps"] is None for m in body["messages"])
    assert sorted(p["key"] for p in body["prefs"]) == sorted(mine["pref_keys"])
    assert sorted(r["id"] for r in body["runs"]) == sorted(mine["run_ids"])
    # Thread ids are the union of run ids (single-turn threads) and
    # session ids (continuation threads carry no runs row; review
    # 2026-10-14 compliance fix) — deduped, exactly this user's.
    assert sorted(body["checkpoint_thread_ids"]) == sorted(
        set(mine["run_ids"]) | set(mine["session_ids"])
    )

    # Belt-and-braces cross-user exclusion: nothing of user 2's appears
    # anywhere in the serialized body, on any field.
    assert not any(
        marker in resp.text
        for marker in (
            *other["session_ids"],
            *other["contents"],
            *other["pref_keys"],
            *other["run_ids"],
        )
    ), "another user's data leaked into the export body"
    assert _export_body_users(body) == {1}


async def test_export_includes_the_step_trace(app_ctx) -> None:
    """Spec scenario "Export carries the trace for agent turns": the
    assistant row exports its persisted trace and every other row (user
    rows included) exports `steps: null`."""
    app, pool = app_ctx
    mine = await _seed_user(pool, 1)
    trace = [
        {"id": "step_1", "kind": "rag", "status": "ok"},
        {"id": "step_2", "kind": "llm", "status": "ok"},
    ]
    async with pool.acquire() as conn:
        assistant_id = await conn.fetchval(
            "SELECT id FROM agent.chat_messages "
            "WHERE role='assistant' AND content=$1",
            mine["contents"][1],
        )
        await conn.execute(
            "UPDATE agent.chat_messages SET steps=$2::jsonb WHERE id=$1",
            assistant_id,
            _json.dumps(trace),
        )

    resp = await _call(app, "GET", "/agent/me/export", user_id=1)
    assert resp.status_code == 200, resp.text
    rows = {m["id"]: m for m in resp.json()["messages"]}

    assert rows[assistant_id]["steps"] == trace
    assert all(
        row["steps"] is None
        for message_id, row in rows.items()
        if message_id != assistant_id
    )


async def test_export_empty_is_well_formed(app_ctx) -> None:
    """Spec scenario "Empty export is a well-formed object"."""
    app, _pool = app_ctx

    resp = await _call(app, "GET", "/agent/me/export", user_id=99)
    assert resp.status_code == 200, resp.text
    body = resp.json()

    assert body["user_id"] == 99
    assert body["sessions"] == []
    assert body["messages"] == []
    assert body["prefs"] == []
    assert body["runs"] == []
    assert body["checkpoint_thread_ids"] == []


async def test_export_includes_soft_deleted_sessions(app_ctx) -> None:
    """Spec scenario "Soft-deleted sessions are erased too" — the export half:
    a soft-deleted session (and its messages) must still be exported."""
    app, pool = app_ctx
    mine = await _seed_user(
        pool, 1, session_message_counts=(2, 1), soft_deleted_sessions=1
    )
    soft_id = mine["session_ids"][0]

    resp = await _call(app, "GET", "/agent/me/export", user_id=1)
    assert resp.status_code == 200, resp.text
    body = resp.json()

    exported = {s["id"]: s for s in body["sessions"]}
    assert sorted(exported) == sorted(mine["session_ids"])
    assert exported[soft_id]["deleted_at"] is not None, (
        "the soft-deleted row is present AND marked as soft-deleted"
    )
    # Its messages travel with it.
    assert sorted(m["content"] for m in body["messages"]) == sorted(mine["contents"])


# ---------------------------------------------------------------------------
# Erase
# ---------------------------------------------------------------------------


async def test_erase_walks_every_store(app_ctx) -> None:
    """Spec scenario "Erase walks every table": counts match, post-state
    zero, `adelete_thread` invoked once per run id."""
    app, pool = app_ctx
    mine = await _seed_user(pool, 1)
    await _seed_user(pool, 2)

    resp = await _call(app, "DELETE", "/agent/me", user_id=1)
    assert resp.status_code == 200, resp.text
    body = resp.json()

    assert body["deleted"] == {
        "sessions": 2,
        "messages": 5,
        "prefs": 1,
        "runs": 2,
        "threads": 4,  # 2 run-keyed + 2 session-keyed threads (review fix)
        "thread_failures": 0,
    }
    assert await _counts_for(pool, 1) == {
        "sessions": 0,
        "messages": 0,
        "prefs": 0,
        "runs": 0,
    }
    # Cross-user isolation: user 2 untouched.
    assert await _counts_for(pool, 2) == {
        "sessions": 2,
        "messages": 5,
        "prefs": 1,
        "runs": 2,
    }

    # Checkpoint GC ran exactly once per thread id: the union of run ids
    # and session ids (continuation threads are session-keyed; review
    # 2026-10-14 compliance fix).
    recorder = app.state.checkpointer
    assert sorted(recorder.calls) == sorted(
        set(mine["run_ids"]) | set(mine["session_ids"])
    )
    assert len(recorder.calls) == len(set(recorder.calls)) == 4


async def test_erase_removes_soft_deleted_sessions_too(app_ctx) -> None:
    """Spec scenario "Soft-deleted sessions are erased too": retention is
    not a precondition for erasure."""
    app, pool = app_ctx
    mine = await _seed_user(
        pool, 1, session_message_counts=(2, 1), soft_deleted_sessions=1
    )
    soft_id = mine["session_ids"][0]

    resp = await _call(app, "DELETE", "/agent/me", user_id=1)
    assert resp.status_code == 200, resp.text
    assert resp.json()["deleted"]["sessions"] == 2

    async with pool.acquire() as conn:
        still = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_sessions WHERE id=$1",
            _uuid.UUID(soft_id),
        )
        orphan_messages = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_messages WHERE session_id=$1",
            _uuid.UUID(soft_id),
        )
    assert still == 0
    assert orphan_messages == 0


async def test_double_erase_is_safe(app_ctx) -> None:
    """Spec scenario "Double erase is safe": second call → 200, zeros, and
    a second `erase` audit row."""
    app, pool = app_ctx
    await _seed_user(pool, 1)

    first = await _call(app, "DELETE", "/agent/me", user_id=1)
    assert first.status_code == 200, first.text
    second = await _call(app, "DELETE", "/agent/me", user_id=1)
    assert second.status_code == 200, second.text
    assert second.json()["deleted"] == {
        "sessions": 0,
        "messages": 0,
        "prefs": 0,
        "runs": 0,
        "threads": 0,
        "thread_failures": 0,
    }

    rows = await _audit_rows(pool, user_id=1)
    assert [r["action"] for r in rows] == ["erase", "erase"]
    assert rows[1]["payload"]["sessions"] == 0


async def test_checkpoint_gc_failure_is_contained(app_ctx) -> None:
    """Spec scenario "Checkpoint GC failure is contained": the failing id
    is warn-logged and counted as not deleted, the rest still run, the
    audit row is written, and the response is 200."""
    app, pool = app_ctx
    mine = await _seed_user(pool, 1)
    # Thread ids are run ids ∪ session ids (review fix); fail ONE of them.
    all_threads = sorted(set(mine["run_ids"]) | set(mine["session_ids"]))
    failing = all_threads[0]
    app.state.checkpointer.fail_for.add(failing)

    resp = await _call(app, "DELETE", "/agent/me", user_id=1)
    assert resp.status_code == 200, resp.text
    deleted = resp.json()["deleted"]
    assert deleted["runs"] == 2
    assert deleted["threads"] == 3, "only the successes count as deleted"

    # The failing id did NOT abort the remaining ids.
    assert sorted(app.state.checkpointer.calls) == all_threads
    assert set(app.state.checkpointer.calls) == set(all_threads)

    # Data is still gone; the audit row reflects reality.
    assert (await _counts_for(pool, 1))["runs"] == 0
    rows = await _audit_rows(pool, user_id=1)
    assert len(rows) == 1
    assert rows[0]["action"] == "erase"
    assert rows[0]["payload"]["threads"] == 3
    assert rows[0]["payload"]["runs"] == 2


# ---------------------------------------------------------------------------
# Audit rows — counts, not content
# ---------------------------------------------------------------------------


async def test_audit_rows_carry_counts_not_content(app_ctx) -> None:
    """Spec scenario "Export is audited with counts, not content" (export
    half) + D4 for both actions: payload keys/values are counts only and
    no subject content appears anywhere in the audit table."""
    app, pool = app_ctx
    mine = await _seed_user(pool, 1)

    export_resp = await _call(app, "GET", "/agent/me/export", user_id=1)
    assert export_resp.status_code == 200, export_resp.text
    erase_resp = await _call(app, "DELETE", "/agent/me", user_id=1)
    assert erase_resp.status_code == 200, erase_resp.text

    rows = await _audit_rows(pool, user_id=1)
    assert [r["action"] for r in rows] == ["export", "erase"]
    assert rows[0]["payload"] == {
        "sessions": 2,
        "messages": 5,
        "prefs": 1,
        "runs": 2,
        "threads": 4,  # run ids ∪ session ids (review fix)
        "audit_entries": 0,  # no audit rows existed at export time (N2)
    }
    assert rows[1]["payload"] == {
        "sessions": 2,
        "messages": 5,
        "prefs": 1,
        "runs": 2,
        "threads": 4,
        "thread_failures": 0,  # review L3
    }

    allowed_keys = {
        "sessions",
        "messages",
        "prefs",
        "runs",
        "threads",
        "thread_failures",  # review L3
        "audit_entries",  # review N2 (export payload)
    }
    for row in rows:
        assert set(row["payload"]) <= allowed_keys
        assert all(
            isinstance(v, int) for v in row["payload"].values()
        ), "payload values must be counts"
        for marker in (*mine["contents"], *mine["pref_keys"], *mine["session_ids"]):
            assert marker not in row["raw"], (
                "the audit row must not become a shadow copy of the data"
            )


# ---------------------------------------------------------------------------
# Kill switch + header enforcement
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("method", "path"),
    [("GET", "/agent/me/export"), ("DELETE", "/agent/me")],
)
async def test_disabled_returns_503_and_touches_nothing(
    app_ctx_gdpr_disabled, method, path
) -> None:
    """Spec scenario "Disabled responds 503 without touching data"."""
    app, pool = app_ctx_gdpr_disabled
    await _seed_user(pool, 1)
    before = await _counts_for(pool, 1)

    resp = await _call(app, method, path, user_id=1)
    assert resp.status_code == 503, resp.text
    body = resp.json()
    # Standard error envelope with a static safe detail.
    assert body["error"] == "GDPR endpoints disabled"
    assert "PYTHON_AGENT_GDPR_ENABLED" in body["details"]

    # No store read/write, no audit row.
    assert await _counts_for(pool, 1) == before
    assert await _audit_rows(pool) == []


async def test_missing_or_non_integer_user_header_400(app_ctx) -> None:
    """Same 400 contract as `chat_sessions.py` — fail loudly, never run as
    an implicit user 0."""
    app, _pool = app_ctx

    for method, path in (("GET", "/agent/me/export"), ("DELETE", "/agent/me")):
        missing = await _call(app, method, path, user_id=None)
        assert missing.status_code == 400, missing.text
        assert "missing X-Nyx-User-Id" in missing.json()["detail"]

        garbage = await _call(app, method, path, user_id="abc")
        assert garbage.status_code == 400, garbage.text
        assert "must be an integer" in garbage.json()["detail"]


# ---------------------------------------------------------------------------
# Audit retention iteration
# ---------------------------------------------------------------------------


async def test_audit_retention_reaps_while_gdpr_disabled(
    app_ctx_gdpr_disabled,
) -> None:
    """Spec scenarios "Retention reaps old audit rows" + "GDPR kill switch
    disturbs nothing at startup": audit retention runs independently of
    `gdpr_enabled`, reaps the backdated row, and ticks the counter."""
    app, pool = app_ctx_gdpr_disabled
    settings = app.state.settings
    assert settings.gdpr_enabled is False, "fixture precondition"

    # Backdate one audit row (user 8) past the 365-day window; user 9's
    # fresh row must survive.
    async with pool.acquire() as conn:
        await conn.execute(
            "INSERT INTO agent.data_audit_log "
            "(user_id, action, payload_summary, requested_at) VALUES "
            "($1, 'export', $2::jsonb, now() - interval '400 days')",
            8,
            _json.dumps({"sessions": 1, "messages": 0, "prefs": 0, "runs": 0, "threads": 0}),
        )
        await conn.execute(
            "INSERT INTO agent.data_audit_log "
            "(user_id, action, payload_summary) VALUES ($1, 'export', $2::jsonb)",
            9,
            _json.dumps({"sessions": 0, "messages": 0, "prefs": 0, "runs": 0, "threads": 0}),
        )

    from agent.audit import agent_audit_retention_deleted_total
    from agent.retention import RetentionCron

    cron = RetentionCron(
        app.state.run_metadata,
        None,
        None,
        settings=settings,
        audit_log=app.state.audit_log,
    )
    counter_before = agent_audit_retention_deleted_total._value.get()
    await cron._run_once()
    counter_after = agent_audit_retention_deleted_total._value.get()

    assert counter_after == counter_before + 1, (
        "the counter ticks for the reaped row while gdpr_enabled=false"
    )
    rows = await _audit_rows(pool)
    assert len(rows) == 1, "only the backdated row was reaped"
    assert rows[0]["user_id"] == 9, "the fresh row survived"


# ---------------------------------------------------------------------------
# OTel spans
# ---------------------------------------------------------------------------


def _patch_tracer(monkeypatch, module, name: str):
    """Point a module's `_tracer` at an in-memory exporter.

    The handlers call `_tracer` as a module global at request time, so
    the patch takes effect without rebuilding the app.
    """
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
        InMemorySpanExporter,
    )

    exporter = InMemorySpanExporter()
    provider = TracerProvider(resource=Resource.create({}))
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    monkeypatch.setattr(module, "_tracer", provider.get_tracer(name))
    return exporter


async def test_gdpr_span_attributes(app_ctx, monkeypatch) -> None:
    """Observability contract: both endpoint spans carry user_id + counts;
    the erase span carries the thread-failure count (never an exception)."""
    import agent.api.gdpr as gdpr_module

    exporter = _patch_tracer(monkeypatch, gdpr_module, "test-gdpr")
    app, pool = app_ctx
    mine = await _seed_user(pool, 1)
    app.state.checkpointer.fail_for.add(mine["run_ids"][0])

    assert (await _call(app, "GET", "/agent/me/export", user_id=1)).status_code == 200
    assert (await _call(app, "DELETE", "/agent/me", user_id=1)).status_code == 200

    spans = {s.name: s for s in exporter.get_finished_spans()}
    export_attrs = dict(spans["agent.gdpr.export"].attributes)
    assert export_attrs["user_id"] == 1
    assert export_attrs["sessions"] == 2
    assert export_attrs["messages"] == 5
    assert export_attrs["prefs"] == 1
    assert export_attrs["runs"] == 2
    assert export_attrs["threads"] == 4  # run ids ∪ session ids (review fix)

    erase_attrs = dict(spans["agent.gdpr.erase"].attributes)
    assert erase_attrs["user_id"] == 1
    assert erase_attrs["runs"] == 2
    assert erase_attrs["threads"] == 3  # 4 union threads - 1 failing (review fix)
    assert erase_attrs["thread_failures"] == 1
    # A contained thread failure is an attribute, never an error status.
    from opentelemetry.trace import StatusCode

    assert spans["agent.gdpr.erase"].status.status_code != StatusCode.ERROR


async def test_audit_retention_span(app_ctx_audit_short, monkeypatch) -> None:
    """Observability contract: `agent.audit.retention` with deleted_count."""
    import agent.retention as retention_module

    exporter = _patch_tracer(monkeypatch, retention_module, "test-retention")
    app, pool = app_ctx_audit_short

    async with pool.acquire() as conn:
        await conn.execute(
            "INSERT INTO agent.data_audit_log "
            "(user_id, action, payload_summary, requested_at) VALUES "
            "($1, 'erase', $2::jsonb, now() - interval '400 days')",
            9,
            _json.dumps({"sessions": 0, "messages": 0, "prefs": 0, "runs": 0, "threads": 0}),
        )

    from agent.retention import RetentionCron

    cron = RetentionCron(
        app.state.run_metadata,
        None,
        None,
        settings=app.state.settings,
        audit_log=app.state.audit_log,
    )
    await cron._run_once()

    spans = [s for s in exporter.get_finished_spans() if s.name == "agent.audit.retention"]
    assert len(spans) == 1
    assert dict(spans[0].attributes)["deleted_count"] == 1


async def test_lifespan_wires_audit_log_into_retention_cron(monkeypatch) -> None:
    """Review M5: the docstring above claims this file pins the main.py
    wiring, but the standard fixture runs with retention_enabled=False.
    Build one app WITH retention enabled and assert the cron got the
    audit store — a refactor dropping the kwarg would otherwise leave
    audit retention silently dead with a green suite."""
    async with _running_app(monkeypatch, retention_enabled=True) as (app, _pool):
        assert app.state.retention is not None
        assert app.state.retention._audit_log is app.state.audit_log


async def test_disabled_beats_missing_header(app_ctx_gdpr_disabled) -> None:
    """Review N1: the kill switch short-circuits BEFORE header parsing —
    disabled + missing header is 503, not 400. Pin the precedence so a
    future reorder is a test failure, not a silent behavior change."""
    app, _pool = app_ctx_gdpr_disabled
    resp = await _call(app, "GET", "/agent/me/export", user_id=None)
    assert resp.status_code == 503
    assert "GDPR" in resp.json()["error"]


async def test_export_includes_the_subjects_own_audit_entries(app_ctx) -> None:
    """Review N2 (decision: INCLUDE): the subject's audit rows travel in
    the export — counts-only by construction, so no privacy risk, and a
    subject-access request covers records about the data subject. The
    first export has none yet; the second sees the first's row."""
    app, pool = app_ctx
    await _seed_user(pool, 1)

    first = await _call(app, "GET", "/agent/me/export", user_id=1)
    assert first.status_code == 200
    assert first.json()["audit_entries"] == []

    second = await _call(app, "GET", "/agent/me/export", user_id=1)
    entries = second.json()["audit_entries"]
    assert [e["action"] for e in entries] == ["export"]
    assert entries[0]["payload_summary"]["sessions"] == 2
    assert "requested_at" in entries[0]
