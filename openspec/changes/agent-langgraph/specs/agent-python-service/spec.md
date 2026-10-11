# Spec Delta

## Purpose

Hosts the Plan-and-Execute agent orchestrator as a Python service so the Go backend can stay thin and the orchestrator can use mature state-graph primitives (LangGraph) and Pydantic models without forcing the same patterns into the rest of the Go stack.

## ADDED Requirements

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

### Requirement: Authenticated callers only

The service SHALL require a Bearer access token in the `Authorization` header on `/agent/run` and SHALL reject any request without one with HTTP 401 before invoking the orchestrator. The user identifier used for per-owner scoping and budget enforcement SHALL be the identifier embedded in the verified JWT (not the request body).

#### Scenario: Missing token rejected

- **WHEN** `POST /agent/run` arrives without `Authorization: Bearer <token>`
- **THEN** the service returns HTTP 401 and the orchestrator does not run

#### Scenario: User id read from verified JWT

- **WHEN** a request arrives with a valid Bearer token
- **THEN** the orchestrator reads the user identifier from the token claims (not from the request body's `user_id` field) and uses it for per-owner RAG retrieval, tool dispatch via MCP, and the budget counter key

### Requirement: Configurable via environment

The service SHALL read `PYTHON_AGENT_LLM_BASE_URL`, `PYTHON_AGENT_LLM_API_KEY`, `PYTHON_AGENT_LLM_MODEL`, `PYTHON_AGENT_LLM_PLANNER_MODEL` (defaults to `PYTHON_AGENT_LLM_MODEL` when empty), `PYTHON_AGENT_PLAN_MAX_STEPS` (default 10), `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS` (default 5), `PYTHON_AGENT_MCP_URL` (default `http://backend:8080/mcp`), `PYTHON_AGENT_RAG_BRIDGE_URL` (default `http://backend:8080/internal/rag/retrieve`), `PYTHON_AGENT_REDIS_URL` (default `redis://redis:6379`), `PYTHON_AGENT_OTEL_ENDPOINT` (default `http://jaeger:4318`), `PYTHON_AGENT_OTEL_SERVICE_NAME` (default `nyx-agent`), `PYTHON_AGENT_METRICS_PORT` (default 9001), and optional `PYTHON_AGENT_JWT_SECRET` / `PYTHON_AGENT_JWT_ALGORITHM` / `PYTHON_AGENT_JWT_AUDIENCE` / `PYTHON_AGENT_JWT_ISSUER` from environment. The service SHALL refuse to start when required keys are missing and the corresponding feature is enabled.

#### Scenario: Default values applied when env unset

- **WHEN** the service starts with no env vars set
- **THEN** `PYTHON_AGENT_PLAN_MAX_STEPS=10`, `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS=5`, `PYTHON_AGENT_MCP_URL=http://backend:8080/mcp`, `PYTHON_AGENT_RAG_BRIDGE_URL=http://backend:8080/internal/rag/retrieve` are applied and the service starts

#### Scenario: Required key missing refuses startup

- **WHEN** `PYTHON_AGENT_REDIS_URL` is unset and the budget guard is enabled
- **THEN** the service fails to start with a clear error naming the missing key

### Requirement: Health endpoint for compose

The service SHALL expose `GET /healthz` returning HTTP 200 with body `{"status":"ok"}` when the service is ready to accept requests. The check SHALL fail when the LLM provider is unreachable AND the budget Redis client is unreachable (so compose restarts the container rather than serving a broken orchestrator).

#### Scenario: Healthz succeeds when dependencies healthy

- **WHEN** the LLM provider and Redis are reachable
- **THEN** `GET /healthz` returns HTTP 200 with `{"status":"ok"}`

#### Scenario: Healthz fails when Redis unreachable

- **WHEN** the Redis client fails its first ping
- **THEN** `GET /healthz` returns HTTP 503 and compose restarts the container