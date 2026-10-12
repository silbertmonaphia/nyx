# Spec Delta: Agent Python Service

## Purpose

Additive delta on the existing `agent-python-service` capability: extend the `POST /agent/run` request/response surface to mint and resume `run_id`s against the new `agent-run-store-py` capability. State persistence itself is delegated to langGraph's `AsyncPostgresSaver` (compiled into the `StateGraph` via `checkpointer=...`). The orchestrator's behavior, the existing SSE frame kinds, the existing error codes, and the network boundary are unchanged; only three new constructs are introduced — an optional `run_id` request field, a `run_id` SSE frame emitted first on fresh runs, and `note{kind:"resumed"}` / `note{kind:"fresh_run"}` SSE frames around the resume decision.

## ADDED Requirements

### Requirement: Request body carries an optional `run_id`

The `POST /agent/run` request body SHALL accept an optional `run_id: string` field. When present, it MUST parse as a uuid4; values that do not parse as uuid4 SHALL be rejected with HTTP 400 before the orchestrator runs. When absent, the service SHALL mint a fresh `run_id` (uuid4) on the server side and persist it as part of the new run. Old clients that omit the field continue to behave as today — each request gets a fresh run.

#### Scenario: Valid `run_id` is accepted

- **WHEN** `POST /agent/run` arrives with `run_id` set to a valid uuid4 string
- **THEN** the request is accepted, the service attempts to resume, and HTTP 200 opens the SSE stream

#### Scenario: Malformed `run_id` is rejected

- **WHEN** `POST /agent/run` arrives with `run_id` set to a string that does not parse as uuid4
- **THEN** the service returns HTTP 400 with `detail` naming the field and the orchestrator does not run

#### Scenario: Missing `run_id` is accepted

- **WHEN** `POST /agent/run` arrives without a `run_id` field
- **THEN** the service mints a fresh `run_id`, persists the new run, and HTTP 200 opens the SSE stream

### Requirement: Fresh runs emit `run_id` as the first SSE frame

When the service mints a fresh `run_id`, the SSE stream SHALL emit `run_id` as the first frame (before any other frame including `plan`). The frame shape SHALL be `event: run_id\ndata: {"run_id": "<uuid>"}\n\n`. Old clients that ignore unknown event kinds continue to render the existing `plan` / `step_*` / `delta` / `done` / `terminator` frames unchanged.

#### Scenario: First frame of a fresh run is `run_id`

- **WHEN** the orchestrator mints a new `run_id` for a fresh run
- **THEN** the SSE stream's first frame is `event: run_id\ndata: {"run_id": "<uuid>"}\n\n`, followed by the existing `plan`, `step_*`, `done`, `terminator` frames in their original order

#### Scenario: Old clients ignore the `run_id` frame

- **WHEN** a client that only parses `plan` / `step_*` / `delta` / `done` / `note` / `error` / `terminator` subscribes
- **THEN** the `run_id` frame arrives at the start of the stream but is ignored; the rest of the stream renders unchanged

### Requirement: Resumed runs emit `note{kind:"resumed"}` before the plan

When `POST /agent/run` arrives with a `run_id` whose `agent.runs` row exists, is owned by the request's `user_id`, and is non-terminal (`status NOT IN ('completed', 'failed')`), the service SHALL build `config = {"configurable": {"thread_id": str(run_id)}}`, call `graph.get_state(config)` to retrieve the latest `AgentState` from langGraph's `PostgresSaver`, and use it as the executor's initial state. The SSE stream SHALL emit `event: note\ndata: {"kind": "resumed", "last_step_id": "<id>"}\n\n` immediately before the `plan` frame, and the `plan` / `step_*` / `done` / `terminator` frames SHALL cover only what remains from the resumed state — completed nodes are NOT re-executed. When `RunMetadata.get_run(run_id, user_id)` returns `None` (not found, wrong user, or terminal), the service SHALL mint a fresh `run_id` and emit `event: note\ndata: {"kind": "fresh_run", "run_id": "<uuid>"}\n\n` instead, then proceed with the new plan.

#### Scenario: Resume rehydrates from langGraph's last checkpoint

- **WHEN** `POST /agent/run` arrives with a `run_id` whose `agent.runs` row is `status='running'` and langGraph's checkpoint at `thread_id=<run_id>` is mid-graph
- **THEN** the SSE stream emits `note{kind:"resumed"}` with `last_step_id` pointing to the last checkpointed node, then the `plan` frame, then only the remaining step's frames; prior steps are not re-executed (token usage confirms no double-spend)

#### Scenario: Cross-user resume attempt is treated as fresh

- **WHEN** `POST /agent/run` arrives with a `run_id` whose `agent.runs` row exists but with a different `user_id` from the request's `X-Nyx-User-Id`
- **THEN** `RunMetadata.get_run(run_id, other_user_id)` returns `None` and the service mints a fresh `run_id`, emitting `note{kind:"fresh_run"}` followed by the new plan; the existing run is left untouched and no checkpoint data is read

#### Scenario: Terminal metadata row attempt is treated as fresh

- **WHEN** `POST /agent/run` arrives with a `run_id` whose `agent.runs` row is `status='completed'` or `'failed'`
- **THEN** `RunMetadata.get_run` returns `None` and the service mints a fresh `run_id`, emitting `note{kind:"fresh_run"}` followed by the new plan; no checkpoint data is read

### Requirement: New error code `run_metadata_unavailable` for metadata layer failures

The existing `error` frame SHALL be allowed to carry a new `code` value: `run_metadata_unavailable`. This code MUST be emitted when the orchestrator cannot write to the `agent.runs` metadata table (via `RunMetadata`) after the retry budget is exhausted. Old error codes (`plan_too_large`, `react_iterations_exceeded`, `budget_exceeded`, `budget_unavailable`, `agent_unavailable`) SHALL remain valid and unchanged. Emitting `run_metadata_unavailable` SHALL NOT terminate the live run if the failure is non-fatal — the run MUST continue its plan via langGraph's `PostgresSaver`; only the metadata row update is lost for the current request.

#### Scenario: Sustained metadata failure emits the typed error

- **WHEN** `RunMetadata` raises `RunMetadataUnavailable` after the retry budget is exhausted during a terminal event write
- **THEN** the SSE stream emits `event: error\ndata: {"code": "run_metadata_unavailable"}\n\n`, and the run continues its plan (subsequent metadata writes are best-effort)

#### Scenario: langGraph checkpoint failure still terminates the run

- **WHEN** langGraph's `PostgresSaver` cannot write a checkpoint after the framework's retry budget
- **THEN** the SSE stream emits the existing `event: error\ndata: {"code": "agent_unavailable"}\n\n` and the run terminates — in-memory state cannot be trusted without the checkpoint landing

#### Scenario: Old error codes still work

- **WHEN** an `error` frame is emitted with `code` in the existing code set
- **THEN** the wire shape is byte-for-byte identical to the pre-change stream

### Requirement: New env keys for run-state persistence and retention

The service SHALL read `PYTHON_AGENT_DATABASE_URL` (required when `AGENT_ENABLED=true`), `PYTHON_AGENT_RETENTION_DAYS` (default 30), `PYTHON_AGENT_RETENTION_INTERVAL_SECONDS` (default 3600), and `PYTHON_AGENT_RETENTION_ENABLED` (default true). The service SHALL refuse to start when `AGENT_ENABLED=true` and `PYTHON_AGENT_DATABASE_URL` is unset, with a clear error naming the missing key. `PYTHON_AGENT_DATABASE_URL` MUST include `options=-c search_path=langgraph,agent,public`; the service SHALL refuse to start otherwise. The same DSN is consumed by langGraph's `AsyncPostgresSaver.from_conn_string(..., schema_name="langgraph")` and by the `RunMetadata` connection pool.

#### Scenario: Missing `PYTHON_AGENT_DATABASE_URL` refuses startup

- **WHEN** `AGENT_ENABLED=true` and `PYTHON_AGENT_DATABASE_URL` is unset
- **THEN** the service fails to start with a clear error naming the missing key

#### Scenario: DSN without `search_path` refuses startup

- **WHEN** `PYTHON_AGENT_DATABASE_URL` is set but does not include `options=-c search_path=langgraph,agent,public`
- **THEN** the service fails to start with a clear error naming the missing DSN option

#### Scenario: Retention env defaults applied when unset

- **WHEN** the service starts with `PYTHON_AGENT_RETENTION_DAYS` and `PYTHON_AGENT_RETENTION_INTERVAL_SECONDS` unset
- **THEN** retention runs every 3600 seconds and deletes metadata rows older than 30 days; langGraph checkpoint tables are pruned via langGraph's own retention story and are NOT affected by this cron
