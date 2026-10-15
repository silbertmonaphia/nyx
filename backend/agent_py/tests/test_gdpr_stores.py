"""Store-layer tests for GDPR erasure + export (openspec/changes/agent-gdpr-compliance).

Covers the schema/store half of the change (the endpoint half lives in
`tests/test_gdpr_endpoints.py`):

  1. `ChatStore.delete_all_for_user` — soft-deleted sessions AND their
     messages are erased (retention is not a precondition for erasure),
     ALL prefs go including NULL-source rows, exact per-table counts are
     returned, a second user's rows are untouched (counts before/after),
     and a second erase is all zeros.
  2. `ChatStore.list_all_sessions_for_user` / `list_all_messages_for_user`
     — soft-deleted rows INCLUDED (export completeness), deterministic
     `(started_at, id)` / `(created_at, id)` ordering, `tool_calls`
     decoded, cross-user exclusion.
  3. `PrefsStore.list_all_prefs_for_user` — unpaginated, key-ASC,
     user-scoped.
  4. `RunMetadata.delete_all_for_user` — returns the run ids (checkpoint
     GC work-list), ignores status, foreign runs untouched, idempotent.
  5. `RunMetadata.list_all_runs_for_user` — unpaginated, oldest-first,
     user-scoped.
  6. `AuditLog` — `record` → `list_for_user` readback (`payload_summary`
     decoded dict, oldest-first), `retention_delete` reaps only rows past
     the window and increments `agent_audit_retention_deleted_total`.
  7. Settings — `gdpr_enabled` / `audit_retention_days` defaults, the
     below-365 refusal (exact message), disabled-flag load.

DB-backed tests self-skip without `TEST_ASYNC_PG_DSN` (same pool-fixture
posture as `test_chat_store.py`); the settings tests are pure unit tests
and always run. The `nyx_test`-only guard lives in `conftest.py`.
"""
from __future__ import annotations

import os
import uuid as _uuid

import asyncpg
import pytest

from agent.config import Settings

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

_DB_ONLY = pytest.mark.skipif(
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
    pool = asyncpg.create_pool(
        dsn=_DSN,
        min_size=1,
        max_size=10,
        # Unqualified `chat_sessions` / `chat_messages` / `chat_user_prefs` /
        # `runs` / `data_audit_log` resolve to `agent.*` — same
        # server_settings posture as `test_chat_store.py`.
        server_settings={"search_path": "agent,langgraph"},
    )
    return await pool


@pytest.fixture
async def pool():
    """Isolated pool per test: drop + rebuild both schemas, then migrate to
    head (which creates `data_audit_log` via alembic 0006).

    The guard here is load-bearing beyond `_DB_ONLY`: a test that reaches
    this fixture WITHOUT the marker (or a future one added without it)
    must skip, not blackhole on a real connect against a DSN-less
    environment (found 2026-10-14: a ~60 s connect timeout from an
    unmarked test).
    """
    if not _DSN:
        pytest.skip("TEST_ASYNC_PG_DSN not set; needs a live Postgres")
    await _drop_schemas()
    pool = await _build_pool()
    from migrations._dev_upgrade_hook import run_upgrade_head

    await run_upgrade_head()
    yield pool
    await pool.close()
    await _drop_schemas()


async def _table_counts(pool: asyncpg.Pool, user_id: int) -> dict[str, int]:
    """Row counts per user-owned table for `user_id` (isolation snapshots)."""
    async with pool.acquire() as conn:
        return {
            "sessions": await conn.fetchval(
                "SELECT count(*) FROM agent.chat_sessions WHERE user_id=$1",
                user_id,
            ),
            "messages": await conn.fetchval(
                "SELECT count(*) FROM agent.chat_messages m "
                "JOIN agent.chat_sessions s ON s.id = m.session_id "
                "WHERE s.user_id=$1",
                user_id,
            ),
            "prefs": await conn.fetchval(
                "SELECT count(*) FROM agent.chat_user_prefs WHERE user_id=$1",
                user_id,
            ),
        }


# --- ChatStore.delete_all_for_user (erasure) --------------------------------


@_DB_ONLY
async def test_delete_all_for_user_erases_everything_including_soft_deleted(
    pool: asyncpg.Pool,
) -> None:
    """The highest-blast-radius contract: erasure removes soft-deleted
    sessions + their messages, ALL prefs (NULL-source included), returns
    exact counts, and never touches a second user's rows."""
    from agent.chat_store import ChatStore
    from agent.prefs import PrefsStore

    chat_store = ChatStore(pool)
    prefs_store = PrefsStore(pool)
    user_a, user_b = 42, 43

    # User A: a live session and a session soft-deleted after receiving
    # messages (simulates the user-delete path AND retention phase 1 —
    # neither is a precondition for erasure).
    live_id = str(_uuid.uuid4())
    soft_id = str(_uuid.uuid4())
    await chat_store.create_session(user_a, live_id)
    await chat_store.create_session(user_a, soft_id)
    await chat_store.record_message(live_id, user_a, role="user", content="a1")
    await chat_store.record_message(live_id, user_a, role="assistant", content="a2")
    await chat_store.record_message(soft_id, user_a, role="user", content="a3")
    assert await chat_store.soft_delete_session(soft_id, user_a) is True

    # Prefs: one session-sourced, one NULL-source (the retention path
    # deliberately skips NULL-source rows; erasure must not).
    await prefs_store.upsert_pref(user_a, "snack", "鱿鱼丝", source_session_id=live_id)
    await prefs_store.upsert_pref(user_a, "editor", "vim", source_session_id=None)

    # User B: the untouched control.
    b_session = str(_uuid.uuid4())
    await chat_store.create_session(user_b, b_session)
    await chat_store.record_message(b_session, user_b, role="user", content="b1")
    await prefs_store.upsert_pref(user_b, "snack", "seaweed", source_session_id=b_session)
    b_before = await _table_counts(pool, user_b)

    counts = await chat_store.delete_all_for_user(user_a)

    # Exact counts: both sessions (soft-deleted included), all three
    # messages (the soft-deleted session's one included), both prefs.
    assert {k: v for k, v in counts.items() if k != "session_ids"} == {
        "messages": 3,
        "sessions": 2,
        "prefs": 2,
    }
    # The deleted session ids travel alongside the counts: they double as
    # langGraph thread ids so the erase endpoint can GC session-keyed
    # threads (review 2026-10-14 compliance fix).
    assert sorted(counts["session_ids"]) == sorted([live_id, soft_id])

    # Everything of A's is gone across all three tables.
    assert await _table_counts(pool, user_a) == {
        "sessions": 0,
        "messages": 0,
        "prefs": 0,
    }
    # B's rows are byte-for-byte untouched (counts compared before/after).
    assert await _table_counts(pool, user_b) == b_before
    assert b_before == {"sessions": 1, "messages": 1, "prefs": 1}

    # Idempotent: a second erase is all zeros (empty id list), no error.
    assert await chat_store.delete_all_for_user(user_a) == {
        "messages": 0,
        "sessions": 0,
        "prefs": 0,
        "session_ids": [],
    }


# --- ChatStore export reads --------------------------------------------------


@_DB_ONLY
async def test_list_all_sessions_and_messages_include_soft_deleted(
    pool: asyncpg.Pool,
) -> None:
    """Export completeness: soft-deleted rows are part of the dump, the
    ordering is deterministic, `tool_calls` is decoded, and a second
    user's rows never appear."""
    from agent.chat_store import ChatStore

    store = ChatStore(pool)
    user_a, user_b = 7, 8

    live_id = str(_uuid.uuid4())
    soft_id = str(_uuid.uuid4())
    await store.create_session(user_a, live_id, title="live")
    await store.create_session(user_a, soft_id, title="soft")
    live_msg = await store.record_message(
        live_id,
        user_a,
        role="assistant",
        content="live-1",
        tool_calls=[{"name": "list_feeds", "args": {"n": 1}}],
    )
    soft_msg = await store.record_message(soft_id, user_a, role="user", content="soft-1")
    assert await store.soft_delete_session(soft_id, user_a) is True

    # Foreign rows must be invisible to both reads.
    b_session = str(_uuid.uuid4())
    await store.create_session(user_b, b_session)
    await store.record_message(b_session, user_b, role="user", content="b-1")

    # Pin the ordering deterministically: equal `started_at` (→ id ASC
    # tiebreak) and equal `created_at` (→ id ASC tiebreak), instead of
    # relying on clock races between per-statement transactions.
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.chat_sessions SET started_at = '2026-01-01T00:00:00+00' "
            "WHERE user_id = $1",
            user_a,
        )
        await conn.execute(
            "UPDATE agent.chat_messages SET created_at = '2026-01-01T00:00:00+00' "
            "WHERE id = ANY($1::bigint[])",
            [live_msg, soft_msg],
        )

    sessions = await store.list_all_sessions_for_user(user_a)
    # Both rows, soft-deleted included, ordered (started_at ASC, id ASC).
    assert [str(s.id) for s in sessions] == sorted(
        [live_id, soft_id], key=_uuid.UUID
    )
    assert {str(s.id) for s in sessions if s.deleted_at is not None} == {soft_id}

    messages = await store.list_all_messages_for_user(user_a)
    # Both messages — the soft-deleted session's one included — ordered
    # (created_at ASC, id ASC).
    assert [m.id for m in messages] == [live_msg, soft_msg]
    assert [m.content for m in messages] == ["live-1", "soft-1"]
    # jsonb `tool_calls` decodes like `list_messages` (array or None).
    assert messages[0].tool_calls == [{"name": "list_feeds", "args": {"n": 1}}]
    assert messages[1].tool_calls is None


# --- PrefsStore export read --------------------------------------------------


@_DB_ONLY
async def test_list_all_prefs_for_user_key_ordered(
    pool: asyncpg.Pool,
) -> None:
    """Unpaginated, key-ASC, user-scoped; jsonb values decoded."""
    from agent.prefs import PrefsStore

    store = PrefsStore(pool)
    user_a, user_b = 3, 4
    await store.upsert_pref(user_a, "snack", "鱿鱼丝")
    await store.upsert_pref(user_a, "editor", "vim")
    await store.upsert_pref(user_a, "music_genre", ["city_pop"])
    await store.upsert_pref(user_b, "snack", "foreign")

    prefs = await store.list_all_prefs_for_user(user_a)
    assert [p.key for p in prefs] == ["editor", "music_genre", "snack"]
    assert prefs[0].value == "vim"
    assert prefs[1].value == ["city_pop"]
    assert [p.value for p in prefs if p.key == "snack"] == ["鱿鱼丝"]


# --- RunMetadata erasure + export reads --------------------------------------


@_DB_ONLY
async def test_run_metadata_delete_all_for_user_returns_ids(
    pool: asyncpg.Pool,
) -> None:
    """The returned ids are the checkpoint-GC work-list; foreign runs and
    the second call's idempotency are pinned."""
    from agent.run_metadata import RunMetadata

    metadata = RunMetadata(pool)
    user_a, user_b = 11, 12
    run_a1 = await metadata.create_run(user_a)
    run_a2 = await metadata.create_run(user_a)
    # A terminal row must be erased too (erasure ignores status).
    await metadata.mark_completed(run_a2, total_tokens=5)
    run_b = await metadata.create_run(user_b)

    ids = await metadata.delete_all_for_user(user_a)
    assert sorted(ids) == sorted([run_a1, run_a2])

    # Foreign run untouched; second call is an empty, no-error delete.
    assert await metadata.get_run(run_b, user_b) is not None
    assert await metadata.delete_all_for_user(user_a) == []


@_DB_ONLY
async def test_list_all_runs_for_user_oldest_first_and_scoped(
    pool: asyncpg.Pool,
) -> None:
    from agent.run_metadata import RunMetadata

    metadata = RunMetadata(pool)
    user_a, user_b = 21, 22
    run_1 = await metadata.create_run(user_a)
    run_2 = await metadata.create_run(user_a)
    run_3 = await metadata.create_run(user_a)
    await metadata.create_run(user_b)

    # Backdate so the ascending order is pinned without clock races.
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.runs SET started_at = CASE id "
            "WHEN $2::uuid THEN now() - interval '3 days' "
            "WHEN $3::uuid THEN now() - interval '2 days' "
            "WHEN $4::uuid THEN now() - interval '1 day' "
            "ELSE started_at END "
            "WHERE user_id = $1",
            user_a,
            run_1,
            run_2,
            run_3,
        )

    runs = await metadata.list_all_runs_for_user(user_a)
    assert [str(r.id) for r in runs] == [run_1, run_2, run_3]
    assert all(r.user_id == user_a for r in runs)


# --- AuditLog ----------------------------------------------------------------


@_DB_ONLY
async def test_audit_record_and_readback(pool: asyncpg.Pool) -> None:
    """record → list_for_user: oldest-first, decoded dict payload, no
    cross-user leak, counts-only values."""
    from agent.audit import AuditLog

    audit = AuditLog(pool)
    user_a, user_b = 77, 78
    await audit.record(
        user_a, "export", {"sessions": 2, "messages": 5, "prefs": 1, "runs": 2}
    )
    await audit.record(
        user_a,
        "erase",
        {"sessions": 2, "messages": 5, "prefs": 1, "runs": 2, "threads": 2},
    )
    await audit.record(user_a, "export", {})
    await audit.record(user_b, "export", {"sessions": 9})

    rows = await audit.list_for_user(user_a)
    # Oldest-first; user B's row absent.
    assert [r.action for r in rows] == ["export", "erase", "export"]
    assert all(r.user_id == user_a for r in rows)

    # payload_summary round-trips as a decoded dict (not a JSON string).
    assert isinstance(rows[0].payload_summary, dict)
    assert rows[0].payload_summary == {
        "sessions": 2,
        "messages": 5,
        "prefs": 1,
        "runs": 2,
    }
    assert rows[1].payload_summary["threads"] == 2
    assert rows[2].payload_summary == {}
    # Counts-not-content: every recorded value is an int.
    assert all(isinstance(v, int) for v in rows[0].payload_summary.values())
    assert rows[0].requested_at is not None
    assert rows[0].id < rows[1].id < rows[2].id


@_DB_ONLY
async def test_audit_retention_delete_reaps_old_rows_and_counts(
    pool: asyncpg.Pool,
) -> None:
    """Only rows past the window are reaped; the store-owned counter
    increments by the reaped count (and stands still on a zero reap)."""
    from agent.audit import AuditLog, agent_audit_retention_deleted_total

    audit = AuditLog(pool)
    user_id = 91
    await audit.record(user_id, "export", {"sessions": 1})  # to be reaped
    await audit.record(user_id, "erase", {"sessions": 1})  # inside the window
    rows = await audit.list_for_user(user_id)
    old_id = rows[0].id

    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.data_audit_log "
            "SET requested_at = now() - interval '400 days' WHERE id = $1",
            old_id,
        )

    counter_before = agent_audit_retention_deleted_total._value.get()
    deleted = await audit.retention_delete(365)
    counter_after = agent_audit_retention_deleted_total._value.get()

    assert deleted == 1
    assert counter_after == counter_before + 1

    # Only the old row is gone; the fresh row survives.
    remaining = await audit.list_for_user(user_id)
    assert [r.action for r in remaining] == ["erase"]

    # Nothing left to reap → zero, and the counter does not move.
    assert await audit.retention_delete(365) == 0
    assert agent_audit_retention_deleted_total._value.get() == counter_after


# --- settings (agent-python-service delta) -----------------------------------


def test_gdpr_settings_defaults(monkeypatch):
    """Spec scenario "Default values applied when env unset":
    `PYTHON_AGENT_GDPR_ENABLED=true` and
    `PYTHON_AGENT_AUDIT_RETENTION_DAYS=1095`."""
    monkeypatch.delenv("PYTHON_AGENT_GDPR_ENABLED", raising=False)
    monkeypatch.delenv("PYTHON_AGENT_AUDIT_RETENTION_DAYS", raising=False)
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.gdpr_enabled is True
    assert s.audit_retention_days == 1095


def test_gdpr_disabled_flag_loads(monkeypatch):
    """Spec scenario "GDPR kill switch disturbs nothing at startup":
    gdpr_enabled=False is valid configuration and leaves the audit
    retention settings untouched."""
    monkeypatch.delenv("PYTHON_AGENT_GDPR_ENABLED", raising=False)
    monkeypatch.delenv("PYTHON_AGENT_AUDIT_RETENTION_DAYS", raising=False)
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1", gdpr_enabled=False)
    assert s.gdpr_enabled is False
    assert s.audit_retention_days == 1095


def test_below_floor_audit_retention_refused(monkeypatch):
    """Spec scenario "Below-floor audit retention refuses startup": the
    exact ValueError message is part of the contract."""
    monkeypatch.delenv("PYTHON_AGENT_AUDIT_RETENTION_DAYS", raising=False)
    with pytest.raises(ValueError) as excinfo:
        Settings(
            _env_file=None,
            llm_base_url="http://vllm:8000/v1",
            audit_retention_days=100,
        )
    assert "PYTHON_AGENT_AUDIT_RETENTION_DAYS must be >= 365; got 100" in str(
        excinfo.value
    )


def test_below_floor_audit_retention_refused_via_env(monkeypatch):
    """Same refusal through the env var (pins the `PYTHON_AGENT_` name)."""
    monkeypatch.setenv("PYTHON_AGENT_AUDIT_RETENTION_DAYS", "100")
    with pytest.raises(ValueError) as excinfo:
        Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert "PYTHON_AGENT_AUDIT_RETENTION_DAYS must be >= 365; got 100" in str(
        excinfo.value
    )


def test_boundary_audit_retention_accepted(monkeypatch):
    """365 is the floor, not an off-by-one bug — the exact boundary loads."""
    monkeypatch.delenv("PYTHON_AGENT_AUDIT_RETENTION_DAYS", raising=False)
    s = Settings(
        _env_file=None,
        llm_base_url="http://vllm:8000/v1",
        audit_retention_days=365,
    )
    assert s.audit_retention_days == 365


@_DB_ONLY
async def test_upsert_pref_refuses_a_dead_source_session(pool: asyncpg.Pool) -> None:
    """Review H2a (2026-10-14): a prefs write whose source session no
    longer exists is skipped — the erasure-durability guard.

    The column deliberately has no FK, so without the in-SQL EXISTS
    guard an extraction task racing `DELETE /agent/me` (or a retention
    hard-delete) could resurrect a prefs row for a just-erased subject
    pointing at a deleted session (reproduced in the review).
    """
    from agent.prefs import PrefsStore

    store = PrefsStore(pool)
    dead_session = str(_uuid.uuid4())

    # No chat_sessions row for `dead_session` → write is skipped.
    await store.upsert_pref(9, "snack", "鱿鱼丝", source_session_id=dead_session)
    assert await store.load_prefs(9, limit=10) == []

    # A live session writes normally (guard is not over-broad).
    live = str(_uuid.uuid4())
    await _insert_session(pool, live, user_id=9)
    await store.upsert_pref(9, "snack", "鱿鱼丝", source_session_id=live)
    rows = await store.load_prefs(9, limit=10)
    assert [(p.key, p.value) for p in rows] == [("snack", "鱿鱼丝")]

    # NULL-source writes still work (the erasure path relies on them).
    await store.upsert_pref(9, "editor", "vim", source_session_id=None)
    assert {p.key for p in await store.load_prefs(9, limit=10)} == {"snack", "editor"}


async def test_record_message_on_erased_session_raises_store_unavailable(
    pool: asyncpg.Pool,
) -> None:
    """Review H2b: a message write racing an erasure (FK violation) is
    translated to `ChatStoreUnavailable` so the run's existing
    best-effort handlers contain it — it must not escape as a raw
    `ForeignKeyViolationError` (reproduced in the review as an unhandled
    ASGI error after the last SSE chunk)."""
    from agent.chat_store import ChatStore, ChatStoreUnavailable

    store = ChatStore(pool)
    sid = str(_uuid.uuid4())
    await _insert_session(pool, sid, user_id=10)
    await store.delete_all_for_user(10)

    with pytest.raises(ChatStoreUnavailable):
        await store.record_message(sid, 10, role="assistant", content="late")


async def test_audit_record_rejects_non_int_payloads(pool: asyncpg.Pool) -> None:
    """Review L2: the counts-not-content contract is enforced at
    runtime — a str value (e.g. a leaked uuid) raises instead of landing
    in the evidence table."""
    from agent.audit import AuditLog

    audit = AuditLog(pool)
    with pytest.raises(ValueError, match="counts-not-content"):
        await audit.record(11, "erase", {"sessions": "not-an-int"})  # type: ignore[dict-item]
    # Bools are ints in Python — also rejected.
    with pytest.raises(ValueError, match="counts-not-content"):
        await audit.record(11, "erase", {"sessions": True})  # type: ignore[dict-item]


async def _insert_session(pool: asyncpg.Pool, session_id: str, *, user_id: int) -> None:
    async with pool.acquire() as conn:
        await conn.execute(
            "INSERT INTO agent.chat_sessions (id, user_id, title) VALUES ($1, $2, '')",
            _uuid.UUID(session_id),
            user_id,
        )
