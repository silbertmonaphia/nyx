# Proposal

## Why

`agent.runs` rows left `running` by a crashed or killed agent process — or by the deliberate `agent_unavailable` path (`agent/main.py:477-482`) — are never reaped: the retention cron deletes only terminal rows, and nothing else touches them. They leak forever, stay indistinguishable from live runs for the resume decision (an echoed `run_id` re-hydrates a dead run's checkpoint), and their `langgraph.checkpoint_*` rows accumulate unbounded — `retention.py`'s claim that LangGraph prunes its own checkpoints is wrong; the framework ships no retention story and `adelete_thread` is never called. The industry baseline for durable workflow engines (Temporal, Celery, Inngest) is heartbeat/lease + timeout reaper; `AgentArchitecture.md`'s gap table ranks this the highest remaining fault-tolerance gap, and `main.py` carries the deferral comment since `agent-run-state`.

## What Changes

- Add `last_heartbeat_at` to `agent.runs` (alembic `0003`). While a run is live, a background asyncio task in the SSE generator touches it every `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (default 30); the task is cancelled in the generator's `finally`. Heartbeat write failures are non-fatal (log-and-continue), matching the metadata layer's existing posture.
- New `StaleRunSweeper` cron alongside `RetentionCron`: every `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS` (default 300), reap rows matching `status='running' AND last_heartbeat_at < now() - PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` (default 900) to `status='failed'`, `error_code='stale_run'`, `ended_at=now()`. OTel span `agent.run.sweep`; counter `agent_runs_swept_total`.
- Sweep-threshold semantics: the timeout is the window in which a crashed run can still be resumed by echoing `run_id` (resume path unchanged). After reaping, the row is terminal and resume follows the existing `fresh_run` note path. No SSE wire change — a reaped run has no live stream to notify.
- First-writer-wins concurrency: heartbeat touches and sweep reaps are conditional on `status='running'`; terminal writes (`mark_completed` / `mark_failed`) stay authoritative, and `mark_completed` additionally clears `error_code` so a premature sweep cannot leave a stale code on a completed row.
- Bundle LangGraph checkpoint GC, **single-homed at retention**: `adelete_thread(run_id)` runs for every retention-deleted terminal row (including previously reaped runs, whose rows became terminal at reap time) — replacing the false "framework prunes checkpoints" comment and fixing the unbounded `langgraph.checkpoint_*` growth. Reaped runs' checkpoint threads are deliberately NOT deleted at reap; a crashed run's checkpoint is collected with its row at retention time, so it survives the same window as any other terminal run.
- Config: `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS`, `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS`, `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS`, `PYTHON_AGENT_SWEEP_ENABLED` (default true). Startup validators reject negative values and a stale timeout below 2× the heartbeat interval (a misconfigured ratio would reap live runs). Rollback is `PYTHON_AGENT_SWEEP_ENABLED=false`.
- Scope fence (parallel-work constraint): this change does not touch `executor.py`, `types.py`, `tools_client.py`, `state.py`, `frontend/`, or `openspec/changes/agent-tool-dispatch-retry*`.

## Capabilities

### New Capabilities

- `agent-run-sweeper`: heartbeat liveness for live runs, the stale-run reaper cron, and the resume-window semantics of the sweep threshold. (Checkpoint GC itself is retention-owned — the sweeper never touches checkpoint data; see the `agent-run-store-py` delta.)

### Modified Capabilities

- `agent-run-store-py`: `create_run` row shape gains `last_heartbeat_at`; lifecycle writes gain the conditional / first-writer-wins rules; the retention cron now deletes each removed run's langGraph checkpoint thread via `adelete_thread` (replacing the "cron SHALL NOT touch checkpoint tables" clause).
- `agent-python-service`: the env-var list (requirement 7) gains the four new settings; the retention requirement (12) is unchanged in names and gains nothing.
- `agent-observability`: the counter list (requirement 2) gains `agent_runs_swept_total`.

## Impact

- **Code**: `backend/agent_py/agent/run_metadata.py` (heartbeat touch, conditional writes, sweep query, retention returns deleted ids), `backend/agent_py/migrations/versions/0003_*.py` (new), `backend/agent_py/agent/main.py` (heartbeat task in the SSE generator; sweeper wiring; checkpointer passed to both crons), `backend/agent_py/agent/sweeper.py` (new), `backend/agent_py/agent/retention.py` (GC hook), `backend/agent_py/agent/config.py` (settings + validators).
- **Wire**: none — no new SSE frame, no new error code on the wire (`stale_run` is a DB `error_code` only). Old clients are unaffected.
- **Dependencies**: none new. `adelete_thread` is the existing `AsyncPostgresSaver`.
- **Docs**: `FUTURE_AGENT.md`, `AgentArchitecture.md`, `AgentFaultTolerance.md`, `backend/agent_py/README.md`, `backend/agent_py/.env.example`.
- **Tests**: `backend/agent_py/tests/test_run_metadata.py`, new `test_sweeper.py`, `test_config.py`, `test_run.py` / `test_smoke_run_state.py` (heartbeat coverage).
- **Rollout gates tracked in** `openspec/changes/agent-stale-run-sweeper-rollout/` (per the rollout-split convention; dev-stack kill-9 smoke, post-deploy observation, rollback drill).
