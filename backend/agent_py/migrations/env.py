"""Alembic env for the Python agent service.

Reads `PYTHON_AGENT_DATABASE_URL` from the environment and runs migrations
against an async SQLAlchemy engine. The `search_path` is set per-connection
via SQL (`SET search_path TO ...`) inside `do_run_migrations` rather than
via the URL `options=-c search_path=...` parameter — psycopg passes
URL options as `-c name=value` CLI args to postgres which chokes on `=`
inside the value.

Our alembic version table (`alembic_version`) lives in `agent.*`
(per `version_table_schema='agent'`). langGraph's `checkpoint_*` tables
live in `langgraph.*` (managed by langGraph's `setup()` at app
startup, not by this alembic chain).
"""
from __future__ import annotations

import logging
import os

from alembic import context
from sqlalchemy import pool
from sqlalchemy.engine import Connection
from sqlalchemy.ext.asyncio import async_engine_from_config

logger = logging.getLogger("alembic.env")

config = context.config

DATABASE_URL_ENV = "PYTHON_AGENT_DATABASE_URL"


def _resolve_database_url() -> str:
    """Return the configured DSN from env (or alembic.ini as fallback)."""
    url = os.environ.get(DATABASE_URL_ENV) or config.get_main_option("sqlalchemy.url")
    if not url:
        raise RuntimeError(
            f"{DATABASE_URL_ENV} must be set or sqlalchemy.url configured in alembic.ini"
        )
    return url


def run_migrations_offline() -> None:
    """Run migrations in 'offline' mode (emit SQL without a DB connection)."""
    context.configure(
        url=_resolve_database_url(),
        target_metadata=None,
        literal_binds=True,
        dialect_opts={"paramstyle": "named"},
        version_table_schema="agent",
    )
    with context.begin_transaction():
        context.run_migrations()


def do_run_migrations(connection: Connection) -> None:
    # Pre-create the `langgraph` and `agent` schemas so alembic's
    # `CREATE TABLE agent.alembic_version` (which fires *before* our
    # revisions run) lands on an existing schema. Both are idempotent.
    # Set `search_path` via SQL rather than via the URL `options=...`
    # parameter — psycopg passes URL options as `-c name=value` args to
    # postgres, which chokes on `=` inside the value.
    #
    # Each statement runs in its own implicit transaction (psycopg default
    # for autocommit-style DDL), so we explicitly commit between the
    # pre-DDL and `context.configure()` so the CREATE SCHEMA calls land
    # even when `context.begin_transaction()` later rolls back.
    connection.exec_driver_sql("CREATE SCHEMA IF NOT EXISTS langgraph")
    connection.exec_driver_sql("CREATE SCHEMA IF NOT EXISTS agent")
    connection.exec_driver_sql("SET search_path TO langgraph, agent")
    connection.commit()
    context.configure(
        connection=connection,
        target_metadata=None,
        version_table_schema="agent",
    )
    with context.begin_transaction():
        context.run_migrations()
    # Commit the outer transaction so DDL isn't rolled back when the
    # `async with connectable.connect()` block exits.
    connection.commit()


async def run_async_migrations() -> None:
    """Run migrations using an async engine."""
    url = _resolve_database_url()
    logger.info("alembic: DSN %s", url)
    config_section = config.get_section(config.config_ini_section, {})
    config_section["sqlalchemy.url"] = url
    connectable = async_engine_from_config(
        config_section,
        prefix="sqlalchemy.",
        poolclass=pool.NullPool,
    )
    async with connectable.connect() as connection:
        await connection.run_sync(do_run_migrations)
    await connectable.dispose()


def run_migrations_online() -> None:
    """Run migrations in 'online' mode with an async engine."""
    import asyncio

    asyncio.run(run_async_migrations())


if context.is_offline_mode():
    run_migrations_offline()
else:
    run_migrations_online()
