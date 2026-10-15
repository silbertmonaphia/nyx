# Agent Run Store (Python) Specification

## Purpose

Adds Postgres-backed persistence for Plan-and-Execute agent runs so that a backend or Python-service restart mid-run does not lose already-spent tokens or in-flight progress. Runs survive process restarts and resume from the last completed step when the client echoes the `run_id`. Per-user scoping is enforced at the SQL layer as defence in depth even though the upstream `X-Nyx-User-Id` header is set from a verified JWT.

State persistence is delegated to **langGraph's `AsyncPostgresSaver`** (built-in `thread_id` / `checkpoint_id` semantics, mid-iteration `react` resume). langGraph ships a hardcoded MIGRATIONS list (no alembic) and applies it via `await AsyncPostgresSaver.setup()` at app startup. A **thin `agent.runs` metadata table** owned by us covers the user-facing queries langGraph's checkpoint blob doesn't expose (`list_runs_for_user`, retention cron over `ended_at`, `total_tokens` / `error_code` aggregates). The two layers live in disjoint Postgres schemas: `langgraph.*` for langGraph's checkpoint tables, `agent.*` for our metadata. Both are separate from the Go backend's `public` schema. Schema versioning for the `agent` schema is owned by alembic (SQLAlchemy Core only — no ORM models). Data access for the metadata layer uses raw `asyncpg`. The Go chat proxy, RAG bridge, MCP, tool registry, and provider seam are not changed.

## Requirements

### Requirement: Three Postgres schemas with disjoint ownership

The store SHALL persist across three Postgres schemas: `public` (Go-owned, unchanged), `langgraph.*` (langGraph framework-owned, holds langGraph's `checkpoint_*` tables), and `agent.*` (Python agent-owned, holds `agent.runs` and `agent.alembic_version`). The Postgres DSN SHALL include `options=-c search_path=langgraph,agent` so unqualified table names in the Python service resolve in priority order (langGraph first, ours second). `public` is NOT in the search_path because the Python service never reads Go's tables. Go-side queries SHALL NOT be modified to set `search_path`; Go continues to operate against `public`.

#### Scenario: langGraph checkpoint tables live under `langgraph.*`, not `agent.*` or `public.*`

- **WHEN** the agent service starts and `await AsyncPostgresSaver.setup()` runs after the `0001` alembic revision has created the `langgraph` schema
- **THEN** langGraph's checkpoint tables exist in `langgraph.*`; `agent.*` is created by `0002` and does not contain langGraph's tables; `public.*` is untouched

#### Scenario: Our metadata table lives under `agent.*`, separate from langGraph's

- **WHEN** alembic upgrade head completes (which applies our `0002_runs_metadata.py`)
- **THEN** `agent.runs` exists; `langgraph.checkpoint_*` is untouched; both schemas coexist with disjoint ownership

#### Scenario: DSN without `search_path` is rejected at startup

- **WHEN** `PYTHON_AGENT_DATABASE_URL` is configured without `options=-c search_path=langgraph,agent`
- **THEN** the agent service refuses to start with a clear error naming the missing DSN option

### Requirement: State graph is compiled with `AsyncPostgresSaver`

The compiled `StateGraph` SHALL carry `checkpointer=AsyncPostgresSaver.from_conn_string(database_url)`. langGraph's `from_conn_string` accepts no `schema_name` kwarg; schema placement is via (a) the DSN's `options=-c search_path=langgraph,agent` and (b) the pre-existing `langgraph` schema created by our `0001` alembic revision. When `await saver.setup()` runs at app startup, langGraph's hardcoded MIGRATIONS list executes `CREATE TABLE IF NOT EXISTS checkpoint_*` against the connection, resolving to `langgraph.checkpoint_*` per the search_path. The graph's `thread_id` SHALL be our `run_id` (uuid4). Per-step persistence SHALL be automatic via langGraph's saver on every node transition; the agent SHALL NOT maintain a separate `agent.steps` table or call `RunMetadata.append_step` for step boundaries.

#### Scenario: Compiled graph carries the saver

- **WHEN** `agent/graph.py` builds the state graph
- **THEN** the compiled graph's `checkpointer` is an instance of `AsyncPostgresSaver`

#### Scenario: Per-node transitions produce Postgres checkpoints

- **WHEN** the graph executes a node (planner, executor, react subgraph)
- **THEN** langGraph writes a checkpoint row to `langgraph.checkpoint_*` for that `thread_id`, and a later `graph.get_state(config={"configurable":{"thread_id": run_id}})` returns the latest checkpointed state

### Requirement: Alembic owns schema versioning in the agent schema

Schema migrations SHALL be managed by alembic with SQLAlchemy **Core only** (no ORM models, no `declarative_base`). `alembic_version` SHALL live in the `agent` schema via `version_table_schema='agent'`. Migration scripts SHALL use `op.execute(...)` and `op.create_table(..., schema="agent")` bodies. Migration `0001_langgraph_checkpoint` SHALL create the `langgraph` schema (idempotent `CREATE SCHEMA IF NOT EXISTS langgraph`); langGraph's checkpoint tables are created later by `await AsyncPostgresSaver.setup()` at app startup and are NOT versioned by alembic. Migration `0002_runs_metadata` SHALL create our metadata table in `agent.*`. The Go backend's golang-migrate chain (`backend/migrations/`) SHALL NOT be extended with agent or langGraph tables.

#### Scenario: Alembic upgrade applies our metadata table; langGraph's tables appear after `setup()`

- **WHEN** `alembic upgrade head` is run against an empty database followed by app startup (`await AsyncPostgresSaver.setup()`)
- **THEN** `langgraph.checkpoint_*` (langGraph's tables, no version table), `agent.runs`, and `agent.alembic_version` exist; `alembic check` reports `No new upgrade operations detected`

#### Scenario: Alembic downgrade removes our schema; full reset requires manual drop of `langgraph`

- **WHEN** `alembic downgrade base` is run after a successful upgrade
- **THEN** `agent.runs` and `agent.alembic_version` no longer exist; the `langgraph` schema is NOT removed by alembic (langGraph owns those tables) — a full reset requires an explicit `DROP SCHEMA langgraph CASCADE`

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

### Requirement: Resume uses `graph.get_state(config)` for state rehydration

When `POST /agent/run` arrives with a `run_id` whose `agent.runs` row exists, is owned by the request's `user_id`, and is non-terminal, the agent SHALL build `config = {"configurable": {"thread_id": str(run_id)}}`, call `graph.get_state(config)`, and use the returned `AgentState` as the executor's initial state. The agent SHALL emit `note{kind:"resumed", "last_step_id": "<id>"}` before `plan`. If `get_run` returns `None` (not found, wrong user, or terminal), the agent SHALL mint a fresh `run_id` and emit `note{kind:"fresh_run"}` — it SHALL NOT call `graph.get_state` for a foreign or terminal `run_id`.

#### Scenario: Resume rehydrates from langGraph's last checkpoint

- **WHEN** `POST /agent/run` arrives with a `run_id` whose `agent.runs` row is `status='running'` and the langGraph checkpoint at `thread_id=<run_id>` is mid-graph
- **THEN** `graph.get_state(config)` returns the latest `AgentState` and the executor continues from the next node without re-executing prior nodes (token usage confirms no double-spend)

#### Scenario: Cross-user resume attempt is treated as fresh

- **WHEN** `POST /agent/run` arrives with a `run_id` whose `agent.runs` row exists but with a different `user_id` from the request's `X-Nyx-User-Id`
- **THEN** `get_run(run_id, other_user_id)` returns `None` and the agent mints a fresh `run_id`, emitting `note{kind:"fresh_run"}`; the existing run is left untouched and no checkpoint data is read for it

#### Scenario: Terminal metadata row attempt is treated as fresh

- **WHEN** `POST /agent/run` arrives with a `run_id` whose `agent.runs` row is `status='completed'` or `'failed'`
- **THEN** `get_run` returns `None` and the agent mints a fresh `run_id`, emitting `note{kind:"fresh_run"}`; no checkpoint data is read

### Requirement: `RunMetadata.list_runs_for_user` returns paginated history

`list_runs_for_user(user_id, limit, offset) -> list[RunSummary]` SHALL SELECT `id, status, started_at, ended_at, total_tokens, error_code` from `agent.runs WHERE user_id = $1 ORDER BY started_at DESC LIMIT $2 OFFSET $3`. The result SHALL NOT include rows for any other `user_id`. Index `(user_id, started_at DESC)` SHALL back the query.

#### Scenario: Listing paginates and scopes to one user

- **WHEN** `list_runs_for_user(user_A, limit=10, offset=0)` is called and there are 25 runs for user A and 3 for user B
- **THEN** the result contains 10 rows, all with `user_id = user_A`, ordered by `started_at DESC`, with no user-B rows

### Requirement: Per-user scoping on every metadata read

Every metadata read SHALL include `user_id` in the SQL `WHERE` clause. No read method SHALL accept a `user_id` parameter it does not filter on. The defence-in-depth posture is documented: the upstream `X-Nyx-User-Id` header is the primary authentication, but the SQL filter is the contract. langGraph's `thread_id` is server-minted (uuid4) so its value space is not enumerable; cross-user checkpoint reads are not reachable via this path.

#### Scenario: SQL filter is mandatory

- **WHEN** `get_run(run_id, user_id)` is called
- **THEN** the SQL issued is `... WHERE id = $1 AND user_id = $2` (verified via test fixture capturing the emitted SQL)

### Requirement: Transient metadata errors retry; sustained failures translate to `run_metadata_unavailable`

`asyncpg.PostgresConnectionError` and other transient errors SHALL trigger 3-retry exponential backoff (100ms, 200ms, 400ms). If retries exhaust, the call SHALL raise `RunMetadataUnavailable`. Pool acquire timeout SHALL be 5s; exceeding it SHALL raise `RunMetadataUnavailable`. The SSE consumer SHALL translate `RunMetadataUnavailable` to `error{code:"run_metadata_unavailable"}` and continue the run (the run completes its plan via langGraph's saver; only the metadata table update is lost for that request). langGraph checkpoint write failures are a separate failure mode and translate to the existing `error{code:"agent_unavailable"}` (the run terminates — the right posture when the in-memory state can't be persisted).

#### Scenario: Transient connection error retry

- **WHEN** the underlying TCP connection drops once during a metadata write
- **THEN** the call retries with the next backoff interval and the run completes without `run_metadata_unavailable`

#### Scenario: Sustained metadata unavailability translates to a typed error event

- **WHEN** the connection stays down for longer than the retry budget
- **THEN** `RunMetadataUnavailable` is raised, caught by the SSE consumer, and `event: error\ndata: {"code": "run_metadata_unavailable"}\n\n` is emitted; the run continues its plan via langGraph

#### Scenario: langGraph checkpoint failure translates to `agent_unavailable`

- **WHEN** the `PostgresSaver` cannot write a checkpoint after the framework's retry budget
- **THEN** the SSE stream emits `event: error\ndata: {"code": "agent_unavailable"}\n\n` and the run terminates (the in-memory state can't be trusted without the checkpoint landing)

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

### Requirement: Direct asyncpg + langGraph saver connections, no Go HTTP bridge

The Python service SHALL hold its own `asyncpg.Pool` (for `RunMetadata`) and langGraph-managed `AsyncPostgresSaver` connection. Both connect directly to the Postgres compose service (`db:5432`). No Go HTTP bridge mediates Python→DB traffic for run state. The Go chat proxy (`internal/chat/agent_proxy.go`) still mediates `/api/chat` → `/agent/run` HTTP traffic; this is an HTTP proxy, not a DB bridge. RAG retrieval and MCP tool dispatch continue to flow through their existing Go HTTP bridges (`/internal/rag/retrieve`, `/mcp` JSON-RPC); only run-state writes use the direct connection.

#### Scenario: Compose wires the agent to db directly

- **WHEN** `docker compose up` runs with `AGENT_ENABLED=true` and `PYTHON_AGENT_DATABASE_URL` set
- **THEN** the `agent` service reaches `db:5432` over the bridge network without an HTTP intermediary for either metadata writes or langGraph checkpoint writes
