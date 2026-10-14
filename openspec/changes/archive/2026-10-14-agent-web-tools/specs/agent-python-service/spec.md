# Spec Delta

## MODIFIED Requirements

### Requirement: Step kinds cover the agent surface

The orchestrator SHALL support step kinds `rag` (vector retrieval over feeds via the RAG bridge), `tool` (tool call — an enabled agent-native tool when the emitted name matches one, otherwise a registry call via MCP `tools/call`), `llm` (single model call), and `react` (nested model-tool-model loop with iteration cap). The planner MAY emit any combination of these kinds in a single plan.

#### Scenario: Mixed-kind plan runs in order

- **WHEN** the planner returns steps of kinds `[rag, tool, llm]`
- **THEN** the executor runs them in that order, each emits its own `step_start`/`step_end` pair, and each tool step dispatches to the agent-native implementation when its name is an enabled web tool and otherwise through MCP's `tools/call` JSON-RPC endpoint

#### Scenario: Nested ReAct caps iterations

- **WHEN** a `react` step runs and the model emits tool calls
- **THEN** the loop iterates model → tool → model until either the model emits a final answer OR the configured iteration cap (`PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS`) is hit; exceeding the cap emits `step_end{status:"failed",reason:"react_max_iterations"}` and `error{code:"react_iterations_exceeded"}`

## ADDED Requirements

### Requirement: New env keys for web tools

The service SHALL read `PYTHON_AGENT_WEB_FETCH_ENABLED` (default false), `PYTHON_AGENT_WEB_SEARCH_ENABLED` (default false), `PYTHON_AGENT_TAVILY_API_KEY` (optional — an empty key selects the provider's keyless tier; `PYTHON_AGENT_TAVILY_BASE_URL` default `https://api.tavily.com/search`), `PYTHON_AGENT_WEB_SEARCH_MAX_RESULTS` (default 5, clamped to the hard cap of 10), `PYTHON_AGENT_WEB_FETCH_MAX_BYTES` (default 2097152), `PYTHON_AGENT_WEB_FETCH_TIMEOUT_SECONDS` (default 10), `PYTHON_AGENT_WEB_FETCH_MAX_CHARS` (default 20000), `PYTHON_AGENT_WEB_FETCH_MAX_REDIRECTS` (default 5), and `PYTHON_AGENT_WEB_FETCH_ALLOW_PRIVATE` (default false; relaxes the private-address refusal and the 80/443 port restriction for local fixture servers — same posture as the Go backend's `LLM_ALLOW_PRIVATE_URL`). The service SHALL refuse to start when any numeric web-tool setting is negative. Search without an API key is valid configuration (keyless tier) and MUST NOT refuse startup.

#### Scenario: Defaults disable both tools

- **WHEN** the service starts with no web-tool env vars set
- **THEN** `PYTHON_AGENT_WEB_FETCH_ENABLED=false` and `PYTHON_AGENT_WEB_SEARCH_ENABLED=false` are applied and the service starts

#### Scenario: Keyless search boots without a key

- **WHEN** `PYTHON_AGENT_WEB_SEARCH_ENABLED=true` and `PYTHON_AGENT_TAVILY_API_KEY` is unset or empty
- **THEN** the service starts cleanly and search dispatches against the provider's keyless tier

#### Scenario: Negative numeric setting refuses startup

- **WHEN** any of `PYTHON_AGENT_WEB_SEARCH_MAX_RESULTS`, `PYTHON_AGENT_WEB_FETCH_MAX_BYTES`, `PYTHON_AGENT_WEB_FETCH_TIMEOUT_SECONDS`, `PYTHON_AGENT_WEB_FETCH_MAX_CHARS`, or `PYTHON_AGENT_WEB_FETCH_MAX_REDIRECTS` is negative
- **THEN** the service refuses to start with a clear error naming the offending env var

#### Scenario: Private-address opt-in is explicit

- **WHEN** `PYTHON_AGENT_WEB_FETCH_ALLOW_PRIVATE=true` is set
- **THEN** the fetch guard permits private/loopback targets (intended for local fixture servers in dev and e2e) and the relaxed posture is logged at startup
