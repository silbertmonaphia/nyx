"""Dev-only alembic upgrade hook.

Used by `make dev` when no dedicated `agent-migrate` compose service is
running. The compose production path runs `alembic upgrade head` as a
one-shot `agent-migrate` service instead — see `docker-compose.yml`.

Gated by `PYTHON_AGENT_MIGRATIONS_DISABLED` (default false, so the
inline path is the default for `make dev-agent`).
"""
from __future__ import annotations

import logging
from pathlib import Path

from alembic import command as alembic_command
from alembic.config import Config as AlembicConfig

log = logging.getLogger("nyx.agent.migrations.dev_hook")


def _build_config() -> AlembicConfig:
    """Build an alembic Config pointing at the project's migrations/ tree."""
    here = Path(__file__).resolve().parent  # backend/agent_py/migrations
    project_root = here.parent  # backend/agent_py
    cfg = AlembicConfig(str(project_root / "alembic.ini"))
    cfg.set_main_option("script_location", str(here))
    cfg.set_main_option("prepend_sys_path", str(project_root))
    return cfg


async def run_upgrade_head() -> None:
    """Run `alembic upgrade head` against the configured database URL.

    Reads the DSN via `agent.config.Settings` (NOT `os.environ`) because
    pydantic-settings loads `.env` file values into Settings fields but
    does NOT populate `os.environ` — `os.environ.get()` would silently miss
    values that the Settings class has already read.

    Sync alembic call — wrap in `asyncio.to_thread` to avoid blocking the
    event loop. The connection round-trip to Postgres dominates the runtime;
    alembic's own overhead is negligible.
    """
    import asyncio

    from agent.config import get_settings

    db_url = get_settings().database_url
    if not db_url:
        raise RuntimeError(
            "PYTHON_AGENT_DATABASE_URL must be set to run dev migrations. "
            "Add it to backend/agent_py/.env (see backend/agent_py/.env.example)."
        )
    cfg = _build_config()
    # ConfigParser interpolates `%` as `%(name)s` references, but our
    # URL has `%20` / `%3D` / `%2C` from URL-encoding the
    # `search_path=langgraph,agent` value. Escape `%` → `%%` so
    # ConfigParser stores the literal `%` and SQLAlchemy/asyncpg see
    # the correct URL.
    cfg.set_main_option(
        "sqlalchemy.url", db_url.replace("%", "%%")
    )
    log.info("running alembic upgrade head (dev inline path)")
    await asyncio.to_thread(alembic_command.upgrade, cfg, "head")
    log.info("alembic upgrade head complete")
