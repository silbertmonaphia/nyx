# Spec Delta

## MODIFIED Requirements

### Requirement: `RunMetadata.create_run` mints a `run_id` and persists a metadata row

The metadata layer SHALL mint a `run_id` (uuid4) on `create_run(user_id) -> uuid`, INSERT a row into `agent.runs`, and return the new uuid. The row SHALL carry `status='running'`, `started_at=now()`, `last_heartbeat_at=now()`, `user_id` from the upstream header, `total_tokens=0`, `error_code=NULL`, `ended_at=NULL`. The row does NOT carry `plan_json` — that lives in langGraph's checkpoint blob.

#### Scenario: Fresh run inserts a metadata row with the right shape

- **WHEN** the agent mints a `run_id` and calls `RunMetadata.create_run(user_id)`
- **THEN** `agent.runs` contains one row with `id = <uuid>`, `user_id = <user_id>`, `status = 'running'`, `started_at` within the last second, `last_heartbeat_at` within the last second, `ended_at IS NULL`, `total_tokens = 0`

### Requirement: `RunMetadata.mark_completed` and `mark_failed` set terminal metadata state

`mark_completed(run_id, total_tokens)` SHALL set `agent.runs.status='completed'`, `ended_at=now()`, `total_tokens=<int>`, and SHALL clear `error_code` to NULL. `mark_failed(run_id, error_code)` SHALL set `agent.runs.status='failed'`, `ended_at=now()`, `error_code=<code>`. Both writes SHALL be unconditional and authoritative: the live process knows the run's true outcome, so a terminal write SHALL overwrite a previous `error_code='stale_run'` written by the sweeper against a stalled heartbeat. Once a run's own live path has driven the row terminal, no further lifecycle writes occur for it. **These methods update the metadata only**; langGraph's checkpoint lifecycle is managed by the saver (and by the sweeper / retention GC paths, which delete threads for runs that no longer need them).

#### Scenario: Completed metadata row carries the cumulative token total

- **WHEN** `mark_completed(run_id, total_tokens=12345)` is called
- **THEN** `agent.runs` row for `run_id` has `status='completed'`, `ended_at IS NOT NULL`, `total_tokens = 12345`, `error_code IS NULL`

#### Scenario: Completion overrides a stale-run reap

- **WHEN** a row was previously reaped by the sweeper (`status='failed'`, `error_code='stale_run'`) but the run's process was actually still alive and then completes, calling `mark_completed`
- **THEN** the row ends `status='completed'`, `error_code IS NULL`, `total_tokens` from the completion write

### Requirement: Retention cron hard-deletes terminal metadata rows after N days

An `asyncio` task SHALL run every `PYTHON_AGENT_RETENTION_INTERVAL_SECONDS` (default 3600s) and execute `DELETE FROM agent.runs WHERE ended_at < now() - interval 'PYTHON_AGENT_RETENTION_DAYS day' AND status IN ('completed', 'failed')`. The task SHALL wrap each iteration in OTel span `agent.run.retention` with `deleted_count` attribute and increment Prometheus counter `agent_run_retention_deleted_total` by the deleted count. The task SHALL start on FastAPI lifespan startup and stop cleanly on shutdown. `PYTHON_AGENT_RETENTION_DAYS` defaults to 30; `PYTHON_AGENT_RETENTION_ENABLED=true` enables it (default). For every deleted run, the task SHALL also delete the run's langGraph checkpoint thread via the checkpointer's `adelete_thread(run_id)` — langGraph ships no retention story of its own, so this path is the **only** place `langgraph.checkpoint_*` is pruned (the stale-run sweeper deliberately preserves checkpoint data; see `agent-run-sweeper`). A per-run `adelete_thread` failure SHALL be logged and MUST NOT abort the remaining deletes.

#### Scenario: Retention deletes only terminal metadata rows older than the window

- **WHEN** `agent.runs` contains: 1 running row, 2 completed rows from 60 days ago, 1 completed row from 1 day ago, 1 failed row from 60 days ago
- **THEN** after retention runs with `RETENTION_DAYS=30`, exactly 3 rows are deleted (the 2 completed-old + 1 failed-old), leaving the running and the recent completed rows

#### Scenario: Retention skips in-flight runs and leaves langGraph checkpoint tables alone

- **WHEN** `agent.runs` contains a row with `status='running'` and `ended_at IS NULL`
- **THEN** retention does not delete it regardless of `started_at`, and no `langgraph.checkpoint_*` rows are deleted for its thread

#### Scenario: Retention deletes the deleted runs' checkpoint threads

- **WHEN** retention deletes terminal rows
- **THEN** for each deleted run, `adelete_thread(run_id)` removes its `langgraph.checkpoint_*` rows in the same iteration

#### Scenario: Checkpoint-deletion failure is contained

- **WHEN** `adelete_thread` raises for one of the deleted runs
- **THEN** the failure is logged, the metadata deletion for that run still stands, and the remaining runs in the iteration are still processed
