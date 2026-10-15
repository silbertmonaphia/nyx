"""create agent.chat_user_prefs

Revision ID: 0005_chat_user_prefs
Revises: 0004_chat_sessions_and_messages
Create Date: 2026-10-14

User-preference memory (openspec/changes/agent-user-preferences, Slice 3
of `AgentMemoryRoadmap.md`).

One table, `agent.chat_user_prefs` — one row per `(user_id, key)`:

  - Upsert-per-key model: a preference ("snack -> 鱿鱼丝") has exactly one
    current truth, so the UNIQUE index on `(user_id, key)` is the
    `ON CONFLICT` target and writers just overwrite `value` /
    `source_session_id` / `updated_at` in place. The injection read is a
    plain `ORDER BY updated_at DESC LIMIT N` per user — no read-time
    reduction, no unbounded growth (design.md D1). No confidence column:
    the flat key/value regime keeps only the latest statement.

  - `source_session_id` is NULLABLE and deliberately has NO foreign key:
    the writer may run on a metadata-only dev path where the session row
    is absent, and the retention coupling is enforced explicitly by the
    deletion path instead. The chat-retention cron's hard-delete of a
    session deletes that session's derived prefs in the same iteration
    (`DELETE FROM chat_user_prefs WHERE source_session_id = ANY($1::uuid[])`,
    design.md D6) — scoped to the exact ids the cron just captured from
    its own hard-delete `RETURNING id`.

No backfill: no historical prefs exist to derive; retro-extraction over
stored sessions is explicitly out of scope (design.md migration plan).
"""
from __future__ import annotations

import sqlalchemy as sa
from alembic import op

revision: str = "0005_chat_user_prefs"
down_revision: str | None = "0004_chat_sessions_and_messages"
branch_labels: str | None = None
depends_on: str | None = None


def upgrade() -> None:
    op.create_table(
        "chat_user_prefs",
        sa.Column(
            "id",
            sa.BigInteger(),
            sa.Identity(always=False),
            primary_key=True,
        ),
        sa.Column("user_id", sa.BigInteger(), nullable=False),
        sa.Column("key", sa.Text(), nullable=False),
        sa.Column(
            "value",
            sa.dialects.postgresql.JSONB(),
            nullable=False,
        ),
        sa.Column(
            "source_session_id",
            sa.dialects.postgresql.UUID(as_uuid=False),
            nullable=True,
        ),
        sa.Column(
            "created_at",
            sa.dialects.postgresql.TIMESTAMP(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        sa.Column(
            "updated_at",
            sa.dialects.postgresql.TIMESTAMP(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        schema="agent",
    )

    # Upsert anchor: one row per (user_id, key) — the `ON CONFLICT
    # (user_id, key) DO UPDATE` target of `PrefsStore.upsert_pref`.
    op.create_index(
        "chat_user_prefs_user_key_idx",
        "chat_user_prefs",
        ["user_id", "key"],
        schema="agent",
        unique=True,
    )

    # Injection read index — `PrefsStore.load_prefs` runs
    # `WHERE user_id=$1 ORDER BY updated_at DESC LIMIT $2` on every planner
    # call; the DESC ordering lives in the index so the read is a bounded
    # index scan, not a sort.
    op.create_index(
        "chat_user_prefs_user_updated_idx",
        "chat_user_prefs",
        ["user_id", sa.text("updated_at DESC")],
        schema="agent",
        unique=False,
    )


def downgrade() -> None:
    op.drop_index(
        "chat_user_prefs_user_updated_idx",
        table_name="chat_user_prefs",
        schema="agent",
    )
    op.drop_index(
        "chat_user_prefs_user_key_idx",
        table_name="chat_user_prefs",
        schema="agent",
    )
    op.drop_table("chat_user_prefs", schema="agent")
