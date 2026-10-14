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

The following env vars are added by the user-preferences change:
- `PYTHON_AGENT_PREFS_ENABLED` (default true) — kill-switch for preference extraction AND planner injection; when false, no extraction task fires and the planner prompt omits the prefs block
- `PYTHON_AGENT_PREFS_MAX_INJECTED` (default 50) — maximum preferences injected into the planner prompt (top-N by `updated_at DESC`); MUST be ≥ 0

The service SHALL refuse to start when `PYTHON_AGENT_PREFS_MAX_INJECTED` is negative.

#### Scenario: Default values applied when env unset

- **WHEN** the service starts with no env vars set
- **THEN** `PYTHON_AGENT_PLAN_MAX_STEPS=10`, `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS=5`, `PYTHON_AGENT_MCP_URL=http://backend:8080/mcp`, `PYTHON_AGENT_RAG_BRIDGE_URL=http://backend:8080/internal/rag/retrieve`, `PYTHON_AGENT_PLANNER_RETRY_MAX=1`, `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=true`, `PYTHON_AGENT_TOOL_RETRY_MAX=2`, `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS=30`, `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS=900`, `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS=300`, `PYTHON_AGENT_SWEEP_ENABLED=true`, `PYTHON_AGENT_CHAT_RETENTION_DAYS=365`, `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS=7`, `PYTHON_AGENT_CHAT_RETENTION_ENABLED=true`, `PYTHON_AGENT_SESSIONS_PAGE_SIZE=10`, `PYTHON_AGENT_PREFS_ENABLED=true`, `PYTHON_AGENT_PREFS_MAX_INJECTED=50` are applied and the service starts

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

#### Scenario: Negative prefs cap refuses startup

- **WHEN** `PYTHON_AGENT_PREFS_MAX_INJECTED=-1` is set
- **THEN** startup raises `ValueError: PYTHON_AGENT_PREFS_MAX_INJECTED must be >= 0; got -1` and the process exits

#### Scenario: Prefs kill switch disturbs nothing at startup

- **WHEN** `PYTHON_AGENT_PREFS_ENABLED=false` is set
- **THEN** the service starts normally; no extraction task is scheduled on any run and the planner prompt builder omits the prefs block
