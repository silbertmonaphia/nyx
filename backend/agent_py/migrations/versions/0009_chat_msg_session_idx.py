"""index agent.chat_messages on (session_id, id)

Revision ID: 0009_chat_msg_session_idx
Revises: 0008_chat_message_steps
Create Date: 2026-11-13

The paged replay keyset (`WHERE session_id=$1 AND id < $2 ORDER BY id
DESC`) filters on `session_id` while ordering by the primary key, so
without this composite index the plan walks the global PK backwards
across every user's rows until it accumulates a page for the requested
session. `(session_id, id)` lets the scan start at the session's own
slice and stop after `LIMIT` rows.

Built CONCURRENTLY (autocommit block): the migration runs on every
agent boot, so a plain CREATE INDEX would take a SHARE lock and block
chat-message writes for the build's duration on a populated table.
The concurrent build is a no-op when the index already exists.
"""
from __future__ import annotations

from alembic import op

revision: str = "0009_chat_msg_session_idx"
down_revision: str | None = "0008_chat_message_steps"
branch_labels: str | None = None
depends_on: str | None = None


def upgrade() -> None:
    with op.get_context().autocommit_block():
        op.create_index(
            "chat_messages_session_id_idx",
            "chat_messages",
            ["session_id", "id"],
            schema="agent",
            unique=False,
            postgresql_concurrently=True,
        )


def downgrade() -> None:
    with op.get_context().autocommit_block():
        op.drop_index(
            "chat_messages_session_id_idx",
            table_name="chat_messages",
            schema="agent",
            postgresql_concurrently=True,
        )
