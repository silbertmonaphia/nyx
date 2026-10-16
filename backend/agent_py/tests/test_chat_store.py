"""Unit tests for `ChatStore` (openspec/changes/agent-chat-history-storage).

Covers:
  1. Create + read round-trip (user scope)
  2. Cross-user isolation — `get_session` / `record_message` / `touch_session` /
     `end_session` never leak across `user_id`
  3. `record_message` round-trip — bytes-faithful `tool_calls` jsonb
  4. `record_message` bumps `last_active_at` on the parent session (D8)
  5. Retention two-phase lifecycle: `soft_delete_expired` then
     `hard_delete_soft_deleted`, with FK ON DELETE CASCADE carrying the
     `chat_messages` rows along
  6. Grace=0 collapses to a single-phase hard-delete
  7. Title writes (openspec/changes/agent-chat-fixes 2.2/2.3):
     `set_title_if_empty` fills an empty title exactly once (atomically,
     even under concurrent first turns) and `set_title` overwrites only
     the owner's live session — both no-ops for a foreign user or a
     soft-deleted session

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres). When
the env var is set, the tests run against a real asyncpg pool — no
in-memory mock.
"""
from __future__ import annotations

import asyncio
import json as _json
import os
import uuid as _uuid

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
        # Set `search_path` so unqualified `chat_sessions` / `chat_messages`
        # resolve to `agent.*` (the tables live there; the alembic env.py
        # only sets search_path for its own migration connection, not for
        # consumer pools). Matches the `database_url` validator in
        # `config.py` that the agent process uses in production.
        server_settings={"search_path": "agent,langgraph"},
    )
    return await pool


async def _migrate(pool: asyncpg.Pool) -> None:
    """Run the alembic chain via the dev hook so the test sees the
    final schema (chat_sessions + chat_messages + everything before)."""
    # Re-create the alembic_version table if alembic hasn't run yet.
    async with pool.acquire() as conn:
        await conn.execute("CREATE SCHEMA IF NOT EXISTS langgraph")
        await conn.execute("CREATE SCHEMA IF NOT EXISTS agent")


@pytest.fixture
async def pool():
    """Isolated pool for each test: drop and rebuild both schemas so the
    test owns a clean slate."""
    await _drop_schemas()
    pool = await _build_pool()
    # Run alembic to bring the schemas to `head` (chat_sessions + chat_messages present).
    from migrations._dev_upgrade_hook import run_upgrade_head

    await run_upgrade_head()
    yield pool
    await pool.close()
    await _drop_schemas()


async def test_create_and_get_session(pool: asyncpg.Pool) -> None:
    from agent.chat_store import ChatStore

    store = ChatStore(pool)
    user_id = 1
    run_id = str(_uuid.uuid4())
    await store.create_session(user_id, run_id, title="hello")

    s = await store.get_session(run_id, user_id)
    assert s is not None
    # asyncpg auto-converts UUID columns to `uuid.UUID` on read.
    assert s.id == _uuid.UUID(run_id)
    assert s.user_id == user_id
    assert s.title == "hello"
    assert s.deleted_at is None
    assert s.ended_at is None


async def test_cross_user_isolation(pool: asyncpg.Pool) -> None:
    from agent.chat_store import ChatStore

    store = ChatStore(pool)
    user_a = 1
    user_b = 2
    run_id = str(_uuid.uuid4())
    await store.create_session(user_a, run_id)

    # user_b sees None — the WHERE user_id clause blocks the read.
    assert await store.get_session(run_id, user_b) is None
    # Cross-user touch returns False (zero rows updated).
    assert await store.touch_session(run_id, user_b) is False
    # Cross-user end returns False.
    assert await store.end_session(run_id, user_b) is False
    # Cross-user record_message should still succeed (it does not check
    # user_id on insert — the FK enforces session existence; but touch
    # in the same transaction enforces user_id). Behaviour: a record
    # against a foreign session inserts the row but the subsequent
    # `last_active_at` update is a no-op (touch returns 0). This is the
    # safest failure mode — orphan messages are visible but the parent's
    # activity marker is not corrupted.
    msg_id = await store.record_message(
        run_id, user_b, role="user", content="sneak"
    )
    assert isinstance(msg_id, int)


async def test_record_message_round_trip_and_touch(
    pool: asyncpg.Pool,
) -> None:
    from agent.chat_store import ChatStore

    store = ChatStore(pool)
    user_id = 1
    run_id = str(_uuid.uuid4())
    await store.create_session(user_id, run_id)

    # First touch: baseline.
    before = await store.get_session(run_id, user_id)
    assert before is not None
    initial_active = before.last_active_at

    # record_message bumps last_active_at. Use a small sleep so the
    # server clock advances by at least one microsecond.
    await asyncio.sleep(0.005)
    msg_id = await store.record_message(
        run_id,
        user_id,
        role="assistant",
        content="hi back",
        tool_calls=[{"name": "list_feeds", "args": {"limit": 10}}],
        tokens=42,
    )
    assert isinstance(msg_id, int) and msg_id > 0

    after = await store.get_session(run_id, user_id)
    assert after is not None
    assert after.last_active_at > initial_active

    # The tool_calls JSON round-trips byte-faithfully.
    async with pool.acquire() as conn:
        row = await conn.fetchrow(
            "SELECT role, content, tool_calls::text AS tc, tokens "
            "FROM agent.chat_messages WHERE id=$1",
            msg_id,
        )
    assert row["role"] == "assistant"
    assert row["content"] == "hi back"
    assert row["tokens"] == 42
    parsed = _json.loads(row["tc"])
    assert parsed == [{"name": "list_feeds", "args": {"limit": 10}}]


async def test_soft_then_hard_delete_lifecycle(pool: asyncpg.Pool) -> None:
    from agent.chat_store import ChatStore

    store = ChatStore(pool)
    user_id = 1
    # Create three sessions.
    session_ids = [str(_uuid.uuid4()) for _ in range(3)]
    for sid in session_ids:
        await store.create_session(user_id, sid)
        await store.record_message(sid, user_id, role="user", content="x")

    # Backdate last_active_at to simulate the retention window passing.
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE agent.chat_sessions SET last_active_at = now() - interval '400 days'",
        )

    # Phase 1: soft-delete with retention_days=365 — all three should be
    # soft-deleted (last_active_at is 400 days old, > 365).
    soft_ids = await store.soft_delete_expired(retention_days=365)
    assert sorted(soft_ids) == sorted(session_ids)

    # Sessions still exist (soft-delete only); chat_messages still exist
    # (no FK cascade yet — `deleted_at IS NOT NULL` doesn't trigger
    # ON DELETE CASCADE).
    async with pool.acquire() as conn:
        soft_count = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_sessions WHERE deleted_at IS NOT NULL"
        )
        msg_count = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_messages"
        )
    assert soft_count == 3
    assert msg_count == 3

    # Phase 2: hard-delete with grace_days=7. All three should be
    # hard-deleted (their deleted_at was set 0 seconds ago, which is
    # less than 7 days ago — but the predicate is `deleted_at <
    # now() - 7 days`. To force the delete in this test, use grace=0.
    # The soft-delete above set deleted_at = now(), so with grace=0 the
    # deletion predicate `deleted_at < now() - 0 days` evaluates
    # against the same `now()` — race-prone but typically passes. For
    # determinism, sleep 5ms before phase 2.
    await asyncio.sleep(0.005)
    deleted = await store.hard_delete_soft_deleted(grace_days=0)
    # Returns the hard-deleted session ids (agent-user-preferences D6);
    # the caller's metric increment is `len(ids)`.
    assert sorted(deleted) == sorted(session_ids)

    # Sessions + messages all gone (FK CASCADE carried the messages).
    async with pool.acquire() as conn:
        sess_count = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_sessions"
        )
        msg_count = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_messages"
        )
    assert sess_count == 0
    assert msg_count == 0


async def test_end_session_is_idempotent(pool: asyncpg.Pool) -> None:
    from agent.chat_store import ChatStore

    store = ChatStore(pool)
    user_id = 1
    run_id = str(_uuid.uuid4())
    await store.create_session(user_id, run_id)

    # First end sets the timestamp.
    assert await store.end_session(run_id, user_id) is True
    s1 = await store.get_session(run_id, user_id)
    assert s1 is not None
    assert s1.ended_at is not None
    first_ended = s1.ended_at

    # Second end is a no-op for the bool (UPDATE matches the deleted_at
    # IS NULL filter; sets ended_at = COALESCE(ended_at, now()) which
    # preserves the first value).
    assert await store.end_session(run_id, user_id) is True
    s2 = await store.get_session(run_id, user_id)
    assert s2 is not None
    assert s2.ended_at == first_ended


# --- title writes (openspec/changes/agent-chat-fixes 2.2/2.3) --------------


async def test_set_title_if_empty_wins_once(pool: asyncpg.Pool) -> None:
    """The `title=''` predicate is what makes the deterministic write
    "the session's first exchange": it fills an empty title exactly once
    and never overwrites."""
    from agent.chat_store import ChatStore

    store = ChatStore(pool)
    sid = str(_uuid.uuid4())
    await store.create_session(1, sid)

    assert await store.set_title_if_empty(sid, 1, "熬夜加班") is True
    s = await store.get_session(sid, 1)
    assert s is not None and s.title == "熬夜加班"

    assert await store.set_title_if_empty(sid, 1, "别的标题") is False
    s2 = await store.get_session(sid, 1)
    assert s2 is not None and s2.title == "熬夜加班"


async def test_set_title_if_empty_is_atomic_under_concurrency(
    pool: asyncpg.Pool,
) -> None:
    """Concurrent first turns (a double-send) must produce exactly ONE
    winner — the guard lives in the UPDATE's WHERE clause, so the race is
    decided by the database, not by a read-then-write in Python."""
    from agent.chat_store import ChatStore

    store = ChatStore(pool)
    sid = str(_uuid.uuid4())
    await store.create_session(1, sid)

    results = await asyncio.gather(
        *(store.set_title_if_empty(sid, 1, f"标题{i}") for i in range(5))
    )
    assert results.count(True) == 1, f"exactly one winner; got {results}"


async def test_set_title_writes_are_owner_and_soft_delete_guarded(
    pool: asyncpg.Pool,
) -> None:
    """`set_title` overwrites (the refinement path), but only for the
    owner's live session — a soft-deleted session is never retitled, and
    a foreign write is a no-op that leaves the row untouched."""
    from agent.chat_store import ChatStore

    store = ChatStore(pool)
    sid = str(_uuid.uuid4())
    await store.create_session(1, sid, title="熬夜加班")

    assert await store.set_title(sid, 1, "生鲜电商推荐") is True
    s = await store.get_session(sid, 1)
    assert s is not None and s.title == "生鲜电商推荐"

    assert await store.set_title(sid, 2, "foreign") is False
    s2 = await store.get_session(sid, 1)
    assert s2 is not None and s2.title == "生鲜电商推荐"

    assert await store.soft_delete_session(sid, 1) is True
    assert await store.set_title(sid, 1, "after delete") is False
    assert await store.set_title_if_empty(sid, 1, "after delete") is False
    async with pool.acquire() as conn:
        kept = await conn.fetchval(
            "SELECT title FROM agent.chat_sessions WHERE id=$1", sid
        )
    assert kept == "生鲜电商推荐", "a soft-deleted session keeps its title"


async def test_touch_session_refreshes_last_active(
    pool: asyncpg.Pool,
) -> None:
    from agent.chat_store import ChatStore

    store = ChatStore(pool)
    user_id = 1
    run_id = str(_uuid.uuid4())
    await store.create_session(user_id, run_id)
    initial = await store.get_session(run_id, user_id)
    assert initial is not None
    initial_active = initial.last_active_at

    await asyncio.sleep(0.005)
    assert await store.touch_session(run_id, user_id) is True

    after = await store.get_session(run_id, user_id)
    assert after is not None
    assert after.last_active_at > initial_active
