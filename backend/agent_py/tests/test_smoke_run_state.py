"""End-to-end smoke test for run-state persistence.

Exercises the REAL FastAPI lifespan against a live Postgres — alembic
migrations, langGraph `AsyncPostgresSaver.setup()`, pool init — then
asserts the resulting committed DB state with a fresh connection.

This is the class of test that would have caught the
`pool is not initialized` bug and the alembic-rollback bug: it runs the
actual startup path instead of importing modules and calling helpers in
isolation. Assertions happen AFTER the lifespan exits, so uncommitted
DDL (visible inside the block, gone after rollback) fails the test.

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres) — same
posture as the Go integration tests' `t.Skip()` on missing `dbURL`.
"""
from __future__ import annotations

import os

import asyncpg
import pytest

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

pytestmark = pytest.mark.skipif(
    not _DSN,
    reason="TEST_ASYNC_PG_DSN not set; needs a live Postgres (see Makefile / CI)",
)

# Tables that MUST NOT exist in `public` — they belong to the `langgraph`
# or `agent` schemas. If any appears in `public` after a lifespan run,
# search_path handling regressed (this is the exact leak the 2026-10
# unawaited-coroutine bug produced). Dropped at test start so stale
# artifacts from earlier broken runs don't mask the regression check.
_FORBIDDEN_IN_PUBLIC = (
    "checkpoints",
    "checkpoint_blobs",
    "checkpoint_writes",
    "checkpoint_migrations",
    "alembic_version",
    "runs",
)


async def _drop_schemas() -> None:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
        for table in _FORBIDDEN_IN_PUBLIC:
            await conn.execute(f'DROP TABLE IF EXISTS public."{table}" CASCADE')
    finally:
        await conn.close()


async def _fetch_schema_names() -> set[str]:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        rows = await conn.fetch(
            "SELECT schema_name FROM information_schema.schemata"
        )
        return {r["schema_name"] for r in rows}
    finally:
        await conn.close()


async def _fetch_tables(schema: str) -> set[str]:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        rows = await conn.fetch(
            "SELECT table_name FROM information_schema.tables "
            "WHERE table_schema = $1",
            schema,
        )
        return {r["table_name"] for r in rows}
    finally:
        await conn.close()


async def test_lifespan_creates_schemas_and_tables(monkeypatch):
    """Full lifespan startup against live PG; assert committed schema state."""
    await _drop_schemas()  # clean slate (re-runnable)

    # Drive the Settings singleton explicitly. Init kwargs beat both the
    # `.env` file and ambient env, so the test doesn't depend on the
    # developer's local config.
    import agent.config as config_mod

    settings = config_mod.Settings(
        database_url=_DSN,  # validator appends the encoded search_path option
        llm_base_url="http://localhost:9999/v1",  # bypass the OpenAI-key guard
        metrics_port=0,  # random free port; avoids collisions across tests
        retention_enabled=False,  # keep the smoke test deterministic
    )
    monkeypatch.setattr(config_mod, "_settings", settings)

    # Import here (not module-level): importing agent.main constructs a
    # module-level app with ambient settings; patched singleton must be
    # in place for the app we build ourselves.
    from agent.main import create_app, lifespan

    app = create_app()

    async with lifespan(app):
        pass  # startup runs migrations + saver.setup(); shutdown closes conns

    # --- Assertions AFTER shutdown: catches uncommitted/rolled-back DDL ---

    schemas = await _fetch_schema_names()
    assert "langgraph" in schemas, "alembic must create the langgraph schema"
    assert "agent" in schemas, "alembic must create the agent schema"

    agent_tables = await _fetch_tables("agent")
    assert "runs" in agent_tables, "0002 revision must create agent.runs"
    assert "alembic_version" in agent_tables, "alembic bookkeeping table"

    langgraph_tables = await _fetch_tables("langgraph")
    assert "checkpoints" in langgraph_tables, (
        "langGraph setup() must land checkpoint tables in langgraph.* — "
        "if this fails, the search_path set before setup() regressed"
    )
    assert "checkpoint_migrations" in langgraph_tables

    public_tables = await _fetch_tables("public")
    leaked = [t for t in _FORBIDDEN_IN_PUBLIC if t in public_tables]
    assert not leaked, (
        f"these tables leaked into public (search_path handling regressed): {leaked}"
    )

    await _drop_schemas()  # leave the DB as we found it


async def test_metadata_roundtrip_after_lifespan(monkeypatch):
    """After startup, the RunMetadata layer actually works end-to-end.

    Directly exercises create → get → mark_completed against the real
    schema the lifespan just created. This is the runtime path that the
    `pool is not initialized` bug killed.
    """
    await _drop_schemas()

    import agent.config as config_mod

    settings = config_mod.Settings(
        database_url=_DSN,
        llm_base_url="http://localhost:9999/v1",
        metrics_port=0,
        retention_enabled=False,
    )
    monkeypatch.setattr(config_mod, "_settings", settings)

    from agent.main import create_app, lifespan
    from agent.run_metadata import RunMetadata

    app = create_app()
    async with lifespan(app):
        run_metadata: RunMetadata = app.state.run_metadata
        assert run_metadata is not None, "lifespan must wire RunMetadata"

        run_id = await run_metadata.create_run(user_id=42)
        fetched = await run_metadata.get_run(run_id, user_id=42)
        assert fetched is not None
        assert fetched.status == "running"

        # Per-user scoping: another user must NOT see this run.
        assert await run_metadata.get_run(run_id, user_id=7) is None

        await run_metadata.mark_completed(run_id, total_tokens=1234)
        done = await run_metadata.get_run(run_id, user_id=42)
        assert done is not None
        assert done.status == "completed"
        assert done.total_tokens == 1234
        assert done.ended_at is not None

    await _drop_schemas()
