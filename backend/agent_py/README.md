# nyx-agent

Python Plan-and-Execute agent orchestrator for Nyx. Replaces the Go `internal/agent/` package
that shipped in the first OpenSpec change (`openspec/changes/agent-loop/`). RAG stays in Go
(unchanged); tools dispatch through the existing MCP JSON-RPC endpoint; the chat SSE wire
contract is byte-for-byte identical to what the Go chat handler emits today.

## Endpoints

- `POST /agent/run` — accepts `{messages, agent?, model?}`, streams SSE in the existing envelope
  (`event: <kind>\ndata: <json>\n\n` for the 10 kinds: `plan`, `step_start`, `tool_call`,
  `tool_result`, `step_end`, `delta`, `done`, `note`, `error`, `terminator`).
- `GET /healthz` — 200 with `{"status":"ok"}` when Redis is reachable; 503 otherwise.
- `GET /metrics` — Prometheus counters (`agent_runs_total`, `agent_steps_total`,
  `agent_tool_calls_total`, `agent_tokens_total`).

## Architecture

```
planner node  ── one LLM call → Pydantic Plan (validated)
executor node ── sequential over Plan.steps
  ├─ rag   → POST /internal/rag/retrieve  (Go RAG bridge)
  ├─ tool  → POST /mcp tools/call        (Go MCP shim)
  ├─ llm   → direct langchain-openai call
  └─ react → nested subgraph (model → tool → model), capped by
             PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS
budget guard ── Redis counter `agent:budget:u={user_id}:h={utc_hour}`,
                fail-closed on Redis error
```

## Run locally

```
cp .env.example .env
# fill in PYTHON_AGENT_LLM_BASE_URL + PYTHON_AGENT_LLM_API_KEY
pip install -e '.[dev]'
python -m agent.main
# or
uvicorn agent.main:app --port 9000 --reload
```

## Tests

```
pytest tests/ -v
```

Tests use `fakeredis` for Redis and `httpx.MockTransport` for the HTTP clients. No live
network needed.

## Compose

The `agent` service in `docker-compose.yml` builds from this directory's Dockerfile,
exposes 9000 + 9001 on the bridge network, and depends on `redis` + `backend` (the
Go service that hosts the RAG bridge + MCP shim).

## Trust model

The Python service does **not** verify JWTs. It runs on the private compose / k8s
network — port 9000 is not exposed to the host (`docker-compose.yml` only publishes
`127.0.0.1:9001:9001` for metrics scraping). The trust boundary at this hop is the
network, not application auth:

- **Go's chat handler** verifies the user JWT at the SPA edge, then forwards the
  verified user id in the `X-Nyx-User-Id` header. Python reads this header directly
  and uses it for the per-user budget counter and per-owner scoping on tool calls.
- The `Authorization` bearer token is forwarded as a passthrough to Go's `/mcp` and
  `/internal/rag/retrieve` endpoints, which re-verify it themselves (Python never
  decodes the token).

If you ever expose port 9000 to a network where the Python service is not the only
reachable caller, you MUST add an auth layer (mTLS, a shared service secret, or a
network-level firewall). Today this is enforced by `docker-compose.yml` and the
intended k8s `NetworkPolicy`. See `FUTURE_AGENT.md` for the operator-facing
rationale and `docker-compose.yml` for the bridge-network contract.