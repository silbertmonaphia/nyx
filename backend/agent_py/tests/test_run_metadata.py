"""Coverage for `RunMetadata` edge cases not pinned by `test_smoke_run_state.py`.

`test_smoke_run_state.py` covers the happy path: create_run → get_run →
mark_completed, scoped by user_id. This file pins the three
behaviours the smoke does NOT exercise:

  1. `retention_delete_terminal` — hard-deletes only terminal rows
     (completed/failed) older than the retention window; running rows
     and recent terminal rows are preserved.
  2. `list_runs_for_user` — paginates and never leaks across users.
  3. Pool exhaustion — when the asyncpg pool can't satisfy acquire()
     within `acquire_timeout`, the call raises `RunMetadataUnavailable`.

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""
from __future__ import annotations

import asyncio
import os
import uuid as _uuid
from datetime import datetime, timedelta, timezone

import asyncpg
import pytest

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

pytestmark = pytest.mark.skipif(
    not _DSN,
    reason="TEST_ASYNC_PG_DSN not set; needs a live Postgres (see Makefile / CI)",
)


async def _drop_schemas() -> None:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
    finally:
        await conn.close()


async def _build_pool() -> asyncpg.Pool:
    """Build a pool sized for the test. asyncpg 0.32+ needs `await pool` to init."""
    pool = asyncpg.create_pool(dsn=_DSN, min_size=1, max_size=10)
    return await pool


@pytest.fixture
async def app_with_metadata(monkeypatch):
    """Drive the real lifespan so RunMetadata is on app.state and the
    `agent.runs` table exists."""
    _StubLlm_call_count = 0  # noqa: F841 — placeholder; fixture only
    await _drop_schemas()

    import agent.config as config_module

    settings = config_module.Settings(
        database_url=_DSN,
        llm_base_url="http://localhost:9999/v1",
        metrics_port=0,
        retention_enabled=False,
    )
    monkeypatch.setattr(config_module, "_settings", settings)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)

    from agent.main import create_app, lifespan

    app = create_app()
    async with lifespan(app):
        yield app


# ---- retention_delete_terminal -----------------------------------------


async def test_retention_deletes_only_old_terminal_rows(app_with_metadata):
    """Four rows in `agent.runs`:
      - completed 60d ago → DELETED
      - completed  1d ago → kept
      - failed    60d ago → DELETED
      - running   60d ago → kept (status='running' is excluded)
    After `retention_delete_terminal(30)`, only the 60-day terminal
    rows are gone.
    """
    rm = app_with_metadata.state.run_metadata
    # Reach the underlying pool to insert rows with controlled timestamps.
    pool = rm._pool
    now = datetime.now(timezone.utc)
    old = now - timedelta(days=60)
    recent = now - timedelta(days=1)

    async with pool.acquire() as conn:
        # Cleanup any leftover from prior runs.
        await conn.execute("DELETE FROM runs")
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, started_at, ended_at, total_tokens) "
            "VALUES ($1, 1, 'completed', $2, $2, 100)",
            _uuid.uuid4(),
            old,
        )
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, started_at, ended_at, total_tokens) "
            "VALUES ($1, 1, 'completed', $2, $2, 200)",
            _uuid.uuid4(),
            recent,
        )
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, started_at, ended_at, error_code) "
            "VALUES ($1, 1, 'failed', $2, $2, 'plan_too_large')",
            _uuid.uuid4(),
            old,
        )
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, started_at) "
            "VALUES ($1, 1, 'running', $2)",
            _uuid.uuid4(),
            old,
        )

    deleted_ids = await rm.retention_delete_terminal(30)
    assert len(deleted_ids) == 2, f"expected 2 deletions, got {len(deleted_ids)}"
    # The returned ids are the GC handles for the retention cron's
    # `adelete_thread` calls; both rows must actually be gone.
    for rid in deleted_ids:
        async with pool.acquire() as conn:
            gone = await conn.fetchval("SELECT count(*) FROM runs WHERE id=$1", rid)
        assert gone == 0, f"returned id {rid} still present"

    # After retention: the two 60-day terminal rows are gone; the
    # recent-completed and old-running rows survive.
    async with pool.acquire() as conn:
        survivors = [
            (r["status"], r["ended_at"])
            for r in await conn.fetch(
                "SELECT status, ended_at FROM runs ORDER BY started_at"
            )
        ]
    assert len(survivors) == 2, (
        f"expected 2 survivors; got {survivors}; "
        "60-day completed/failed should be gone, recent + running must remain"
    )
    statuses = {s for s, _ in survivors}
    assert statuses == {"completed", "running"}, (
        f"unexpected survivor statuses: {statuses}"
    )


# ---- list_runs_for_user pagination + scoping ---------------------------


async def test_list_paginates_and_scopes_to_user(app_with_metadata):
    """Insert 25 runs for user 1 and 3 for user 2. Page through user 1's
    history 10 at a time, assert each page is exactly user 1 and
    ordered by started_at DESC.
    """
    rm = app_with_metadata.state.run_metadata
    pool = rm._pool

    async with pool.acquire() as conn:
        await conn.execute("DELETE FROM runs")
        # 25 runs for user 1, spaced 1 hour apart so started_at order is unique.
        base = datetime.now(timezone.utc) - timedelta(hours=25)
        for i in range(25):
            await conn.execute(
                "INSERT INTO runs (id, user_id, status, started_at, total_tokens) "
                "VALUES ($1, 1, 'completed', $2, 100)",
                _uuid.uuid4(),
                base + timedelta(hours=i),
            )
        # 3 runs for user 2 — must never appear in user 1's list.
        for i in range(3):
            await conn.execute(
                "INSERT INTO runs (id, user_id, status, started_at, total_tokens) "
                "VALUES ($1, 2, 'completed', $2, 100)",
                _uuid.uuid4(),
                base + timedelta(hours=i),
            )

    page1 = await rm.list_runs_for_user(1, limit=10, offset=0)
    page2 = await rm.list_runs_for_user(1, limit=10, offset=10)
    page3 = await rm.list_runs_for_user(1, limit=10, offset=20)

    assert len(page1) == 10
    assert len(page2) == 10
    assert len(page3) == 5  # 25 - 20

    # All rows belong to user 1 (no cross-user leakage).
    for r in page1 + page2 + page3:
        assert r.user_id == 1, f"row {r.id} leaked from user {r.user_id}"

    # Order is started_at DESC across pages — the first row of page 2
    # must come right after the last row of page 1.
    all_rows = page1 + page2 + page3
    for prev, nxt in zip(all_rows, all_rows[1:]):
        assert prev.started_at >= nxt.started_at, (
            f"order broken: {prev.started_at} < {nxt.started_at}"
        )

    # User 2's runs must never appear in user 1's pages.
    async with pool.acquire() as conn:
        user2_count = await conn.fetchval(
            "SELECT count(*) FROM runs WHERE user_id = 2"
        )
    assert user2_count == 3, (
        f"expected 3 user-2 rows in DB, got {user2_count}"
    )
    # Total of user 1's pages = 25, all distinct.
    ids = {r.id for r in all_rows}
    assert len(ids) == 25


# ---- create_run heartbeat ----------------------------------------------


async def test_create_run_sets_last_heartbeat_at(app_with_metadata):
    """Spec scenario "Fresh run inserts a metadata row with the right
    shape" (agent-stale-run-sweeper delta): a fresh run carries
    `last_heartbeat_at` within the last second, so the sweeper can see
    the row is alive without waiting for the first heartbeat tick."""
    rm = app_with_metadata.state.run_metadata
    run_id = await rm.create_run(user_id=7)

    async with rm._pool.acquire() as conn:
        row = await conn.fetchrow(
            "SELECT status, ended_at, "
            "extract(epoch from (now() - last_heartbeat_at)) AS age_s "
            "FROM runs WHERE id = $1",
            run_id,
        )
    assert row["status"] == "running"
    assert row["ended_at"] is None
    assert 0 <= float(row["age_s"]) <= 1.0, (
        f"last_heartbeat_at must be set at INSERT time; age={row['age_s']}s"
    )


# ---- terminal writes override a premature reap --------------------------

async def test_mark_completed_overrides_premature_reap(app_with_metadata):
    """Spec `agent-run-sweeper` — "Completion after a premature reap wins":
    a row reaped while its process was actually alive and then genuinely
    completed ends `completed` with `error_code IS NULL`."""
    rm = app_with_metadata.state.run_metadata
    run_id = await rm.create_run(user_id=7)
    # Stall the heartbeat, reap the row (the sweeper's false-positive case).
    async with rm._pool.acquire() as conn:
        await conn.execute(
            "UPDATE runs SET last_heartbeat_at = now() - interval '60 seconds' "
            "WHERE id=$1",
            run_id,
        )
    reaped = await rm.sweep_stale_runs(timeout_seconds=30)
    assert reaped == [run_id]

    # The live process finishes and writes its terminal state.
    await rm.mark_completed(run_id, total_tokens=12345)

    async with rm._pool.acquire() as conn:
        row = await conn.fetchrow(
            "SELECT status, error_code, total_tokens FROM runs WHERE id=$1",
            run_id,
        )
    assert row["status"] == "completed"
    assert row["error_code"] is None, (
        "mark_completed must clear the sweeper's stale_run code"
    )
    assert row["total_tokens"] == 12345


async def test_mark_failed_overrides_premature_reap(app_with_metadata):
    """The live process's real failure code replaces the sweeper's
    `stale_run` — terminal writes stay authoritative."""
    rm = app_with_metadata.state.run_metadata
    run_id = await rm.create_run(user_id=7)
    async with rm._pool.acquire() as conn:
        await conn.execute(
            "UPDATE runs SET last_heartbeat_at = now() - interval '60 seconds' "
            "WHERE id=$1",
            run_id,
        )
    assert await rm.sweep_stale_runs(timeout_seconds=30) == [run_id]

    await rm.mark_failed(run_id, error_code="react_iterations_exceeded")

    async with rm._pool.acquire() as conn:
        row = await conn.fetchrow(
            "SELECT status, error_code FROM runs WHERE id=$1", run_id
        )
    assert row["status"] == "failed"
    assert row["error_code"] == "react_iterations_exceeded"


# ---- sweep_stale_runs ----------------------------------------------------


async def test_sweep_reaps_only_stale_running_rows(app_with_metadata):
    """Zombie `running` rows (heartbeat older than the timeout) are reaped
    to failed/stale_run; fresh running rows and terminal rows are never
    touched; a second sweep is a no-op (spec `agent-run-sweeper` —
    "Zombie row is reaped after the timeout" / "Live and terminal rows
    are untouched")."""
    rm = app_with_metadata.state.run_metadata
    pool = rm._pool
    stale_id = str(_uuid.uuid4())
    fresh_id = str(_uuid.uuid4())
    completed_id = str(_uuid.uuid4())

    async with pool.acquire() as conn:
        await conn.execute("DELETE FROM runs")
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, last_heartbeat_at) "
            "VALUES ($1, 1, 'running', now() - interval '60 seconds')",
            stale_id,
        )
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, last_heartbeat_at) "
            "VALUES ($1, 1, 'running', now())",
            fresh_id,
        )
        # Terminal + stale heartbeat: excluded by status, not by freshness —
        # this is the race-safety predicate (the conditional update can never
        # modify a row a live process already drove terminal).
        await conn.execute(
            "INSERT INTO runs (id, user_id, status, ended_at, last_heartbeat_at) "
            "VALUES ($1, 1, 'completed', now(), now() - interval '60 seconds')",
            completed_id,
        )

    reaped = await rm.sweep_stale_runs(timeout_seconds=30)
    assert reaped == [stale_id], f"expected exactly the stale row; got {reaped}"

    async with pool.acquire() as conn:
        row = await conn.fetchrow(
            "SELECT status, error_code, ended_at FROM runs WHERE id=$1", stale_id
        )
        assert row["status"] == "failed"
        assert row["error_code"] == "stale_run"
        assert row["ended_at"] is not None
        fresh = await conn.fetchrow("SELECT status FROM runs WHERE id=$1", fresh_id)
        assert fresh["status"] == "running"
        done = await conn.fetchrow(
            "SELECT status, error_code FROM runs WHERE id=$1", completed_id
        )
        assert done["status"] == "completed"
        assert done["error_code"] is None

    # Idempotent: nothing left to reap on a second pass.
    assert await rm.sweep_stale_runs(timeout_seconds=30) == []


# ---- touch_heartbeat ----------------------------------------------------


async def test_touch_heartbeat_advances_running_row(app_with_metadata):
    """The heartbeat loop's building block (agent-stale-run-sweeper): a
    conditional touch on a `running` row advances `last_heartbeat_at`."""
    rm = app_with_metadata.state.run_metadata
    run_id = await rm.create_run(user_id=7)
    # Backdate so the advance is unambiguous.
    async with rm._pool.acquire() as conn:
        await conn.execute(
            "UPDATE runs SET last_heartbeat_at = now() - interval '60 seconds' "
            "WHERE id=$1",
            run_id,
        )

    touched = await rm.touch_heartbeat(run_id)
    assert touched is True

    async with rm._pool.acquire() as conn:
        age = await conn.fetchval(
            "SELECT extract(epoch from (now() - last_heartbeat_at)) "
            "FROM runs WHERE id=$1",
            run_id,
        )
    assert 0 <= float(age) <= 1.0, f"heartbeat not refreshed; age={age}s"


async def test_touch_heartbeat_noops_on_terminal_row(app_with_metadata):
    """A heartbeat must never resurrect a terminal row: the conditional
    update matches nothing, returns False, and the row is untouched."""
    rm = app_with_metadata.state.run_metadata
    run_id = await rm.create_run(user_id=7)
    await rm.mark_completed(run_id, total_tokens=5)

    touched = await rm.touch_heartbeat(run_id)
    assert touched is False

    async with rm._pool.acquire() as conn:
        row = await conn.fetchrow(
            "SELECT status, ended_at FROM runs WHERE id=$1", run_id
        )
    assert row["status"] == "completed"
    assert row["ended_at"] is not None


# ---- pool exhaustion ----------------------------------------------------


async def test_pool_exhaustion_raises_run_metadata_unavailable():
    """Build a pool with max_size=1 and acquire_timeout=0.1s. Hold
    the single connection; issue a second `create_run` from the same
    `RunMetadata` instance. The second acquire must time out and
    raise `RunMetadataUnavailable` (after the retry budget is spent).
    """
    from agent.run_metadata import RunMetadata, RunMetadataUnavailable

    pool = await _build_pool()
    try:
        # Drive the lifespan's migrations so `agent.runs` exists.
        await _drop_schemas()
        from agent.config import Settings

        settings = Settings(
            database_url=_DSN,
            llm_base_url="http://localhost:9999/v1",
            metrics_port=0,
            retention_enabled=False,
        )
        import agent.config as config_module

        config_module._settings = settings

        from agent.main import lifespan, create_app

        app = create_app()
        async with lifespan(app):
            # Build a fresh pool with max_size=1, but use the same DSN.
            small_pool = asyncpg.create_pool(
                dsn=_DSN, min_size=1, max_size=1
            )
            small_pool = await small_pool
            try:
                rm = RunMetadata(
                    small_pool, max_retries=1, acquire_timeout=0.1
                )

                # Hold the only connection.
                holder = await small_pool.acquire()

                async def _release_after_delay():
                    await asyncio.sleep(0.3)  # > acquire_timeout
                    await small_pool.release(holder)

                release_task = asyncio.create_task(_release_after_delay())
                try:
                    with pytest.raises(RunMetadataUnavailable):
                        await rm.create_run(user_id=42)
                finally:
                    await release_task
            finally:
                await small_pool.close()
    finally:
        await pool.close()
