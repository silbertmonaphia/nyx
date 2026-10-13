# Agent Python Service Specification

## Purpose

Hosts the Plan-and-Execute agent orchestrator as a Python service so the Go backend can stay thin and the orchestrator can use mature state-graph primitives (LangGraph) and Pydantic models without forcing the same patterns into the rest of the Go stack.

## Requirements

### Requirement: Service exposes Plan-and-Execute over SSE

The service SHALL expose `POST /agent/run` that accepts a chat request (messages, optional model override, user identifier from the verified JWT) and returns a Server-Sent Events response whose frame kinds — `plan`, `step_start`, `tool_call`, `tool_result`, `step_end`, `delta`, `done`, `note`, `error`, `terminator` — and whose wire envelope (`event: <kind>\ndata: <json>\n\n`) are byte-for-byte identical to what the Go chat handler emits today for the agent path.

#### Scenario: Frame kinds and envelope match Go agent path

- **WHEN** the orchestrator emits an event of any kind
- **THEN** the SSE frame is `event: <kind>\ndata: <json>\n\n` where `<kind>` is one of the ten enumerated kinds and the JSON payload is the typed event payload (same field names the Go handler emits)

#### Scenario: Old clients still see unchanged token stream

- **WHEN** a client that only parses `delta` / `done` / `note` / `error` / `terminator` subscribes
- **THEN** those frames arrive in their original wire shape and the client renders the token stream unchanged

### Requirement: Plan and steps carry stable identifiers

The planner SHALL emit a JSON plan with shape `{"steps":[{"id":"step_1","kind":"...","tool":"...","args":{...},"prompt":"...","query":"...","max_iterations":3}, ...]}` where `id` is mandatory and unique within the plan, and the executor SHALL emit one `step_start`/`step_end` frame pair per step carrying the same `step_id`. A client that filters by `step_id` SHALL reconstruct each step's events in the correct order even when frames for two steps arrive interleaved due to async tool dispatch.

#### Scenario: Identifiers survive interleaving

- **WHEN** the executor emits `step_start(step_2)` then `tool_call(step_3)` then `step_end(step_2)` due to async tool dispatch
- **THEN** a client that filters by step_id reconstructs step_2's events and step_3's events independently

### Requirement: Step kinds cover the agent surface

The orchestrator SHALL support step kinds `rag` (vector retrieval over feeds via the RAG bridge), `tool` (registry call via MCP `tools/call`), `llm` (single model call), and `react` (nested model-tool-model loop with iteration cap). The planner MAY emit any combination of these kinds in a single plan.

#### Scenario: Mixed-kind plan runs in order

- **WHEN** the planner returns steps of kinds `[rag, tool, llm]`
- **THEN** the executor runs them in that order, each emits its own `step_start`/`step_end` pair, and tools dispatch through MCP's `tools/call` JSON-RPC endpoint

#### Scenario: Nested ReAct caps iterations

- **WHEN** a `react` step runs and the model emits tool calls
- **THEN** the loop iterates model → tool → model until either the model emits a final answer OR the configured iteration cap (`PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS`) is hit; exceeding the cap emits `step_end{status:"failed",reason:"react_max_iterations"}` and `error{code:"react_iterations_exceeded"}`

### Requirement: Step execution is bounded

The orchestrator SHALL enforce `PYTHON_AGENT_PLAN_MAX_STEPS` (default 10) on every run. A plan exceeding the cap MUST be rejected before execution with a typed `error{code:"plan_too_large"}` event. The orchestrator SHALL NOT silently truncate plans.

#### Scenario: Oversized plan rejected

- **WHEN** the planner returns a plan with more steps than `PYTHON_AGENT_PLAN_MAX_STEPS`
- **THEN** the orchestrator emits `error{code:"plan_too_large"}` and no steps execute

#### Scenario: ReAct iteration cap enforced

- **WHEN** a `react` step exceeds `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS`
- **THEN** the step terminates with `step_end{status:"failed",reason:"react_max_iterations"}` and the run terminates with `error{code:"react_iterations_exceeded"}`

### Requirement: Per-step SSE frames preserve the existing chat wire contract

The orchestrator SHALL emit SSE frames in this order over a run: one `plan` frame, then for each step one `step_start`, zero or more `tool_call` / `tool_result` / `delta` / `note` frames, then one `step_end`, then one `done`, then one `terminator`. The existing `delta`, `done`, `note`, `terminator` shapes MUST match the Go publisher. The `error` frame MAY carry new `code` values; old codes remain valid.

#### Scenario: New client renders intermediate steps

- **WHEN** a client subscribes and parses every event kind
- **THEN** it sees `plan` first, then per-step frames, then `done`, then `terminator`, and the user sees tool names + final reply in order

### Requirement: Caller supplies verified user id via header

The service SHALL require the `X-Nyx-User-Id` request header on `/agent/run` and SHALL reject any request without it with HTTP 400 before invoking the orchestrator. The value MUST parse as an integer; non-integer values are rejected with HTTP 400. The Python service SHALL NOT verify JWTs — the network boundary (compose bridge / k8s NetworkPolicy restricting ingress to the backend) is the trust boundary at this hop. The `Authorization` bearer token is forwarded as a passthrough to the MCP and RAG bridges, which re-verify the JWT themselves.

#### Scenario: Missing header rejected

- **WHEN** `POST /agent/run` arrives without `X-Nyx-User-Id`
- **THEN** the service returns HTTP 400 with `detail` mentioning the header name and the orchestrator does not run

#### Scenario: Non-integer header value rejected

- **WHEN** `POST /agent/run` arrives with `X-Nyx-User-Id` that does not parse as an integer
- **THEN** the service returns HTTP 400 with `detail` explaining the requirement

#### Scenario: User id read from header, not body

- **WHEN** a request arrives with a valid `X-Nyx-User-Id` header
- **THEN** the orchestrator uses that value (not anything from the request body) for per-owner RAG retrieval, tool dispatch via MCP, and the budget counter key

### Requirement: Configurable via environment

The service SHALL read `PYTHON_AGENT_LLM_BASE_URL`, `PYTHON_AGENT_LLM_API_KEY`, `PYTHON_AGENT_LLM_MODEL`, `PYTHON_AGENT_LLM_PLANNER_MODEL` (defaults to `PYTHON_AGENT_LLM_MODEL` when empty), `PYTHON_AGENT_PLAN_MAX_STEPS` (default 10), `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS` (default 5), `PYTHON_AGENT_MCP_URL` (default `http://backend:8080/mcp`), `PYTHON_AGENT_RAG_BRIDGE_URL` (default `http://backend:8080/internal/rag/retrieve`), `PYTHON_AGENT_REDIS_URL` (default `redis://redis:6379`), `PYTHON_AGENT_OTEL_ENDPOINT` (default `http://jaeger:4318`), `PYTHON_AGENT_OTEL_SERVICE_NAME` (default `nyx-agent`), `PYTHON_AGENT_METRICS_PORT` (default 9001), `PYTHON_AGENT_PLANNER_RETRY_MAX` (default 1, must be ≥ 0), `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED` (default true), and optional `PYTHON_AGENT_JWT_SECRET` / `PYTHON_AGENT_JWT_ALGORITHM` / `PYTHON_AGENT_JWT_AUDIENCE` / `PYTHON_AGENT_JWT_ISSUER` from environment. The service SHALL refuse to start when required keys are missing and the corresponding feature is enabled. The service SHALL refuse to start when `PYTHON_AGENT_PLANNER_RETRY_MAX` is set to a negative value.

#### Scenario: Default values applied when env unset

- **WHEN** the service starts with no env vars set
- **THEN** `PYTHON_AGENT_PLAN_MAX_STEPS=10`, `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS=5`, `PYTHON_AGENT_MCP_URL=http://backend:8080/mcp`, `PYTHON_AGENT_RAG_BRIDGE_URL=http://backend:8080/internal/rag/retrieve`, `PYTHON_AGENT_PLANNER_RETRY_MAX=1`, `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=true` are applied and the service starts

#### Scenario: Required key missing refuses startup

- **WHEN** `PYTHON_AGENT_REDIS_URL` is unset and the budget guard is enabled
- **THEN** the service fails to start with a clear error naming the missing key

#### Scenario: Negative planner-retry max refuses startup

- **WHEN** `PYTHON_AGENT_PLANNER_RETRY_MAX=-1` is set
- **THEN** the service refuses to start with a clear error naming the offending env var; no port is bound

#### Scenario: Free-text planner path reachable via flag

- **WHEN** `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false` is set
- **THEN** the planner routes through the legacy free-text path (fence-stripping + `json.loads`); the structured binding is not invoked

### Requirement: Health endpoint for compose

The service SHALL expose `GET /healthz` returning HTTP 200 with body `{"status":"ok"}` when the service is ready to accept requests. The check SHALL fail when the LLM provider is unreachable AND the budget Redis client is unreachable (so compose restarts the container rather than serving a broken orchestrator).

#### Scenario: Healthz succeeds when dependencies healthy

- **WHEN** the LLM provider and Redis are reachable
- **THEN** `GET /healthz` returns HTTP 200 with `{"status":"ok"}`

#### Scenario: Healthz fails when Redis unreachable

- **WHEN** the Redis client fails its first ping
- **THEN** `GET /healthz` returns HTTP 503 and compose restarts the container

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

The service SHALL read `PYTHON_AGENT_DATABASE_URL` (required when `AGENT_ENABLED=true`), `PYTHON_AGENT_RETENTION_DAYS` (default 30), `PYTHON_AGENT_RETENTION_INTERVAL_SECONDS` (default 3600), and `PYTHON_AGENT_RETENTION_ENABLED` (default true). The service SHALL refuse to start when `AGENT_ENABLED=true` and `PYTHON_AGENT_DATABASE_URL` is unset, with a clear error naming the missing key. `PYTHON_AGENT_DATABASE_URL` MUST include `options=-c search_path=langgraph,agent`; the service SHALL refuse to start otherwise. The same DSN is consumed by langGraph's `AsyncPostgresSaver.from_conn_string(settings.database_url)` and by the `RunMetadata` connection pool. Schema placement for langGraph's tables is via the DSN search_path and the pre-existing `langgraph` schema created by the `0001` alembic revision; langGraph's `from_conn_string` accepts no `schema_name` kwarg.

#### Scenario: Missing `PYTHON_AGENT_DATABASE_URL` refuses startup

- **WHEN** `AGENT_ENABLED=true` and `PYTHON_AGENT_DATABASE_URL` is unset
- **THEN** the service fails to start with a clear error naming the missing key

#### Scenario: DSN without `search_path` refuses startup

- **WHEN** `PYTHON_AGENT_DATABASE_URL` is set but does not include `options=-c search_path=langgraph,agent`
- **THEN** the service fails to start with a clear error naming the missing DSN option

#### Scenario: Retention env defaults applied when unset

- **WHEN** the service starts with `PYTHON_AGENT_RETENTION_DAYS` and `PYTHON_AGENT_RETENTION_INTERVAL_SECONDS` unset
- **THEN** retention runs every 3600 seconds and deletes metadata rows older than 30 days; langGraph checkpoint tables are pruned via langGraph's own retention story and are NOT affected by this cron
