"""create langgraph schema for langGraph checkpoint tables

Revision ID: 0001_langgraph_checkpoint
Revises:
Create Date: 2026-10-12

This revision exists as the schema-version anchor for the `langgraph`
namespace — the schema itself is created in `env.py` so alembic can land
its `agent.alembic_version` version table on a pre-existing schema.
langGraph's `checkpoint_*` tables are created by `AsyncPostgresSaver.setup()`
at app startup, which executes langGraph's hardcoded `MIGRATIONS` list
against the configured connection. With `search_path=langgraph,agent` on
the DSN, those tables land in the `langgraph.*` schema.
"""
from __future__ import annotations

from alembic import op

revision: str = "0001_langgraph_checkpoint"
down_revision: str | None = None
branch_labels: str | None = None
depends_on: str | None = None


def upgrade() -> None:
    # Schema pre-created in env.py so alembic's version table can land.
    # This revision is a no-op upgrade but exists as the version anchor.
    pass


def downgrade() -> None:
    op.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
