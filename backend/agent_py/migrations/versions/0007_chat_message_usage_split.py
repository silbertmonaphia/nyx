"""add prompt/completion token split to agent.chat_messages

Revision ID: 0007_chat_message_usage_split
Revises: 0006_data_audit_log
Create Date: 2026-10-18

`tokens` has always carried the run's total token count, but the replay
wire (and therefore a reopened chat panel) could not reconstruct the
prompt/completion split the live `done` frame renders. Both columns are
nullable so pre-migration rows stay distinguishable: NULL means "split
unknown, show the total only", while a written 0 is a real provider
zero.
"""
from __future__ import annotations

import sqlalchemy as sa
from alembic import op

revision: str = "0007_chat_message_usage_split"
down_revision: str | None = "0006_data_audit_log"
branch_labels: str | None = None
depends_on: str | None = None


def upgrade() -> None:
    op.add_column(
        "chat_messages",
        sa.Column("prompt_tokens", sa.Integer(), nullable=True),
        schema="agent",
    )
    op.add_column(
        "chat_messages",
        sa.Column("completion_tokens", sa.Integer(), nullable=True),
        schema="agent",
    )


def downgrade() -> None:
    op.drop_column("chat_messages", "completion_tokens", schema="agent")
    op.drop_column("chat_messages", "prompt_tokens", schema="agent")
