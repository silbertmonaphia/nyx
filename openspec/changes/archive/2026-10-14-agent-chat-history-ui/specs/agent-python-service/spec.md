# Spec Delta

## Purpose

(see existing capability)

## MODIFIED Requirements

### Requirement: Configurable via environment

The service SHALL read `PYTHON_AGENT_LLM_BASE_URL`, `PYTHON_AGENT_LLM_API_KEY`, `PYTHON_AGENT_LLM_MODEL`, `PYTHON_AGENT_LLM_PLANNER_MODEL` (defaults to `PYTHON_AGENT_LLM_MODEL` when empty), `PYTHON_AGENT_PLAN_MAX_STEPS` (default 10), `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS` (default 5), `PYTHON_AGENT_MCP_URL` (default `http://backend:8080/mcp`), `PYTHON_AGENT_RAG_BRIDGE_URL` (default `http://backend:8080/internal/rag/retrieve`), `PYTHON_AGENT_REDIS_URL` (default `redis://redis:6379`), `PYTHON_AGENT_OTEL_ENDPOINT` (default `http://jaeger:4318`), `PYTHON_AGENT_OTEL_SERVICE_NAME` (default `nyx-agent`), `PYTHON_AGENT_METRICS_PORT` (default 9001), `PYTHON_AGENT_PLANNER_RETRY_MAX` (default 1, must be ≥ 0), `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED` (default true), `PYTHON_AGENT_TOOL_RETRY_MAX` (default 2, must be ≥ 0), `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (default 30, must be ≥ 1), `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` (default 900, must be ≥ 2× the heartbeat interval), `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS` (default 300, must be ≥ 1), `PYTHON_AGENT_SWEEP_ENABLED` (default true), `PYTHON_AGENT_LANGFUSE_ENABLED` (default false), `PYTHON_AGENT_LANGFUSE_HOST` (host-dev default `http://localhost:3000`; the compose agent service overrides to the in-network `http://langfuse-web:3000`), `PYTHON_AGENT_LANGFUSE_PUBLIC_KEY`, `PYTHON_AGENT_LANGFUSE_SECRET_KEY`, and optional `PYTHON_AGENT_JWT_SECRET` / `PYTHON_AGENT_JWT_ALGORITHM` / `PYTHON_AGENT_JWT_AUDIENCE` / `PYTHON_AGENT_JWT_ISSUER` from environment. The service SHALL refuse to start when required keys are missing and the corresponding feature is enabled. The service SHALL refuse to start when `PYTHON_AGENT_PLANNER_RETRY_MAX` or `PYTHON_AGENT_TOOL_RETRY_MAX` is set to a negative value, when `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` or `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS` is below 1, or when `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` is smaller than twice `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (such a ratio would let the sweeper reap live runs). The service SHALL refuse to start when `PYTHON_AGENT_LANGFUSE_ENABLED=true` and `PYTHON_AGENT_LANGFUSE_PUBLIC_KEY` or `PYTHON_AGENT_LANGFUSE_SECRET_KEY` is unset.

The following env vars are added by the chat-history storage change:
- `PYTHON_AGENT_CHAT_RETENTION_DAYS` (default 365) — retention floor for `agent.chat_sessions` and `agent.chat_messages`; MUST be ≥ 365
- `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS` (default 7) — number of days a soft-deleted row is retained before hard-delete; MUST be ≥ 0
- `PYTHON_AGENT_CHAT_RETENTION_ENABLED` (default true) — kill-switch for the chat retention cron; when false, no rows are deleted

The service SHALL refuse to start when `PYTHON_AGENT_CHAT_RETENTION_DAYS` is set below 365, or when `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS` is negative.

The following env vars are added by the chat-history UI change:
- `PYTHON_AGENT_SESSIONS_PAGE_SIZE` (default 10) — default page size for `GET /agent/sessions`; MUST be in `[1, 50]`

The service SHALL refuse to start when `PYTHON_AGENT_SESSIONS_PAGE_SIZE` is outside `[1, 50]`.

#### Scenario: Default values applied when env unset

- **WHEN** the service starts with no env vars set
- **THEN** `PYTHON_AGENT_PLAN_MAX_STEPS=10`, `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS=5`, `PYTHON_AGENT_MCP_URL=http://backend:8080/mcp`, `PYTHON_AGENT_RAG_BRIDGE_URL=http://backend:8080/internal/rag/retrieve`, `PYTHON_AGENT_PLANNER_RETRY_MAX=1`, `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=true`, `PYTHON_AGENT_TOOL_RETRY_MAX=2`, `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS=30`, `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS=900`, `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS=300`, `PYTHON_AGENT_SWEEP_ENABLED=true`, `PYTHON_AGENT_CHAT_RETENTION_DAYS=365`, `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS=7`, `PYTHON_AGENT_CHAT_RETENTION_ENABLED=true`, `PYTHON_AGENT_SESSIONS_PAGE_SIZE=10` are applied and the service starts

#### Scenario: Required key missing refuses startup

- **WHEN** `PYTHON_AGENT_REDIS_URL` is unset and the budget guard is enabled
- **THEN** the service fails to start with a clear error naming the missing key

#### Scenario: Negative planner-retry max refuses startup

- **WHEN** `PYTHON_AGENT_PLANNER_RETRY_MAX=-1` is set
- **THEN** the service refuses to start with a clear error naming the offending env var; no port is bound

#### Scenario: Negative tool-retry max refuses startup

- **WHEN** `PYTHON_AGENT_TOOL_RETRY_MAX=-1` is set
- **THEN** the service refuses to start with a clear error naming the offending env var; no port is bound

#### Scenario: Misconfigured sweep ratio refuses startup

- **WHEN** `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` is set below 2× `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (e.g. timeout 45 with the default heartbeat 30)
- **THEN** the service refuses to start with a clear error naming both env vars

#### Scenario: Free-text planner path reachable via flag

- **WHEN** `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false` is set
- **THEN** the planner routes through the legacy free-text path (fence-stripping + `json.loads`); the structured binding is not invoked

#### Scenario: Langfuse enabled without credentials refuses startup

- **WHEN** `PYTHON_AGENT_LANGFUSE_ENABLED=true` is set with `PYTHON_AGENT_LANGFUSE_PUBLIC_KEY` or `PYTHON_AGENT_LANGFUSE_SECRET_KEY` unset
- **THEN** the service refuses to start with a clear error naming the missing key; no port is bound

#### Scenario: Langfuse defaults applied when unset

- **WHEN** the service starts with no Langfuse env vars set
- **THEN** `PYTHON_AGENT_LANGFUSE_ENABLED=false` and `PYTHON_AGENT_LANGFUSE_HOST=http://localhost:3000` are applied, no trace client is constructed, and the service starts

#### Scenario: Below-floor retention refuses startup

- **WHEN** `PYTHON_AGENT_CHAT_RETENTION_DAYS=180` is set
- **THEN** startup raises `ValueError: PYTHON_AGENT_CHAT_RETENTION_DAYS must be >= 365; got 180` and the process exits

#### Scenario: Negative grace period refuses startup

- **WHEN** `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS=-1` is set
- **THEN** startup raises `ValueError: PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS must be >= 0; got -1` and the process exits

#### Scenario: Kill switch honoured at startup

- **WHEN** `PYTHON_AGENT_CHAT_RETENTION_ENABLED=false` is set
- **THEN** the chat retention cron is not constructed and no `agent_chat_retention_deleted_total` increments ever occur (the counter exists in the registry but is never written)

#### Scenario: Page size out of range refuses startup

- **WHEN** `PYTHON_AGENT_SESSIONS_PAGE_SIZE=0` (or 51) is set
- **THEN** startup raises `ValueError: PYTHON_AGENT_SESSIONS_PAGE_SIZE must be in [1, 50]` and the process exits


## ADDED Requirements

### Requirement: Session continuation on echoed session ids

When `POST /agent/run` arrives with a `run_id` matching a live (non-deleted) `chat_sessions` row owned by the requesting user, the service SHALL treat the request as a continuation of that session: the SSE `run_id` frame SHALL echo the same id; the turn's new user message and the assistant reply SHALL be appended to that session; and the service SHALL NOT create or mutate an `agent.runs` row for this execution (a session may have no runs row at all until its first turn, and a terminal runs row must never be mutated). A non-terminal `agent.runs` row for the id takes priority — crash-recovery resume semantics are unchanged. Unknown ids, another user's ids, and soft-deleted sessions SHALL fall through to a freshly minted session. Only the LAST user message of the request is appended on a continuation (the SPA re-sends the session's full history every turn; earlier messages already exist). There is no idle-time session chopping: the client's `+ New chat` is the only session-boundary signal.

#### Scenario: Continuation appends without duplication

- **WHEN** turn 2 of a session arrives echoing the session id with history `[u1, a1, u2]`
- **THEN** the SSE `run_id` frame echoes the same id; the session gains exactly one `u2` row and one new assistant row; `u1`/`a1` are not re-recorded; no second `agent.runs` row exists for the session

#### Scenario: API-created session receives its first turn

- **WHEN** a session was created via `POST /agent/sessions` (no `agent.runs` row) and its first chat echoes its id
- **THEN** the turn is appended to it and no `agent.runs` row is minted for the execution

#### Scenario: Old sessions keep their thread

- **WHEN** a session whose last activity is 30 days old is echoed
- **THEN** the turn is appended to it — there is no idle-time chopping

#### Scenario: Unknown and cross-user ids mint fresh

- **WHEN** the echoed id is unknown, owned by another user, or soft-deleted
- **THEN** the service mints a fresh session id for the turn; the other session (if any) is left untouched

### Requirement: Terminal persistence survives client disconnect

The SSE generator's terminal persistence (the assistant message row, the session `ended_at` stamp, and the `agent.runs` terminal transition for non-continuation runs) SHALL complete even when the client disconnects at or after the `terminator` frame. The writes SHALL run in a task shielded from generator cancellation so a fully-streamed answer can never lose its stored assistant half or leave its run row stuck `running`.

#### Scenario: Disconnect at terminator still persists the answer

- **WHEN** a client closes the stream immediately after the `terminator` frame (the SPA parser's normal close point) while the terminal writes are mid-flight
- **THEN** the assistant row lands in `chat_messages`, the session's `ended_at` is stamped, and the run row reaches `completed` — none of them depend on the client staying connected

#### Scenario: Mid-run disconnect keeps the resumable semantics

- **WHEN** a client disconnects before the run reaches `done`
- **THEN** no assistant row is written and the run row stays `running` (resumable inside the stale-run window; the sweeper reaps it otherwise) — unchanged semantics
