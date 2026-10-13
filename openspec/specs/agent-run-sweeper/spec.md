# agent-run-sweeper Specification

## Purpose
Liveness heartbeats plus a stale-run reaper so that crashed or killed agent processes cannot leak `running` metadata rows or langGraph checkpoint data forever, and so the window in which a crashed run is still resumable is bounded and explicit.

## Requirements

### Requirement: Live runs carry a heartbeat

While a run is in flight, the service SHALL update the run's `last_heartbeat_at` at least once per `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (default 30), starting at run creation (fresh) or resume and stopping when the SSE stream terminates. Each heartbeat update SHALL be conditional on the row still being `running`. A heartbeat write failure SHALL be non-fatal: the service MUST log it and continue the run — it MUST NOT emit an SSE error frame, terminate the stream, or alter the run's terminal metadata outcome.

#### Scenario: Heartbeat advances while a run streams

- **WHEN** a run remains in flight for longer than `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS`
- **THEN** the run's `last_heartbeat_at` advances at least once per interval for the duration of the stream

#### Scenario: Heartbeat write failure does not fail the run

- **WHEN** a heartbeat UPDATE raises (transient DB error)
- **THEN** a warning is logged, no SSE error frame is emitted, and the run continues to completion

#### Scenario: Heartbeat stops at terminal state

- **WHEN** a run reaches a terminal state (the stream emits `done` or terminates with `error`)
- **THEN** no further heartbeat writes are attempted for that run

### Requirement: Stale running rows are reaped

The service SHALL run a sweep at least once per `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS` (default 300) that transitions every row matching `status='running' AND last_heartbeat_at < now() - PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` (default 900) to `status='failed'`, `error_code='stale_run'`, `ended_at=now()`. The transition SHALL be conditional on the row still being `running` at update time. A sweep iteration failure SHALL be logged and MUST NOT kill the sweep task — subsequent iterations still run. When `PYTHON_AGENT_SWEEP_ENABLED=false` the service MUST NOT reap any rows (heartbeat writes continue).

#### Scenario: Zombie row is reaped after the timeout

- **WHEN** a row has `status='running'` and `last_heartbeat_at` older than `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS`
- **THEN** the next sweep marks it `status='failed'`, `error_code='stale_run'`, `ended_at=now()`

#### Scenario: Live and terminal rows are untouched

- **WHEN** a `running` row's `last_heartbeat_at` is within the timeout, or a row is already terminal
- **THEN** the sweep does not modify it

#### Scenario: Reap is race-safe

- **WHEN** a row transitions to a terminal state between the sweep's scan and its update
- **THEN** the conditional update matches no row and the terminal state is preserved unchanged

#### Scenario: Sweeper disabled by config

- **WHEN** `PYTHON_AGENT_SWEEP_ENABLED=false`
- **THEN** no rows are reaped even when stale; heartbeat writes still advance

#### Scenario: Sweep iteration failure is contained

- **WHEN** a sweep iteration raises (e.g. the database is briefly unreachable)
- **THEN** the failure is logged and the next interval's iteration still runs

### Requirement: Sweep outcome is observable

Each sweep iteration SHALL produce an OTel span `agent.run.sweep` carrying the number of reaped rows as an attribute, and SHALL increment the Prometheus counter `agent_runs_swept_total` by the reaped count. An iteration that reaps zero rows SHALL still produce the span (with count 0) and MUST NOT increment the counter.

#### Scenario: Reaped rows are counted

- **WHEN** a sweep reaps N rows
- **THEN** the `agent.run.sweep` span carries `swept_count=N` and `agent_runs_swept_total` increases by N

#### Scenario: Zero-reap iterations are visible

- **WHEN** a sweep runs and finds no stale rows
- **THEN** the span exists with `swept_count=0` and the counter is unchanged

### Requirement: The sweep threshold bounds the resume window

A crashed run's `running` row remains resumable via the existing resume path (echoed `run_id` → rehydrate from the langGraph checkpoint → `note{kind:"resumed"}`) until the sweeper reaps it. After reaping, the row is terminal and the existing terminal-row rule applies: resume mints a fresh run and emits `note{kind:"fresh_run"}` without reading checkpoint state for the reaped thread. Reaping MUST NOT introduce any new SSE frame kind or wire error code.

#### Scenario: Resume inside the window still rehydrates

- **WHEN** a run's process died and its heartbeat is stale but the row has not yet been reaped
- **THEN** `POST /agent/run` with the echoed `run_id` resumes from the last checkpoint and emits `note{kind:"resumed"}` as today

#### Scenario: Resume after a reap is treated as terminal

- **WHEN** `POST /agent/run` echoes the `run_id` of a row already reaped (`status='failed'`, `error_code='stale_run'`)
- **THEN** the service mints a fresh `run_id`, emits `note{kind:"fresh_run"}`, and does not call `graph.get_state` for the reaped thread

#### Scenario: A stream closed before `done` stays resumable

- **WHEN** the client disconnects (or the stream is cancelled) before the run emits `done`
- **THEN** the metadata row stays `running` (it is NOT marked `completed`) — the run remains resumable by echoing its `run_id` inside the window, and the sweeper reaps it if it is never resumed

#### Scenario: No wire change

- **WHEN** any sweep occurs
- **THEN** the set of SSE frame kinds and error codes observable by clients is exactly the set that existed before this capability

### Requirement: Reaped runs' checkpoint threads are preserved until retention

Reaping a run SHALL NOT delete its langGraph checkpoint data. The sweeper MUST NOT call `adelete_thread` (or any checkpoint-deleting API); a reaped run's `langgraph.checkpoint_*` rows are collected only when the retention cron deletes the run's now-terminal metadata row, so a crashed run's checkpoint survives the same retention window as any other terminal run.

#### Scenario: Reap preserves the checkpoint thread

- **WHEN** a stale row is reaped (`status='failed'`, `error_code='stale_run'`)
- **THEN** the `langgraph.checkpoint_*` rows for `thread_id = run_id` still exist

#### Scenario: Reaped rows are collected at retention like any other terminal run

- **WHEN** the retention cron runs after a reaped row's `ended_at` has aged past the retention window
- **THEN** the row is deleted and its checkpoint thread is deleted in the same iteration (see `agent-run-store-py` — retention GC requirement)

### Requirement: Terminal writes remain authoritative over a premature reap

Heartbeat updates and sweep reaps SHALL be conditional on `status='running'`. `mark_completed` and `mark_failed` SHALL update the row unconditionally — the live process knows the run's true outcome — and `mark_completed` SHALL clear `error_code`, so a row that was reaped while its process was still alive (stalled heartbeat) and then genuinely completed ends `status='completed'`, `error_code IS NULL`.

#### Scenario: Completion after a premature reap wins

- **WHEN** a `running` row is reaped (stale heartbeat) and the still-alive process then completes the run and calls `mark_completed`
- **THEN** the row ends `status='completed'`, `ended_at` from the completion write, and `error_code IS NULL`

#### Scenario: Terminal state survives a racing reap

- **WHEN** the sweep's conditional update fires after the run already drove its row terminal
- **THEN** the row's terminal `status` and `error_code` are preserved unchanged
