# Tasks

> **Scope of this change**: replace the Go agent orchestrator with a Python LangGraph implementation; add the Go chat SSE proxy; add the Go RAG bridge endpoint. Implementation is complete. **Production-rollout gates (manual smoke, 1-week CI green, env flips, rollback drill, archive) are deferred to `openspec/changes/agent-production-rollout/`** — that change owns the cross-cutting rollout for all three agent changes.

## 1. Remove Go agent package

- [x] 1.1 Delete `backend/internal/agent/` (entire directory: `service.go`, `planner.go`, `executor.go`, `executor_react.go`, `executor_tool.go`, `budget.go`, `types.go`, `observability.go`, `metrics.go`, plus all tests) and verify `go build ./...` succeeds with no remaining references
- [x] 1.2 Remove `AGENT_*` env entries from `backend/.env.example` (move to Python service `.env.example`) and verify `internal/platform/config/config.go` still loads cleanly with the removed keys absent
- [x] 1.3 Remove `internal/agent` import from `internal/chat/handler.go` and `internal/chat/service.go` (replace with the new agent proxy call site in task 2.x) and verify `go test ./internal/chat/...` still passes for the `AGENT_ENABLED=false` path

## 2. Add Go chat SSE proxy

- [x] 2.1 Create `backend/internal/chat/agent_proxy.go` with `AgentProxy` type that posts to `PYTHON_AGENT_URL/agent/run` (env-driven, default `http://agent:9000/agent/run`), forwards `Authorization: Bearer <token>`, and streams the response SSE body byte-for-byte. Verify with a unit test using `httptest.Server` returning canned SSE.
- [x] 2.2 Wire `internal/chat/handler.go` to call `AgentProxy` when `AGENT_ENABLED=true` and the legacy single-turn path when `AGENT_ENABLED=false`. Add a regression test asserting the byte-for-byte forwarding of Python's response. Add an `error{code:"agent_unavailable"}` frame on connect failure / 5xx from Python.
- [x] 2.3 Add `AGENT_ENABLED` (default false) and `PYTHON_AGENT_URL` (default empty) to viper config and verify with `internal/platform/config/config_test.go`
- [x] 2.4 Full backend test suite (`SKIP_CONTAINERS=true go test ./...`) passes with zero regressions

## 3. Add Go RAG bridge endpoint

- [x] 3.1 Create `backend/internal/rag/http.go` with a chi handler at `GET /internal/rag/retrieve?query=…&user_id=…`. Bearer JWT required via the existing middleware; user id read from the verified JWT (NOT the query param). Returns `{context_block, citations}` on success, `401` on missing token, `400` on missing params. Errors translated through `api.ClassifyAndLog`.
- [x] 3.2 Mount the route in `cmd/api/main.go` under `/internal/rag/retrieve`. Exclude from huma's OpenAPI generation (internal path prefix is hidden from Swagger).
- [x] 3.3 Tests: `internal/rag/http_test.go` covers authenticated happy path, missing token → 401, missing query → 400, JWT user id wins over query user id, internal error → safe detail. Verify `go test ./internal/rag/...` passes.

## 4. Scaffold Python service

- [x] 4.1 Create `backend/agent_py/` with `pyproject.toml` (deps: `fastapi`, `uvicorn[standard]`, `langgraph`, `langchain-openai`, `pydantic`, `pydantic-settings`, `redis`, `opentelemetry-api`, `opentelemetry-sdk`, `opentelemetry-exporter-otlp-proto-http`, `prometheus-client`; test deps: `pytest`, `pytest-asyncio`, `httpx`, `vcrpy`, `fakeredis`). Verify `pip install -e .` succeeds in a fresh venv.
- [x] 4.2 Create `backend/agent_py/Dockerfile` (multi-stage: `python:3.12-slim` base, copy source, install deps into venv, expose 9000 + 9001, health check on `GET /healthz`). Verify `docker build backend/agent_py/` succeeds.
- [x] 4.3 Create `backend/agent_py/agent/__init__.py`, `agent/main.py` (FastAPI app factory + uvicorn entry), `agent/config.py` (pydantic-settings reading the env keys from the proposal), `agent/types.py` (Pydantic models: `Plan`, `PlanStep`, `StepKind`, `Event` discriminated union, `ChatRequest`, `ChatMessage`, `BudgetDecision`). Verify `python -m agent.main` boots and `GET /healthz` returns `{"status":"ok"}`.

## 5. Implement Python budget guard

- [x] 5.1 Create `backend/agent_py/agent/budget.py` with `BudgetGuard` class wrapping a `redis.asyncio.Redis` instance. Methods: `check_and_increment(user_id, estimated_tokens) -> BudgetDecision`. Redis key `agent:budget:u={user_id}:h={utc_hour}` (UTC `YYYYMMDDHH` integer). TTL 90 min on first increment. Fails closed (`decision="unavailable"`) on Redis error.
- [x] 5.2 Wrap every check in an OTel span `agent.budget` with attributes `user_id`, `tokens_used`, `tokens_remaining`, `decision` (`allow` | `exceeded` | `unavailable`). Verify with `opentelemetry-sdk`'s in-memory span exporter in tests.
- [x] 5.3 Tests (`tests/test_budget.py`): happy-path increment; cap exceeded; Redis down (`fakeredis` or mocked client errors) → `unavailable`. Span attributes asserted via the in-memory exporter.

## 6. Implement Python RAG + tool clients

- [x] 6.1 Create `backend/agent_py/agent/rag_client.py` with an async `httpx.AsyncClient` posting to `RAG_BRIDGE_URL` (default `http://backend:8080/internal/rag/retrieve`) with `Authorization: Bearer <token>` and JSON body `{query, user_id}`. Returns `{context_block, citations}` on 200, raises on 401/400/5xx.
- [x] 6.2 Create `backend/agent_py/agent/tools_client.py` with an async client posting JSON-RPC 2.0 to `MCP_URL` (default `http://backend:8080/mcp`) with `Authorization: Bearer <token>`. Method `tools/call` with `{name, arguments}`. Returns the tool's result payload on success, raises `ToolError` on JSON-RPC error.
- [x] 6.3 Tests (`tests/test_rag_client.py`, `tests/test_tools_client.py`) use `httpx.MockTransport` for canned responses. Cover happy path + 401 propagation + 5xx retry-once-or-raise.

## 7. Implement Python LangGraph state graph

- [x] 7.1 Create `backend/agent_py/agent/planner.py` with `planner_node(state: AgentState) -> AgentState`. One `langchain_openai.ChatOpenAI` (or vLLM-compatible) call requesting JSON output matching the Pydantic `Plan` schema. Validates via `Plan.model_validate_json(...)`. Rejects on: cap exceeded → emit `error{code:"plan_too_large"}` + `done` + `terminator`; duplicate step ids; unknown step kind.
- [x] 7.2 Create `backend/agent_py/agent/executor.py` with `executor_node(state: AgentState) -> AgentState`. Sequential over `state.plan.steps`. For each step, runs the appropriate handler:
  - `rag` → `RagClient.retrieve` → render context block into `state.step_outputs[step.id]`
  - `tool` → `ToolsClient.call(step.tool, step.args)` → render result
  - `llm` → direct LLM call with state.messages + step.prompt → accumulate text deltas
  - `react` → nested subgraph (model → tool → model) capped by `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS`; on cap → emit `step_end{status:"failed",reason:"react_max_iterations"}` + `error{code:"react_iterations_exceeded"}`
- [x] 7.3 Create `backend/agent_py/agent/graph.py` building the StateGraph: `START → planner → executor → END`. Each node appends events to `state.events_so_far` so the SSE emitter can yield them as they happen.
- [x] 7.4 Wrap the graph execution in a parent `agent.run` OTel span with attributes `run_id`, `user_id`, `plan_size`, `total_tokens` accumulated.

## 8. Implement Python SSE emitter

- [x] 8.1 In `agent/main.py`, add `POST /agent/run` route. Accepts `{messages, agent?, model?, user_id}`. Bearer JWT verified (HTTP 401 on missing/invalid). Streams the LangGraph execution: yields `plan` first, then per-step frames as `state.events_so_far` grows, then `done` + `terminator` on completion. Wire format: `event: <kind>\ndata: <json>\n\n` — byte-for-byte identical to the existing Go chat handler envelope.
- [x] 8.2 Add the `/healthz` route returning `{"status":"ok"}` when the budget Redis client and the LLM provider are reachable; `503` otherwise.

## 9. Wire compose

- [x] 9.1 Add `agent` service to `docker-compose.yml` building from `backend/agent_py/Dockerfile`. Port 9000 (FastAPI) and 9001 (Prometheus exporter) exposed on the bridge network only. Health check hits `GET /healthz`. `depends_on`: `redis` (condition: `service_healthy`), `backend` (condition: `service_healthy`).
- [x] 9.2 Add `agent` service env vars from the proposal (`PYTHON_AGENT_*`, `MCP_URL=http://backend:8080/mcp`, `RAG_BRIDGE_URL=http://backend:8080/internal/rag/retrieve`, `REDIS_URL=redis://redis:6379`, `OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318`, `AGENT_METRICS_PORT=9001`).
- [x] 9.3 Add Python agent's Prometheus target (`agent:9001`) to the Prometheus scrape config (compose has a Prometheus service if you wire one; otherwise add `prometheus.yml` updates per `FUTURE_BACKEND.md` §5).
- [x] 9.4 Verify `make up` brings `db` + `redis` + `jaeger` + `backend` + `agent` + `frontend` up. `make dev` boots backend + frontend; `agent` runs in compose.

## 10. Python tests

- [x] 10.1 Unit tests for `agent/planner.py`: valid plan parsed; oversized plan rejected; duplicate ids rejected; unknown kind rejected; provider error → typed error event.
- [x] 10.2 Unit tests for `agent/executor.py`: each step kind handled correctly; react iteration cap; step frame ordering matches spec; error events emit on failures.
- [x] 10.3 End-to-end `tests/test_run.py` against the FastAPI app: planner stub returns a 2-step plan → executor runs rag + tool + llm → SSE stream contains `plan`, `step_start`, `tool_call`, `tool_result`, `step_end`, `delta`, `done`, `terminator` in order. Use `vcrpy` cassettes for canned LLM responses; `httpx.MockTransport` for RAG/MCP clients.
- [x] 10.4 End-to-end `tests/test_budget_e2e.py`: budget exceeded mid-run → `error{code:"budget_exceeded"}` + done + terminator; Redis down → `error{code:"budget_unavailable"}`.

## 11. End-to-end Go ↔ Python integration

- [x] 11.1 New Go test `internal/chat/agent_proxy_integration_test.go`: brings up an `httptest.Server` returning canned Python SSE → asserts the chat handler's `POST /api/chat` response body byte-for-equals the canned SSE.
- [ ] 11.2 (moved) Manual smoke test (requires docker compose): `make up`, trigger a chat that produces a `list_feeds` plan; verify the SSE stream contains `plan` → `step_start` → `tool_call` → `tool_result` → `step_end` → `delta` → `done` → `terminator` against the local stack → see `agent-production-rollout/1.2`.

## 12. Reframe `FUTURE_AGENT.md` and `agent-loop`

- [x] 12.1 Reframe `FUTURE_AGENT.md` §0.3 + §1.2 — items that landed in Go agent-loop but move to Python get `[x]` with a note "shipped in Go, superseded by `agent-langgraph`"; items that survive (provider seam, tool registry, MCP, RAG, chat SSE wire) keep their `[x]` ticks.
- [x] 12.2 Update `openspec/changes/agent-loop/proposal.md` and `tasks.md` — add a note at the top: "The Go agent package was removed in `agent-langgraph`; this change retains the provider seam, tool registry, MCP shim, and chat SSE translator that the Python service consumes. The Go implementation tasks are done; the live follow-up is `agent-langgraph`."
- [x] 12.3 Update `openspec/changes/agent-loop/specs/` — the four specs (`llm-structured-output`, `agent-tool-registry`, `agent-loop`, `agent-observability`) stay valid; `agent-loop` and `agent-observability` get a note in `## Purpose` saying "implementation moved to Python in `agent-langgraph`; this spec stays as the wire contract".

## 13. Dev-environment flip

- [x] 13.1 Add `AGENT_ENABLED=true` (commented) and `PYTHON_AGENT_URL=http://agent:9000` (commented) to `backend/.env.example` and verify `make dev` boots with the agent path
- [x] 13.2 Add `backend/agent_py/.env.example` with the full Python env surface from the proposal

## 14. Deferred to `agent-production-rollout`

> The five items below are cross-cutting production-rollout gates shared with `agent-loop` and `agent-run-state`. They moved out of this change to a single rollout change so the three implementation changes archive cleanly while the rollout work tracks as one workstream. See `openspec/changes/agent-production-rollout/tasks.md`.

- [ ] 11.2 (moved) Manual smoke test against the local stack with a `list_feeds` plan → see `agent-production-rollout/1.2`.
- [ ] 14.1 (moved) 1-week CI green before flipping `AGENT_ENABLED=true` in production → see `agent-production-rollout/2.1`.
- [ ] 14.2 (moved) Flip `AGENT_ENABLED=true` + `PYTHON_AGENT_URL` in production + deploy notes → see `agent-production-rollout/2.3`.
- [ ] 14.3 (moved) Rollback drill (`AGENT_ENABLED=false` in staging) → see `agent-production-rollout/2.4`.
- [ ] 14.4 (moved) Archive once 14.1–14.3 clear (the "archive-after" decision is superseded: the three implementation changes archive now; only this rollout change remains open) → see `agent-production-rollout/2.5`.