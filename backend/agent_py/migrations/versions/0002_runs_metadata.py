"""create agent.runs metadata table

Revision ID: 0002_runs_metadata
Revises: 0001_langgraph_checkpoint
Create Date: 2026-10-12

Thin metadata layer for user-facing queries about agent runs:
list_runs_for_user, retention cron, total_tokens / error_code aggregates.
langGraph's opaque `checkpoint_*` blob in `langgraph.*` carries the actual
state; this table only carries the indexed, queryable user-facing fields.
"""
from __future__ import annotations

import sqlalchemy as sa
from alembic import op

revision: str = "0002_runs_metadata"
down_revision: str | None = "0001_langgraph_checkpoint"
labels_labels: str | None = None
depends_on: str | None = None


def upgrade() -> None:
    # `agent` schema pre-created in env.py so alembic's version table
    # could land. Just create the metadata table here.
    op.create_table(
        "runs",
        sa.Column("id", sa.dialects.postgresql.UUID(as_uuid=False), primary_key=True),
        sa.Column("user_id", sa.BigInteger(), nullable=False),
        sa.Column("status", sa.Text(), nullable=False),
        sa.Column(
            "total_tokens",
            sa.BigInteger(),
            nullable=False,
            server_default=sa.text("0"),
        ),
        sa.Column("error_code", sa.Text(), nullable=True),
        sa.Column(
            "started_at",
            sa.dialects.postgresql.TIMESTAMP(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        sa.Column(
            "ended_at",
            sa.dialects.postgresql.TIMESTAMP(timezone=True),
            nullable=True,
        ),
        sa.Column(
            "archived_at",
            sa.dialects.postgresql.TIMESTAMP(timezone=True),
            nullable=True,
        ),
        schema="agent",
    )

    op.create_index(
        "runs_user_started_idx",
        "runs",
        ["user_id", sa.text("started_at DESC")],
        schema="agent",
        unique=False,
    )


def downgrade() -> None:
    op.drop_index("runs_user_started_idx", table_name="runs", schema="agent")
    op.drop_table("runs", schema="agent")
