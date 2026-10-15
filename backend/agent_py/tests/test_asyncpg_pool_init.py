"""Regression guards for library API contracts our code depends on.

Each test here pins a documented behavior of a dependency that, if it ever
changes, would silently break our call sites. The failure of one of these
tests is a deliberate signal — it means a dep's contract shifted and the
call sites (e.g., `agent/run_metadata.py:build_pool`) need re-verification.

The first one was born from a real bug: `asyncpg.create_pool()` (0.32+)
returns an *uninitialized* `Pool`; the first `acquire()` raises
`InterfaceError("pool is not initialized")` unless you first `await pool`
(which invokes `Pool.__await__` → `_async__init__`) or use `async with`.
"""
from __future__ import annotations

import pytest

import asyncpg
from asyncpg.exceptions import InterfaceError

# These guards need a live Postgres. They self-skip when the env var is
# not set — same posture as the Go integration tests (`t.Skip()` on
# missing `dbURL`). CI with Docker sets it; local runs without Docker
# skip.
import os

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

pytestmark = pytest.mark.skipif(
    not _DSN,
    reason="TEST_ASYNC_PG_DSN not set; needs a live Postgres (see Makefile / CI)",
)


async def test_create_pool_requires_await_before_acquire():
    """Pins asyncpg's documented contract: create_pool() is lazy.

    If a future asyncpg release makes create_pool() eagerly initialize,
    the first `pytest.raises` below will fail — that's the signal to
    revisit `build_pool()` and this guard.
    """
    pool = asyncpg.create_pool(dsn=_DSN, min_size=1, max_size=1)
    # Contract: the raw create_pool() result is NOT usable yet.
    with pytest.raises(InterfaceError, match="pool is not initialized"):
        async with pool.acquire():
            pass
    # Contract: `await pool` initializes it (Pool.__await__ → _async__init__).
    await pool
    async with pool.acquire() as conn:
        assert await conn.fetchval("SELECT 1") == 1
    await pool.close()


async def test_build_pool_returns_initialized_pool():
    """Our build_pool() wrapper must hand back a ready-to-use pool.

    This is the actual regression guard for the bug: if someone drops the
    `await pool` line inside `build_pool`, this test fails on the first
    acquire with `InterfaceError("pool is not initialized")`.
    """
    from agent.run_metadata import build_pool

    pool = await build_pool(_DSN, max_size=2)
    async with pool.acquire() as conn:
        assert await conn.fetchval("SELECT 1") == 1
    await pool.close()
