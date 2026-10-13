"""add last_heartbeat_at to agent.runs

Revision ID: 0003_add_run_heartbeat
Revises: 0002_runs_metadata
Create Date: 2026-10-14

Liveness column for the stale-run sweeper
(openspec/changes/agent-stale-run-sweeper): while a run's SSE stream is in
flight, a background task touches `last_heartbeat_at` every
`PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (default 30). The sweeper reaps
rows with `status='running'` whose heartbeat is older than
`PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` (default 900) to
`status='failed', error_code='stale_run'`.

`server_default=now()` backfills pre-existing rows at ALTER TABLE time —
they get one natural timeout window before any reap can touch them, so no
separate backfill script is needed.
"""
from __future__ import annotations

import sqlalchemy as sa
from alembic import op

revision: str = "0003_add_run_heartbeat"
down_revision: str | None = "0002_runs_metadata"
branch_labels: str | None = None
depends_on: str | None = None


def upgrade() -> None:
    op.add_column(
        "runs",
        sa.Column(
            "last_heartbeat_at",
            sa.dialects.postgresql.TIMESTAMP(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        schema="agent",
    )

    # The sweeper polls `status='running' AND last_heartbeat_at < now() - $1`
    # every PYTHON_AGENT_SWEEP_INTERVAL_SECONDS (default 300s ≈ 288 scans/day;
    # test-review 2026-10-14). A partial index bounded to live runs keeps that
    # recurring scan off the full table.
    op.create_index(
        "runs_running_heartbeat_idx",
        "runs",
        ["last_heartbeat_at"],
        schema="agent",
        unique=False,
        postgresql_where=sa.text("status = 'running'"),
    )


def downgrade() -> None:
    op.drop_index("runs_running_heartbeat_idx", table_name="runs", schema="agent")
    op.drop_column("runs", "last_heartbeat_at", schema="agent")
