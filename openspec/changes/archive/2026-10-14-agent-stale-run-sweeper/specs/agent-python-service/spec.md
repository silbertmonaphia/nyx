# Spec Delta

## MODIFIED Requirements

### Requirement: Configurable via environment

The service SHALL read `PYTHON_AGENT_LLM_BASE_URL`, `PYTHON_AGENT_LLM_API_KEY`, `PYTHON_AGENT_LLM_MODEL`, `PYTHON_AGENT_LLM_PLANNER_MODEL` (defaults to `PYTHON_AGENT_LLM_MODEL` when empty), `PYTHON_AGENT_PLAN_MAX_STEPS` (default 10), `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS` (default 5), `PYTHON_AGENT_MCP_URL` (default `http://backend:8080/mcp`), `PYTHON_AGENT_RAG_BRIDGE_URL` (default `http://backend:8080/internal/rag/retrieve`), `PYTHON_AGENT_REDIS_URL` (default `redis://redis:6379`), `PYTHON_AGENT_OTEL_ENDPOINT` (default `http://jaeger:4318`), `PYTHON_AGENT_OTEL_SERVICE_NAME` (default `nyx-agent`), `PYTHON_AGENT_METRICS_PORT` (default 9001), `PYTHON_AGENT_PLANNER_RETRY_MAX` (default 1, must be ≥ 0), `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED` (default true), `PYTHON_AGENT_TOOL_RETRY_MAX` (default 2, must be ≥ 0), `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (default 30, must be ≥ 1), `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` (default 900, must be ≥ 2× the heartbeat interval), `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS` (default 300, must be ≥ 1), `PYTHON_AGENT_SWEEP_ENABLED` (default true), and optional `PYTHON_AGENT_JWT_SECRET` / `PYTHON_AGENT_JWT_ALGORITHM` / `PYTHON_AGENT_JWT_AUDIENCE` / `PYTHON_AGENT_JWT_ISSUER` from environment. The service SHALL refuse to start when required keys are missing and the corresponding feature is enabled. The service SHALL refuse to start when `PYTHON_AGENT_PLANNER_RETRY_MAX` or `PYTHON_AGENT_TOOL_RETRY_MAX` is set to a negative value, when `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` or `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS` is below 1, or when `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` is smaller than twice `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (such a ratio would let the sweeper reap live runs).

#### Scenario: Default values applied when env unset

- **WHEN** the service starts with no env vars set
- **THEN** `PYTHON_AGENT_PLAN_MAX_STEPS=10`, `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS=5`, `PYTHON_AGENT_MCP_URL=http://backend:8080/mcp`, `PYTHON_AGENT_RAG_BRIDGE_URL=http://backend:8080/internal/rag/retrieve`, `PYTHON_AGENT_PLANNER_RETRY_MAX=1`, `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=true`, `PYTHON_AGENT_TOOL_RETRY_MAX=2`, `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS=30`, `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS=900`, `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS=300`, `PYTHON_AGENT_SWEEP_ENABLED=true` are applied and the service starts

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
