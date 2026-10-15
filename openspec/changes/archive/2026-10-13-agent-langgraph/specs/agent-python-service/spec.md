# Spec Delta

## Purpose

Hosts the Plan-and-Execute agent orchestrator as a Python service so the Go backend can stay thin and the orchestrator can use mature state-graph primitives (LangGraph) and Pydantic models without forcing the same patterns into the rest of the Go stack.

## ADDED Requirements

### Requirement: Service exposes Plan-and-Execute over SSE

The service SHALL expose `POST /agent/run` that accepts a chat request (messages, optional model override) and returns a Server-Sent Events response whose frame kinds — `plan`, `step_start`, `tool_call`, `tool_result`, `step_end`, `delta`, `done`, `note`, `error`, `terminator` — and whose wire envelope (`event: <kind>\ndata: <json>\n\n`) are byte-for-byte identical to what the Go chat handler emits today for the agent path.

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

The service SHALL read `PYTHON_AGENT_LLM_BASE_URL`, `PYTHON_AGENT_LLM_API_KEY`, `PYTHON_AGENT_LLM_MODEL`, `PYTHON_AGENT_LLM_PLANNER_MODEL` (defaults to `PYTHON_AGENT_LLM_MODEL` when empty), `PYTHON_AGENT_PLAN_MAX_STEPS` (default 10), `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS` (default 5), `PYTHON_AGENT_MCP_URL` (default `http://backend:8080/mcp`), `PYTHON_AGENT_RAG_BRIDGE_URL` (default `http://backend:8080/internal/rag/retrieve`), `PYTHON_AGENT_REDIS_URL` (default `redis://redis:6379`), `PYTHON_AGENT_OTEL_ENDPOINT` (default `http://jaeger:4318`), `PYTHON_AGENT_OTEL_SERVICE_NAME` (default `nyx-agent`), `PYTHON_AGENT_METRICS_PORT` (default 9001) from environment. **No JWT verification config is read**: the service trusts the network boundary and uses the `X-Nyx-User-Id` header for identity. The service SHALL refuse to start when required keys are missing and the corresponding feature is enabled.

#### Scenario: Default values applied when env unset

- **WHEN** the service starts with no env vars set
- **THEN** `PYTHON_AGENT_PLAN_MAX_STEPS=10`, `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS=5`, `PYTHON_AGENT_MCP_URL=http://backend:8080/mcp`, `PYTHON_AGENT_RAG_BRIDGE_URL=http://backend:8080/internal/rag/retrieve` are applied and the service starts

#### Scenario: Required key missing refuses startup (OpenAI-proper only)

- **WHEN** `PYTHON_AGENT_LLM_BASE_URL` targets `api.openai.com` but `PYTHON_AGENT_LLM_API_KEY` is empty
- **THEN** the service fails to start with a clear error naming the missing key (every LLM call would 401; refuse to boot)
- Local vLLM / OpenRouter / proxied hosts are NOT covered by this guard — empty key is allowed when the base URL does not start with `https://api.openai.com/`

### Requirement: Health endpoint for compose

The service SHALL expose `GET /healthz` returning HTTP 200 with body `{"status":"ok"}` when the service is ready to accept requests. The check SHALL fail when the LLM provider is unreachable AND the budget Redis client is unreachable (so compose restarts the container rather than serving a broken orchestrator).

#### Scenario: Healthz succeeds when dependencies healthy

- **WHEN** the LLM provider and Redis are reachable
- **THEN** `GET /healthz` returns HTTP 200 with `{"status":"ok"}`

#### Scenario: Healthz fails when Redis unreachable

- **WHEN** the Redis client fails its first ping
- **THEN** `GET /healthz` returns HTTP 503 and compose restarts the container