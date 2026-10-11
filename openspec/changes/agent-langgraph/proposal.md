# Proposal: Agent LangGraph (Python)

## Why

The first OpenSpec change `agent-loop` shipped a Plan-and-Execute agent loop in Go (`internal/agent/`). After shipping it, the project owner has decided the agent orchestration layer should live in Python (LangGraph + Pydantic) instead — LangGraph's mature state-graph primitives, checkpointing, and ecosystem are a better fit for the orchestrator than a hand-rolled Go loop. **RAG stays in Go unchanged.** Tools stay in Go (the `internal/tool/` registry and its MCP shim). The provider seam stays in Go (its `OpenAI`-compatible wire is consumed directly by LangGraph's chat-model clients). The chat SSE wire contract stays unchanged — the Go chat handler becomes a thin SSE proxy that streams Python agent events to the existing frontend envelope.

## What Changes

- **REMOVED**: `backend/internal/agent/` (the Go Plan-and-Execute package — planner, executor, budget guard, observability, metrics, types, all tests). The chat handler stops importing it. `backend/.env.example` agent section moves to a Python service `.env`.
- **NEW** `backend/agent_py/` — Python service: FastAPI + LangGraph + Pydantic + pydantic-settings + redis-py + opentelemetry-python + prometheus-client. Endpoints:
  - `POST /agent/run` — accepts `{messages, agent?, model?, user_id}` (Bearer JWT forwarded from Go), emits SSE in the same envelope as the Go chat handler emits today (`event: <kind>\ndata: <json>\n\n` for kinds `plan`, `step_start`, `tool_call`, `tool_result`, `step_end`, `delta`, `done`, `note`, `error`, `terminator`).
  - `GET /healthz` — liveness for compose.
- **NEW** `backend/internal/chat/agent_proxy.go` — Go HTTP client that, when `AGENT_ENABLED=true`, posts to `PYTHON_AGENT_URL/agent/run` and streams the response SSE byte-for-byte back to the chat handler. When `AGENT_ENABLED=false`, today's single-turn path runs unchanged.
- **NEW** `backend/internal/rag/http.go` — one tiny endpoint at `GET /internal/rag/retrieve?query=…&user_id=…` (Bearer JWT required, per-user scoping enforced) that wraps `rag.Service.Retrieve`. Python calls it instead of reimplementing RAG.
- **NEW** `backend/agent_py/compose service` — Docker compose adds an `agent` service with the python image, network-isolated, depended on via `condition: service_healthy`. Health check hits `/healthz`.
- **BREAKING (Go agent removal)**: every test in `backend/internal/agent/*_test.go` is deleted alongside the package. The chat handler's `AGENT_ENABLED=true` branch no longer calls Go code — it proxies HTTP.

## Capabilities

### New Capabilities

- `agent-python-service`: The Python service implements the Plan-and-Execute agent. LangGraph state graph with planner node + executor loop over step kinds (`rag` / `tool` / `llm` / `react`) and nested ReAct inside `react` steps capped by `AGENT_STEP_REACT_MAX_ITERATIONS`. Pydantic models (`Plan`, `PlanStep`, `StepKind`, `Event` discriminated union, `ChatRequest`, `ChatMessage`, `BudgetDecision`) are the wire format. Endpoints: `POST /agent/run` (SSE), `GET /healthz`. Config via `pydantic-settings` reading `PYTHON_AGENT_*` env vars. Defaults match the Go `AGENT_*` defaults that move here.
- `agent-budget-guard-py`: Per-user hourly token budget guard in Python. Redis-backed counter `agent:budget:u={user_id}:h={utc_hour}` (UTC hour format) with TTL 90 minutes. Consulted before every `LLM.chat` call inside the loop. Fails closed on Redis unavailability (`agent_unavailable` SSE error). `agent.budget` OTel span with attributes `user_id`, `tokens_used`, `tokens_remaining`, `decision`. Cap from `AGENT_BUDGET_PER_USER_PER_HOUR` env (default 200000).
- `agent-rag-bridge`: A small Go HTTP endpoint that exposes `rag.Service.Retrieve` to the Python service. Authenticated with the same Bearer JWT the chat uses; per-user scoping enforced server-side. Python calls this rather than reimplementing RAG. Tiny request: `{query: str, user_id: int}`. Tiny response: `{context_block: str, citations?: [{feed_id, snippet}]}`. Lives at `GET /internal/rag/retrieve`.
- `chat-agent-proxy`: The Go chat handler, when `AGENT_ENABLED=true`, posts the chat request to `PYTHON_AGENT_URL/agent/run` (with the user's Bearer token forwarded in the `Authorization` header) and streams the Python service's SSE body byte-for-byte to the client. When `AGENT_ENABLED=false`, the existing single-turn path runs unchanged. The wire envelope produced for the client is identical in both cases.

### Modified Capabilities

None. No existing capabilities have requirement-level changes. (`internal/llm`, `internal/tool`, `internal/rag`, `internal/mcp`, `internal/chat`'s wire contract all keep their shape.)

## Impact

- **Code removed**: `backend/internal/agent/` (entire package — `service.go`, `planner.go`, `executor.go`, `executor_react.go`, `executor_tool.go`, `budget.go`, `types.go`, `observability.go`, `metrics.go`; tests `service_test.go`, `planner_test.go`, `executor_test.go`, `observability_test.go`, `metrics_test.go`, `stubs_test.go`, `service_integration_test.go`). Approximately 2,000 LOC of Go gone.
- **Code added**:
  - `backend/internal/chat/agent_proxy.go` (~150 LOC) — HTTP client + SSE proxy + circuit breaker / fail-closed on `PYTHON_AGENT_URL` unavailability.
  - `backend/internal/rag/http.go` (~80 LOC) — handler that wraps `rag.Service.Retrieve`.
  - `backend/agent_py/` — Python service: `pyproject.toml`, `Dockerfile`, `agent/__init__.py`, `agent/main.py` (FastAPI), `agent/graph.py` (LangGraph state graph), `agent/planner.py`, `agent/executor.py`, `agent/budget.py`, `agent/types.py` (Pydantic), `agent/config.py` (pydantic-settings), `agent/observability.py`, `agent/tools_client.py` (HTTP client for MCP `tools/call`), `agent/rag_client.py` (HTTP client for `/internal/rag/retrieve`), `tests/`.
- **Dependencies**:
  - Go: none new. (HTTP client via stdlib `net/http`.)
  - Python: `fastapi`, `uvicorn[standard]`, `langgraph`, `langchain-openai`, `pydantic`, `pydantic-settings`, `redis`, `opentelemetry-api`, `opentelemetry-sdk`, `opentelemetry-exporter-otlp-proto-http`, `prometheus-client`, `httpx` (test), `pytest`, `pytest-asyncio`, `vcrpy` (test).
- **Config (env, additive only)**:
  - Python service reads: `PYTHON_AGENT_PORT` (default 9000), `PYTHON_AGENT_LLM_BASE_URL`, `PYTHON_AGENT_LLM_API_KEY`, `PYTHON_AGENT_LLM_MODEL`, `PYTHON_AGENT_LLM_PLANNER_MODEL` (default `LLM_MODEL`), `PYTHON_AGENT_PLAN_MAX_STEPS` (default 10), `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS` (default 5), `AGENT_BUDGET_PER_USER_PER_HOUR` (default 200000), `PYTHON_AGENT_REDIS_URL` (default `redis://redis:6379`), `PYTHON_AGENT_MCP_URL` (default `http://backend:8080/mcp`), `PYTHON_AGENT_RAG_BRIDGE_URL` (default `http://backend:8080/internal/rag/retrieve`), `PYTHON_AGENT_OTEL_ENDPOINT` (default `http://jaeger:4318`), `PYTHON_AGENT_OTEL_SERVICE_NAME` (default `nyx-agent`), `PYTHON_AGENT_METRICS_PORT` (default 9001), `PYTHON_AGENT_JWT_SECRET` / `PYTHON_AGENT_JWT_ALGORITHM` / `PYTHON_AGENT_JWT_AUDIENCE` / `PYTHON_AGENT_JWT_ISSUER` (all optional).
  - Go backend reads: `AGENT_ENABLED` (default false), `PYTHON_AGENT_URL` (default `http://agent:9000`). Keeps `LLM_FALLBACK_TOOLS_ENABLED` semantics.
- **API**: `/api/chat` request body unchanged. Response SSE unchanged for old clients. Wire contract is purely additive — `error` gains new `code` value `agent_unavailable`. Old code values stay.
- **Frontend**: unchanged. Wire contract identical.
- **Database**: none. Conversation history tables still deferred per `FUTURE_AGENT.md` §P2.1.
- **Observability**:
  - Python exports OTel spans (`agent.run`, `agent.step.{plan,tool,react,llm}`, `agent.budget`) via OTLP/HTTP → same Jaeger (compose `jaeger` service, `:16686`).
  - Python exports Prometheus counters on `:9001/metrics` via `prometheus-client`. Prometheus scrapes both Go `/metrics` and Python `:9001/metrics`.
- **Security**:
  - Per-owner scoping unchanged — Python forwards the user's Bearer JWT to MCP and to the RAG bridge; both Go endpoints re-validate the JWT and re-read the user id from it (`reqctx.UserIDFromContext`). Python cannot impersonate.
  - Safe-error mapping unchanged — MCP enforces; RAG bridge delegates to `rag.Service` which uses `api.ClassifyAndLog`.
  - New attack surface: the Python service is on the compose bridge network only; not reachable from outside compose. The two new Go endpoints (`/internal/rag/retrieve`, agent proxy) are mounted behind the chi router and gated on Bearer JWT.
- **Roadmap**: `FUTURE_AGENT.md` is reframed — slice 1 now reads "Go infra (provider seam + tool registry + MCP + chat SSE wire) + Python orchestrator (`agent-langgraph`)". The P0.4 / P1.1 / P1.3 / P2 follow-up changes stay as-is.