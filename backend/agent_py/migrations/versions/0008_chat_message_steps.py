"""add the assistant step trace to agent.chat_messages

Revision ID: 0008_chat_message_steps
Revises: 0007_chat_message_usage_split
Create Date: 2026-10-18

Replay rebuilt steps only from `tool_calls`, which native `rag` / `llm`
runs never populate — a live `[rag, llm]` turn replayed as a bare answer.
`steps` carries the assistant row's whole step trace (per-step id, kind,
terminal status, tool name/args/result, retry count, failure reason) in
plan order. Nullable so no-trace rows stay distinguishable: NULL means
"no trace available" — user / tool / system rows, rows persisted before
this column existed, and runs that planned no steps.
"""
from __future__ import annotations

import sqlalchemy as sa
from alembic import op

revision: str = "0008_chat_message_steps"
down_revision: str | None = "0007_chat_message_usage_split"
branch_labels: str | None = None
depends_on: str | None = None


def upgrade() -> None:
    op.add_column(
        "chat_messages",
        sa.Column("steps", sa.dialects.postgresql.JSONB(), nullable=True),
        schema="agent",
    )


def downgrade() -> None:
    op.drop_column("chat_messages", "steps", schema="agent")
