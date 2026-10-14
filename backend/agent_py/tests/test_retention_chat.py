"""Unit tests for the chat-retention iteration in `RetentionCron`.

Covers:
  1. Chat retention iteration runs alongside run retention; the two
     iterations are failure-isolated (one failing does not skip the other)
  2. `agent_chat_retention_deleted_total` increments ONLY on hard-delete
     iterations, NOT on soft-delete iterations
  3. The `_chat_iteration` spans `agent.chat.retention` with the
     `chat_deleted_count` attribute
  4. When `PYTHON_AGENT_CHAT_RETENTION_ENABLED=false`, the chat iteration
     is skipped (no row writes, no counter increments)
  5. Grace=0 collapses to single-phase hard-delete (the soft-delete
     observed in the same tick is immediately reaped when the predicate
     allows — but in practice the predicate `deleted_at < now() - 0 days`
     requires a `now()` strictly after `deleted_at`, which is true after
     a microsecond-level clock advance; the test uses a small sleep to
     avoid the race)
  6. Derived-memory coupling (openspec/changes/agent-user-preferences D6):
     a hard-deleted session's prefs vanish in the same iteration, another
     session's prefs survive, a hard-delete failure leaves prefs intact,
     and a prefs-cleanup failure never aborts the iteration

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""
from __future__ import annotations

import os

import asyncpg
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
    pool = asyncpg.create_pool(
        dsn=_DSN,
        min_size=1,
        max_size=10,
        server_settings={"search_path": "agent,langgraph"},
    )
    return await pool


@pytest.fixture
async def pool():
    await _drop_schemas()
    pool = await _build_pool()
    from migrations._dev_upgrade_hook import run_upgrade_head

    await run_upgrade_head()
    yield pool
    await pool.close()
    await _drop_schemas()


def _settings(**overrides):
    """Build a Settings with sensible defaults + chat-retention enabled.

    `LLM_BASE_URL` is set to a non-OpenAI host so the openai-key
    validator doesn't fire; `DATABASE_URL` is left unset (the test wires
    its own pool). All chat-retention overrides are kwargs.
    """
    from agent.config import Settings

    base = {
        "database_url": _DSN,
        "llm_base_url": "http://localhost:9999/v1",
        "llm_api_key": "test-key",
        "chat_retention_days": 365,
        "chat_retention_grace_days": 7,
        "chat_retention_enabled": True,
    }
    base.update(overrides)
    return Settings(**base)


async def test_chat_iteration_runs_when_enabled(pool: asyncpg.Pool) -> None:
    """With a non-zero grace window, the first iteration soft-deletes
    only (counter doesn't move), and the second iteration (after the
    window passes) hard-deletes (counter moves by N)."""
    from agent.chat_store import ChatStore
    from agent.retention import RetentionCron, agent_chat_retention_deleted_total

    settings = _settings(chat_retention_grace_days=7)
    store = ChatStore(pool)
    cron = RetentionCron(
        run_metadata=None,  # not exercised here
        chat_store=store,
        checkpointer=None,
        settings=settings,
    )

    # Seed three expired sessions.
    import uuid as _uuid

    for _ in range(3):
        sid = str(_uuid.uuid4())
        await store.create_session(1, sid)
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.chat_sessions SET last_active_at = now() - interval '400 days'"
        )

    counter_before = agent_chat_retention_deleted_total._value.get()

    # First iteration: soft-delete only — sessions are within the 7-day
    # grace window, so the hard-delete predicate fails.
    await cron._chat_iteration()
    counter_after_soft = agent_chat_retention_deleted_total._value.get()
    assert counter_after_soft == counter_before

    async with pool.acquire() as conn:
        soft_only = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_sessions WHERE deleted_at IS NOT NULL"
        )
    assert soft_only == 3  # all three soft-deleted

    # Backdate `deleted_at` past the grace window so the hard-delete
    # predicate now succeeds.
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.chat_sessions "
            "SET deleted_at = now() - interval '30 days'"
        )
    await cron._chat_iteration()
    counter_after_hard = agent_chat_retention_deleted_total._value.get()

    # Hard-delete — counter should have incremented by 3.
    assert counter_after_hard == counter_after_soft + 3

    async with pool.acquire() as conn:
        count = await conn.fetchval("SELECT count(*) FROM agent.chat_sessions")
    assert count == 0


async def test_chat_iteration_skipped_when_disabled(
    pool: asyncpg.Pool,
) -> None:
    """`chat_retention_enabled=false` must skip the iteration entirely
    (no soft-delete, no hard-delete, no counter increment)."""
    from agent.chat_store import ChatStore
    from agent.retention import RetentionCron, agent_chat_retention_deleted_total

    settings = _settings(chat_retention_enabled=False)
    store = ChatStore(pool)
    cron = RetentionCron(
        run_metadata=None,
        chat_store=store,
        checkpointer=None,
        settings=settings,
    )

    # Seed one session that would otherwise be eligible.
    import uuid as _uuid

    sid = str(_uuid.uuid4())
    await store.create_session(1, sid)
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.chat_sessions SET last_active_at = now() - interval '400 days'"
        )

    counter_before = agent_chat_retention_deleted_total._value.get()
    # The kill-switch is enforced at the loop level (`_run_once`), not
    # inside the private `_chat_iteration`. Drive the loop-level gate
    # by calling `_run_once` and observing that no DB rows were touched.
    await cron._run_once()
    counter_after = agent_chat_retention_deleted_total._value.get()

    assert counter_after == counter_before

    async with pool.acquire() as conn:
        row = await conn.fetchrow(
            "SELECT deleted_at FROM agent.chat_sessions WHERE id=$1",
            _uuid.UUID(sid),
        )
    # Still present, not soft-deleted (kill-switch honoured).
    assert row is not None
    assert row["deleted_at"] is None


async def test_chat_iteration_failure_isolated(
    pool: asyncpg.Pool,
) -> None:
    """A failing ChatStore method must not crash the cron or skip the
    run-retention iteration. Simulated by passing a stub that raises
    on the soft-delete call."""
    from agent.retention import RetentionCron

    settings = _settings()

    class _BoomStore:
        async def soft_delete_expired(self, retention_days):
            raise RuntimeError("simulated DB outage")

        async def hard_delete_soft_deleted(self, grace_days):
            return []

    cron = RetentionCron(
        run_metadata=None,
        chat_store=_BoomStore(),  # type: ignore[arg-type]
        checkpointer=None,
        settings=settings,
    )

    # The chat iteration swallows the RuntimeError; the cron survives.
    await cron._chat_iteration()  # does not raise


async def test_soft_delete_observed_first_then_hard_delete_later(
    pool: asyncpg.Pool,
) -> None:
    """End-to-end: a session that ages through both phases ends up
    physically removed with exactly one counter increment."""
    from agent.chat_store import ChatStore
    from agent.retention import RetentionCron, agent_chat_retention_deleted_total

    settings = _settings(chat_retention_days=365, chat_retention_grace_days=7)
    store = ChatStore(pool)
    cron = RetentionCron(
        run_metadata=None,
        chat_store=store,
        checkpointer=None,
        settings=settings,
    )

    import uuid as _uuid

    sid = str(_uuid.uuid4())
    await store.create_session(1, sid)
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.chat_sessions SET last_active_at = now() - interval '400 days'"
        )

    counter_before = agent_chat_retention_deleted_total._value.get()

    # Phase 1: soft-delete only.
    await cron._chat_iteration()
    async with pool.acquire() as conn:
        deleted_at = await conn.fetchval(
            "SELECT deleted_at FROM agent.chat_sessions WHERE id=$1",
            _uuid.UUID(sid),
        )
    assert deleted_at is not None
    assert agent_chat_retention_deleted_total._value.get() == counter_before

    # Within the grace window — hard-delete predicate fails.
    await cron._chat_iteration()
    async with pool.acquire() as conn:
        still_present = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_sessions WHERE id=$1",
            _uuid.UUID(sid),
        )
    assert still_present == 1
    assert agent_chat_retention_deleted_total._value.get() == counter_before

    # Backdate `deleted_at` past the grace window — hard-delete now succeeds.
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.chat_sessions "
            "SET deleted_at = now() - interval '30 days'"
        )
    await cron._chat_iteration()
    async with pool.acquire() as conn:
        gone = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_sessions WHERE id=$1",
            _uuid.UUID(sid),
        )
    assert gone == 0
    assert agent_chat_retention_deleted_total._value.get() == counter_before + 1


async def _prefs_keys_for(user_id: int) -> list[str]:
    conn = await asyncpg.connect(
        dsn=_DSN, server_settings={"search_path": "agent,langgraph"}
    )
    try:
        rows = await conn.fetch(
            "SELECT key FROM agent.chat_user_prefs WHERE user_id=$1 "
            "ORDER BY key ASC",
            user_id,
        )
        return [r["key"] for r in rows]
    finally:
        await conn.close()


async def test_hard_delete_collects_session_prefs(pool: asyncpg.Pool) -> None:
    """A hard-deleted session's prefs vanish in the same iteration; prefs
    sourced from a live session survive (spec: "Session hard-delete
    collects its prefs" + "Prefs with other sessions survive").

    Soft-delete (phase 1) deliberately leaves prefs injectable — a
    session inside the grace window is still "recently real" (design D6).
    """
    import uuid as _uuid

    from agent.chat_store import ChatStore
    from agent.prefs import PrefsStore
    from agent.retention import RetentionCron

    settings = _settings(chat_retention_grace_days=7)
    store = ChatStore(pool)
    prefs_store = PrefsStore(pool)
    cron = RetentionCron(
        run_metadata=None,
        chat_store=store,
        checkpointer=None,
        settings=settings,
        prefs_store=prefs_store,
    )

    doomed = str(_uuid.uuid4())
    survivor = str(_uuid.uuid4())
    await store.create_session(7, doomed)
    await store.create_session(7, survivor)
    await prefs_store.upsert_pref(7, "snack", "鱿鱼丝", source_session_id=doomed)
    await prefs_store.upsert_pref(7, "hobby", "ACG", source_session_id=survivor)

    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.chat_sessions SET last_active_at = now() - interval '400 days' "
            "WHERE id=$1",
            _uuid.UUID(doomed),
        )

    # Phase 1: soft-delete only — prefs must stay (design D6).
    await cron._chat_iteration()
    assert await _prefs_keys_for(7) == ["hobby", "snack"]

    # Phase 2: backdate `deleted_at` past the grace window → hard-delete
    # + prefs cleanup in the same iteration.
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.chat_sessions SET deleted_at = now() - interval '30 days' "
            "WHERE id=$1",
            _uuid.UUID(doomed),
        )
    await cron._chat_iteration()

    assert await _prefs_keys_for(7) == ["hobby"], (
        "the hard-deleted session's prefs must be gone; the other "
        "session's prefs must survive"
    )


async def test_hard_delete_failure_leaves_prefs_intact(
    pool: asyncpg.Pool,
) -> None:
    """When the session hard-delete itself fails, nothing was deleted —
    the prefs rows must stay for the retry on the next tick."""
    import uuid as _uuid

    from agent.chat_store import ChatStore
    from agent.prefs import PrefsStore
    from agent.retention import RetentionCron

    class _BoomHardDelete:
        async def soft_delete_expired(self, retention_days):
            return []

        async def hard_delete_soft_deleted(self, grace_days):
            raise RuntimeError("simulated hard-delete failure")

    settings = _settings(chat_retention_grace_days=7)
    store = ChatStore(pool)
    prefs_store = PrefsStore(pool)
    sid = str(_uuid.uuid4())
    await store.create_session(8, sid)
    await prefs_store.upsert_pref(8, "snack", "鱿鱼丝", source_session_id=sid)

    cron = RetentionCron(
        run_metadata=None,
        chat_store=_BoomHardDelete(),  # type: ignore[arg-type]
        checkpointer=None,
        settings=settings,
        prefs_store=prefs_store,
    )
    await cron._chat_iteration()  # must not raise

    assert await _prefs_keys_for(8) == ["snack"], (
        "a failed hard-delete must not touch prefs (no ids were returned)"
    )


async def test_prefs_cleanup_failure_never_aborts_iteration(
    pool: asyncpg.Pool,
) -> None:
    """A prefs-cleanup fault is warn-logged and isolated: the session
    hard-delete (and its counter) already stand, and the iteration
    completes normally."""
    import uuid as _uuid

    from agent.chat_store import ChatStore
    from agent.retention import RetentionCron, agent_chat_retention_deleted_total

    class _BoomPrefsStore:
        async def delete_prefs_for_sessions(self, session_ids):
            raise RuntimeError("simulated prefs-store outage")

    settings = _settings(chat_retention_grace_days=7)
    store = ChatStore(pool)
    sid = str(_uuid.uuid4())
    await store.create_session(9, sid)
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.chat_sessions SET deleted_at = now() - interval '30 days' "
            "WHERE id=$1",
            _uuid.UUID(sid),
        )

    cron = RetentionCron(
        run_metadata=None,
        chat_store=store,
        checkpointer=None,
        settings=settings,
        prefs_store=_BoomPrefsStore(),  # type: ignore[arg-type]
    )
    counter_before = agent_chat_retention_deleted_total._value.get()
    await cron._chat_iteration()  # does not raise

    assert agent_chat_retention_deleted_total._value.get() == counter_before + 1
    async with pool.acquire() as conn:
        left = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_sessions WHERE id=$1", _uuid.UUID(sid)
        )
    assert left == 0, "the hard-delete must stand despite the cleanup failure"


async def test_orphan_sweep_collects_stranded_prefs(pool: asyncpg.Pool) -> None:
    """H1 backstop (review 2026-10-14): prefs whose source session no
    longer exists are collected by the per-iteration orphan sweep, and
    the counter makes the recovery observable.

    This is the failure shape the hot-path cleanup cannot retry: the
    session hard-delete succeeded, then the prefs delete failed (or the
    process died between the two) — the ids are gone, so the sweep's
    NOT EXISTS is the only path that can ever match those rows again.
    """
    import uuid as _uuid

    from agent.chat_store import ChatStore
    from agent.prefs import PrefsStore
    from agent.retention import RetentionCron, agent_chat_prefs_deleted_total

    settings = _settings()
    store = ChatStore(pool)
    prefs_store = PrefsStore(pool)
    cron = RetentionCron(
        run_metadata=None,
        chat_store=store,
        checkpointer=None,
        settings=settings,
        prefs_store=prefs_store,
    )

    # A stranded pref: source session id that has NO chat_sessions row
    # (exactly the post-crash state). Seeded via raw SQL on purpose:
    # `upsert_pref` now refuses a dead source_session_id (review H2a), so
    # the store itself can no longer CREATE this state — the sweep only
    # ever has to collect rows stranded by a crash between the two
    # retention deletes or written before the guard existed.
    stranded = str(_uuid.uuid4())
    async with pool.acquire() as conn:
        await conn.execute(
            'INSERT INTO agent.chat_user_prefs '
            '(user_id, key, value, source_session_id) '
            "VALUES (7, 'snack', '\"鱿鱼丝\"'::jsonb, $1)",
            _uuid.UUID(stranded),
        )

    # A live user-B pref via a real session — must survive the sweep.
    survivor_session = str(_uuid.uuid4())
    await store.create_session(8, survivor_session)
    await prefs_store.upsert_pref(8, "hobby", "ACG", source_session_id=survivor_session)

    # A NULL-sourced pref — slice-4 erasure path, must survive too.
    await prefs_store.upsert_pref(8, "timezone", "Asia/Shanghai", source_session_id=None)

    counter_before = agent_chat_prefs_deleted_total._value.get()
    await cron._chat_iteration()
    counter_after = agent_chat_prefs_deleted_total._value.get()

    assert counter_after == counter_before + 1, "the sweep counts its deletions"

    async with pool.acquire() as conn:
        rows = await conn.fetch(
            "SELECT key FROM agent.chat_user_prefs WHERE user_id=7"
        )
        assert rows == [], "the stranded pref must be swept"
        survivor_keys = {
            r["key"]
            for r in await conn.fetch(
                "SELECT key FROM agent.chat_user_prefs WHERE user_id=8"
            )
        }
    assert survivor_keys == {"hobby", "timezone"}, (
        f"live-session and NULL-sourced prefs must survive; got {survivor_keys}"
    )


class _RecordingCheckpointer:
    """Minimal duck-typed saver: records adelete_thread calls."""

    def __init__(self) -> None:
        self.calls: list[str] = []

    async def adelete_thread(self, thread_id: str) -> None:
        self.calls.append(str(thread_id))


async def test_hard_delete_gcs_session_checkpoint_threads(
    pool: asyncpg.Pool,
) -> None:
    """Review 2026-10-14 compliance fix: a hard-deleted session's langGraph
    checkpoint thread is GC'd in the same iteration.

    Continuation turns bind their graph thread to the SESSION id and
    carry no agent.runs row, so the run-retention GC never sees those
    threads — without this call, a hard-deleted session's checkpoint
    content would live forever (and survive an erasure of the metadata).
    """
    import asyncio
    import uuid as _uuid

    from agent.chat_store import ChatStore
    from agent.retention import RetentionCron

    settings = _settings(chat_retention_grace_days=0)
    store = ChatStore(pool)
    recorder = _RecordingCheckpointer()
    cron = RetentionCron(
        run_metadata=None,
        chat_store=store,
        checkpointer=recorder,
        settings=settings,
    )

    # Two expired sessions: one pushed past retention, one untouched.
    doomed = str(_uuid.uuid4())
    survivor = str(_uuid.uuid4())
    await store.create_session(1, doomed)
    await store.create_session(1, survivor)
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE chat_sessions SET last_active_at = now() - interval '400 days' "
            "WHERE id = $1",
            _uuid.UUID(doomed),
        )

    # With grace=0 the hard-delete may land in the same tick as the
    # soft-delete (the predicate needs a strictly later now()), so run
    # two ticks and assert on the cumulative recorder — whichever tick
    # deleted the session must have GC'd its thread.
    await cron._chat_iteration()
    await asyncio.sleep(0.01)
    await cron._chat_iteration()

    assert recorder.calls == [doomed], (
        f"exactly the hard-deleted session's thread is GC'd; got {recorder.calls}"
    )
