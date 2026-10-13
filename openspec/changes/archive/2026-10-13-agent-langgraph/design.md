# Design

## Context

The first OpenSpec change `agent-loop` shipped a Plan-and-Execute agent loop in Go (`internal/agent/`). After delivery, the project owner decided the orchestrator should live in Python (LangGraph + Pydantic) — LangGraph's mature state-graph primitives, checkpointing, and ecosystem are a better fit than a hand-rolled Go loop. The change reverses the agent-orchestration layer while preserving everything that gave the Go loop its contracts: provider seam (OpenAI-compatible wire), tool registry (per-owner scoped), MCP shim, and the chat SSE wire envelope (10 event kinds). Python will not reimplement RAG — it calls a tiny new Go endpoint that wraps `rag.Service.Retrieve`. Python will not reimplement tools — it calls the existing MCP `tools/call` JSON-RPC endpoint. The Go chat handler becomes a thin SSE proxy.

The full P0–P2 roadmap lives in `FUTURE_AGENT.md`. This design covers the swap from Go orchestrator to Python orchestrator plus the two Go bridge endpoints needed for Python to talk to RAG and tools.

## Goals / Non-Goals

**Goals:**
- Move agent orchestration from Go to Python LangGraph + Pydantic.
- Preserve provider seam, tool registry, MCP, RAG service, and chat SSE wire envelope unchanged.
- Expose RAG to Python via a small authenticated Go endpoint so Python never reimplements RAG.
- Make Python dispatch tools through the existing MCP `tools/call` JSON-RPC endpoint (no new code on the tool side).
- Make the Go chat handler a thin SSE proxy when `AGENT_ENABLED=true`; keep the current path when `AGENT_ENABLED=false`. Wire contract unchanged either way.
- Keep per-user budget guard semantics identical: same Redis key, same TTL, fail-closed, same `agent.budget` span attributes.
- Keep observability continuous: OTel spans flow into the same Jaeger; Prometheus counters live in both runtimes for compose scrape.

**Non-Goals:**
- Conversation history tables (P2.1) — unchanged, deferred per `FUTURE_AGENT.md`.
- DB-backed run state (P1.1) — unchanged, deferred per `FUTURE_AGENT.md`.
- Task routing (P0.4) — unchanged, deferred per `FUTURE_AGENT.md`.
- Fault tolerance (P1.3) — unchanged, deferred per `FUTURE_AGENT.md`. Python owns the run; failure modes are observed and retried only at the LangGraph level until slice 1 has data.
- Self-reflection (P2.2) — out of scope.
- Replacing MCP — MCP's `tools/call` is the tool dispatch surface for Python as well as external agents.
- Splitting the monorepo — Python lives under `backend/agent_py/` and is built into its own Docker image; the Go backend remains a separate image. CI runs both.

## Decisions

### 1. Python service streams the same SSE envelope as the Go chat handler emits today

The Python service is the source of truth for the event payload shapes; the Go chat handler does NOT translate them. It opens an SSE response to the client and pipes Python's response body straight through. Wire contract identity is enforced by a regression test on the Python side (vcrpy cassette of canned LLM responses producing the exact byte sequence the existing chat handler emits today) and on the Go side (`httptest.Server` returning canned Python SSE → assert byte-for-byte forwarding).

**Alternatives considered:**
- *Translate Python events → Go SSE types → JSON on the wire*: rejected. Double translation adds a failure mode and a maintenance burden for what should be an identical contract.
- *Use JSON-RPC over WebSocket instead of SSE*: rejected. The frontend is already wired to SSE; the refresh-on-401 single-flight flow assumes SSE.

### 2. Tools flow through the existing MCP endpoint

Python calls MCP's `tools/call` JSON-RPC endpoint via HTTP. No new code on the Go side for tool dispatch. Per-owner scoping and safe-error mapping already enforced inside `internal/mcp/service.go` carry over.

**Alternatives considered:**
- *Add a separate `/internal/tools/dispatch` endpoint parallel to MCP*: rejected. Two tool-dispatch surfaces invite drift; MCP is the project's contract for tool exposure.
- *Embed the Go tool registry into the Python process via CGo / shared lib*: rejected. CGo adds a build-system surface that's larger than the HTTP boundary; `MCP_URL` already exists for external agents.

### 3. RAG flows through a new tiny Go endpoint

`GET /internal/rag/retrieve?query=…&user_id=…` wraps `rag.Service.Retrieve`. Bearer JWT required; user id is taken from the JWT, never the query param. Returns `{context_block, citations}`. Mounted on the chi router, hidden from Swagger.

**Alternatives considered:**
- *Reimplement RAG in Python*: rejected (per user). pgvector + per-user HNSW + lazy backfill are non-trivial and already shipped in Go.
- *Expose the existing RAG service over the MCP server protocol*: doesn't fit — MCP exposes tools, not domain queries.
- *Open the existing RAG service over the same MCP `tools/call` as a `retrieve_feeds_context` tool*: rejected. Tools take JSON args and return tool-shaped responses; the RAG bridge needs to return a context block, not a tool envelope.

### 4. Go chat handler fails closed when Python is unavailable

When `AGENT_ENABLED=true` and the proxy cannot reach `PYTHON_AGENT_URL`, the handler emits `error{code:"agent_unavailable"}` and terminates. It does NOT fall back to single-turn.

**Alternatives considered:**
- *Fall back to single-turn when Python is down*: rejected. Hides failures from operators; the user explicitly asked for the agent loop; if it's broken, the response should say so.
- *Block the chat request with HTTP 503*: rejected. The SSE envelope was opened; emitting a typed error event keeps the wire contract uniform.

### 5. Budget guard moves to Python

The same Redis counter, same TTL, same fail-closed semantics. Python reads/writes the same `agent:budget:u={user_id}:h={utc_hour}` key. The Go side no longer enforces budget (it can't — it doesn't own the loop).

**Alternatives considered:**
- *Keep a Go-side budget guard that the Python service consults over HTTP*: rejected. Two enforcement points invite drift; one source of truth is correct.
- *Move the budget counter to a different backend (Postgres, in-memory)*: rejected. Redis is already in the stack; `AGENT_BUDGET_PER_USER_PER_HOUR` envs stay compatible.

### 6. Observability — Python exports OTel + Prometheus directly

Python uses `opentelemetry-sdk` with OTLP/HTTP exporter → same `jaeger:4318` endpoint the Go backend uses. Service name `nyx-agent`. Python uses `prometheus-client` with its own `:9001/metrics` HTTP exporter; Prometheus scrape config adds the Python target alongside the Go target. Span and counter names match what Go emits today (`agent.run`, `agent.step.{plan,tool,react,llm}`, `agent.budget`, `agent_runs_total{kind,status}`, `agent_steps_total{kind,status}`, `agent_tool_calls_total{tool,ok}`, `agent_tokens_total{role,model}`).

**Alternatives considered:**
- *Have the Go side proxy Python spans and counters*: rejected. Adds Go complexity for what Python SDKs handle natively.
- *Two Prometheus jobs with different label prefixes*: rejected. Operator dashboards prefer identical label shapes.

### 7. LangGraph state graph is sequential with a nested react subgraph

The graph has two top-level nodes:
- `planner` — one LLM call returning the JSON `Plan`. Validates via Pydantic `Plan` model; rejects on cap violation, duplicate IDs, unknown kinds.
- `executor` — sequential over plan steps. For each step:
  - `rag` → HTTP call to `/internal/rag/retrieve` → wraps result.
  - `tool` → HTTP call to MCP `tools/call` → wraps result.
  - `llm` → direct LLM call with the conversation context + step's `prompt`.
  - `react` → subgraph with model-tool-model loop capped by `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS`.

State flows through `AgentState` (TypedDict): `messages`, `plan`, `step_outputs` (map of step id → output), `events_so_far`, `user_id`, `run_id`, `cumulative_tokens`. Each node appends to `events_so_far` and the streaming consumer yields them as SSE frames.

**Alternatives considered:**
- *Pure ReAct loop at the top level*: rejected per the agent-loop design rationale — multi-step planning is the dominant use case, pure ReAct burns tokens and is harder to debug.
- *Use LangGraph's `create_react_agent` from langgraph.prebuilt*: rejected. The prebuilt agent is opinionated about state shape and tool-calling; we need the planner JSON-schema path.

### 8. Pydantic models are the wire format

`Plan`, `PlanStep`, `StepKind`, `Event` discriminated union, `ChatRequest`, `ChatMessage`, `BudgetDecision`, `AgentState` are the single source of truth for types in Python. FastAPI serialises them to JSON; the SSE emitter uses them to build the `event: <kind>\ndata: <json>\n\n` envelope. Field names match what the Go chat handler emits today.

**Alternatives considered:**
- *Dataclasses*: rejected. Pydantic gives us validation + serialisation + IDE type hints in one.
- *TypedDict only*: rejected. Validation and serialisation are first-class requirements; TypedDict is documentation.

### 9. Configuration via pydantic-settings

`backend/agent_py/agent/config.py` reads env via `pydantic-settings.BaseSettings`. Defaults match the Go `AGENT_*` defaults that move here (`PYTHON_AGENT_PLAN_MAX_STEPS=10`, `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS=5`, `AGENT_BUDGET_PER_USER_PER_HOUR=200000`). Required keys raise on startup.

**Alternatives considered:**
- *YAML/TOML config file*: rejected. Compose env-vars are already the pattern.
- *Hardcoded constants*: rejected. Defaults exist; operators override.

### 10. Docker image lives in `backend/agent_py/Dockerfile`

Multi-stage Python build (slim base, install dependencies into a venv, copy source). Image is built from the repo root by the existing compose `docker-compose.yml` agent service. Health check hits `GET /healthz` (port 9000). Compose adds `agent` to the bridge network; `backend` (Go) and `agent` (Python) talk over the bridge; `agent` connects to `redis` and `db` (db NOT used by Python today — placeholder for future memory).

**Alternatives considered:**
- *A separate repo for Python*: rejected (per user — monorepo).
- *Embed Python in the Go binary via Yaegi or wazero*: rejected. Heavy, fragile, no Pydantic support.

## Risks / Trade-offs

- **HTTP bridge adds latency per request** → ~5–20ms vs in-process Go. For the AgenticStack user case (4–6 tool calls + planner + executor), adds ~30–80ms total orchestration overhead. Tight for sub-second tools; fine for human-visible chat. Mitigation: cap `AGENT_PLAN_MAX_STEPS` low (10) and keep MCP `tools/call` calls short.
- **Two runtimes in compose** → compose has `db`, `redis`, `jaeger`, `backend`, `frontend` already. Adding `agent` is one more service. Mitigation: same compose file, same `make up`/`make dev`, health check + restart policy. CI builds both images.
- **Python service down → chat fails** → mitigated by `agent_unavailable` SSE error + compose health check + restart. Mitigation: explicit `error{code:"agent_unavailable"}` event so dashboards alert on it.
- **Tool safety unchanged** → MCP enforces per-owner scoping + safe-error mapping; Python never bypasses it. Risk: zero new surface. Mitigation: integration test that asserts Python's tool dispatch goes through MCP's JSON-RPC, not direct registry access (which Python can't reach anyway).
- **Wire contract drift between Python and Go** → mitigated by the byte-for-byte regression test on both sides. The Python SSE envelope is frozen in vcrpy cassettes; the Go proxy test asserts the forwarded bytes equal the upstream bytes.
- **Planner JSON-schema divergence from Go's previous planner** → Go's planner JSON schema is documented in the previous `agent-loop` design §6. Python's Pydantic `PlanStep` model must accept the same JSON shape (including `id`, `kind`, `tool`, `args`, `prompt`, `query`, `max_iterations`). Frontend has no opinion on this — frontend only cares about `Event` payload shapes.
- **OTel span identity** → Python spans must be attributed to the same `agent.run` span hierarchy the Go loop used. Use LangGraph's `@traceable` (langsmith) or manual span creation via opentelemetry directly.
- **Budget guard Redis key contract** → identical key, identical TTL. If the Go backend and Python service are co-deployed during cutover, the counter is shared (counter is per-user, not per-runtime). This is fine.
- **Two Prometheus jobs** → counters with the same name in two runtimes will collide if Prometheus aggregates them by name. Mitigation: differentiate by `job` label (`backend` vs `agent`) or by prefixing Python's counters with the same names and using `instance` label. Dashboards will use `rate(agent_runs_total{job="agent"}[5m])` and `rate(agent_runs_total{job="backend"}[5m])` distinctly.

## Migration Plan

1. **Remove Go agent package** — delete `backend/internal/agent/` entirely (~10 files). Delete tests. Verify `internal/chat/handler.go` no longer imports it.
2. **Add Go agent proxy** — `backend/internal/chat/agent_proxy.go` (~150 LOC). Wired in `internal/chat/handler.go` behind `AGENT_ENABLED=true`.
3. **Add Go RAG bridge** — `backend/internal/rag/http.go` (~80 LOC). Mounted in `cmd/api/main.go` chi router under `/internal/rag/retrieve`. Excluded from Swagger.
4. **Scaffold Python service** — `backend/agent_py/` skeleton: `pyproject.toml`, `Dockerfile`, `agent/main.py`, `agent/config.py`, `agent/types.py`, tests scaffold.
5. **Implement Python graph** — `agent/graph.py`, `agent/planner.py`, `agent/executor.py`, `agent/rag_client.py`, `agent/tools_client.py`. Pydantic models first; graph against them.
6. **Implement Python budget guard** — `agent/budget.py`. Redis-backed counter + OTel span. Fails closed.
7. **Implement SSE emitter** — `agent/sse.py` (or inline in `main.py`); FastAPI route streams events as `event: <kind>\ndata: <json>\n\n`.
8. **Wire compose** — add `agent` service to `docker-compose.yml` with health check + depends_on `redis`.
9. **Refactor FUTURE_AGENT.md** — reframe slice 1: "Go infra + Python orchestrator (`agent-langgraph`)". Tick shipped items.
10. **Cutover**:
    - Local dev: `AGENT_ENABLED=true` + `PYTHON_AGENT_URL=http://agent:9000` in `backend/.env.example`. `make up` brings agent up.
    - Production flip: `AGENT_ENABLED=true` + `PYTHON_AGENT_URL` in deploy env; observability dashboards alert on `agent_unavailable` rate.
11. **Rollback**: `AGENT_ENABLED=false` restores single-turn chat; provider seam / tool registry / MCP / RAG / chat SSE wire are untouched and keep working.

## Open Questions

- *Should the Python service own its own prompt history between turns within a single run, or does it always receive the full conversation in the request body?* — current answer: always receive full conversation (matches today's single-turn contract; the orchestrator is a single-shot per request). Cross-session memory is a future change.
- *LLM provider failover in Python* — Python reads `PYTHON_AGENT_LLM_FALLBACK_*` if set; otherwise uses `ChatOpenAI` with the primary only. vLLM parity for tool calls (the same risk the previous design called out) lands as a separate test pass.
- *Should the agent's `terminator` SSE frame be emitted before or after the upstream Python service closes its TCP connection?* — current answer: as soon as Python emits `terminator`, Go forwards it and closes. The proxy does NOT add a delay.