"""create agent.chat_sessions and agent.chat_messages

Revision ID: 0004_chat_sessions_and_messages
Revises: 0003_add_run_heartbeat
Create Date: 2026-10-14

Persistent chat-history store (openspec/changes/agent-chat-history-storage).

Two tables:
  - `agent.chat_sessions` — one row per chat session. `id` reuses the
    same uuid4 that is emitted as the `run_id` SSE frame and stored in
    `agent.runs.id` (per design.md D2 — no separate identifier namespace).
    `last_active_at` is the retention-predicate anchor (D8) and is
    refreshed by `ChatStore.touch_session` on every recorded message.
    `deleted_at` carries the soft-delete state for the retention cron.

  - `agent.chat_messages` — one row per message within a session. FK to
    `chat_sessions.id` with `ON DELETE CASCADE` so retention hard-delete
    cleans up messages in one statement (D4). `tool_calls` is `jsonb`
    carrying the byte-faithful SSE envelope verbatim (D3); NULL for
    non-tool-bearing roles. `tokens` is provider-reported token count
    (0 when unknown).

The `WHERE deleted_at IS NULL` partial index on `chat_sessions.user_id,
last_active_at DESC` keeps the Slice 2 sidebar query cheap and skips
soft-deleted rows automatically (they're invisible to all read paths
in Slice 1).

No backfill needed — both tables start empty.
"""
from __future__ import annotations

import sqlalchemy as sa
from alembic import op

revision: str = "0004_chat_sessions_and_messages"
down_revision: str | None = "0003_add_run_heartbeat"
branch_labels: str | None = None
depends_on: str | None = None


def upgrade() -> None:
    op.create_table(
        "chat_sessions",
        sa.Column(
            "id",
            sa.dialects.postgresql.UUID(as_uuid=False),
            primary_key=True,
        ),
        sa.Column("user_id", sa.BigInteger(), nullable=False),
        sa.Column("title", sa.Text(), nullable=False, server_default=sa.text("''")),
        sa.Column(
            "started_at",
            sa.dialects.postgresql.TIMESTAMP(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        sa.Column(
            "last_active_at",
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
            "deleted_at",
            sa.dialects.postgresql.TIMESTAMP(timezone=True),
            nullable=True,
        ),
        schema="agent",
    )

    # Sidebar query index — `(user_id, last_active_at DESC)` with a partial
    # `WHERE deleted_at IS NULL` predicate so soft-deleted rows never enter
    # the Slice 2 sidebar. Keeps the retention cron's soft-delete pass
    # outside this index too.
    op.create_index(
        "chat_sessions_user_active_idx",
        "chat_sessions",
        ["user_id", sa.text("last_active_at DESC")],
        schema="agent",
        unique=False,
        postgresql_where=sa.text("deleted_at IS NULL"),
    )

    op.create_table(
        "chat_messages",
        sa.Column(
            "id",
            sa.BigInteger(),
            sa.Identity(always=False),
            primary_key=True,
        ),
        sa.Column(
            "session_id",
            sa.dialects.postgresql.UUID(as_uuid=False),
            sa.ForeignKey(
                "agent.chat_sessions.id",
                ondelete="CASCADE",
            ),
            nullable=False,
        ),
        sa.Column("role", sa.Text(), nullable=False),
        sa.Column("content", sa.Text(), nullable=False, server_default=sa.text("''")),
        sa.Column(
            "tool_calls",
            sa.dialects.postgresql.JSONB(),
            nullable=True,
        ),
        sa.Column(
            "tokens",
            sa.Integer(),
            nullable=False,
            server_default=sa.text("0"),
        ),
        sa.Column(
            "created_at",
            sa.dialects.postgresql.TIMESTAMP(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        schema="agent",
    )

    op.create_index(
        "chat_messages_session_created_idx",
        "chat_messages",
        ["session_id", sa.text("created_at")],
        schema="agent",
        unique=False,
    )


def downgrade() -> None:
    op.drop_index(
        "chat_messages_session_created_idx", table_name="chat_messages", schema="agent"
    )
    op.drop_table("chat_messages", schema="agent")
    op.drop_index(
        "chat_sessions_user_active_idx", table_name="chat_sessions", schema="agent"
    )
    op.drop_table("chat_sessions", schema="agent")
