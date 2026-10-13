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
  `agent_tool_calls_total`, `agent_tokens_total`, `agent_planner_parse_failures_total`,
  `agent_tool_retries_total`).

## Planner: structured output with bounded retry

The planner obtains `Plan` via provider structured output rather than
parsing free-text model output: it binds the OpenAI `response_format`
json_schema explicitly (`strict: false`) and parses the content JSON
through Pydantic.

```python
base = ChatOpenAI(...)
base.bind(response_format={
    "type": "json_schema",
    "json_schema": {
        "name": "Plan",
        "schema": Plan.model_json_schema(),
        "strict": False,
    },
}) | JsonOutputParser() | RunnableLambda(Plan.model_validate)
```

Why not `with_structured_output(Plan, method="json_schema", strict=False)`?
Verified against langchain-openai 1.7.0: that helper silently forces
`strict: true` for Pydantic-class schemas (the `strict` flag is dropped at
bind time), and strict mode cannot represent `PlanStep.args`' free-form
object — arguments would resolve to `{}` or the request be rejected. The
explicit bind keeps `strict: false`, preserving arbitrary tool args.
Pydantic validation + the retry loop cover the residual conformance gap.
The wire-format `plan` frame is byte-for-byte identical to the legacy
free-text path's output.

A bounded retry loop catches residual `pydantic.ValidationError` /
`langchain_core.exceptions.OutputParserException` cases (truncation,
schema drift across provider versions, planning prompts that nudge the
model toward a different schema):

| Env var                                   | Default | Purpose                                                                       |
|-------------------------------------------|---------|-------------------------------------------------------------------------------|
| `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED` | `true`  | Use the structured binding (primary path). `false` falls back to free-text.   |
| `PYTHON_AGENT_PLANNER_RETRY_MAX`          | `1`     | Re-prompts after a validation failure, capped at this value. 0 = no retry.     |

The fallback path (`PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false`) keeps
the original fence-strip + `json.loads` parser unchanged — no retry budget
— so operators can compare run-rates between the two paths during rollout.
It is an escape hatch for providers whose parser doesn't honour
`response_format`, not a retry path.

The counter `agent_planner_parse_failures_total{retry}` exposes recovery
behaviour:

- `retry="ok"` — recovery succeeded on a later attempt within the budget.
- `retry="exhausted"` — retry budget exhausted; run terminated with
  `error{code:"plan_invalid"}`.

See `openspec/changes/agent-planner-structured/` for the full rationale
(why `json.loads` on free text fails at every model tier; how retries
inject the validator's last error; cross-provider parity with vLLM
`hermes` parser).

## Tool dispatch: bounded retry on registry / transient failures

The executor wraps every `tools.call(tc_name, args)` site in two retry
loops that run in sequence — only one fires per failure class. Together
they cover the two failure modes the planner-side retry deliberately
leaves out: model-emitted tool calls that the registry rejects (Pydantic
validation, missing required field, wrong type) and transient errors
from the tool's remote call (network timeout, MCP / RAG bridge 5xx,
408 / 429).

| Env var                                     | Default | Purpose                                                                       |
|---------------------------------------------|---------|-------------------------------------------------------------------------------|
| `PYTHON_AGENT_TOOL_RETRY_MAX`               | `2`     | Retries on a tool-dispatch failure, capped at this value. 0 = no retry.       |
| `PYTHON_AGENT_TOOL_RETRY_BACKOFF_BASE_MS`   | `250`   | Base backoff between transient retries. Effective sleep is `min(base * 2^n ± jitter, ceiling)`. |
| `PYTHON_AGENT_TOOL_RETRY_BACKOFF_CEILING_MS`| `5000`  | Hard ceiling on the per-attempt backoff sleep.                                |

- **Validator-feedback path** (registry rejected the call's arguments):
  re-invoke the model up to `PYTHON_AGENT_TOOL_RETRY_MAX` times total,
  appending the validator's error message to the re-prompt's final user
  message under a `PREVIOUS_ATTEMPT_FAILED` block. No backoff sleep —
  re-prompts are immediate. Budget is per-step and per-tool — exhaustion
  in step A does not consume budget in step B.
- **Transient-execution path** (network timeout, 5xx, 408, 429):
  retry with exponential backoff (`min(base * 2^attempt + jitter, ceiling)`,
  jitter ±20%). Non-retryable HTTP 4xx (other than 408 / 429) fails the
  step immediately with no retry. The two retry paths MUST NOT both
  fire on the same error — a validation error routes to the
  validator-feedback path, a transport error routes here.

The counter `agent_tool_retries_total{tool, reason}` exposes per-tool
retry behaviour:

- `reason="tool_arg_invalid"` — registry rejected the call's arguments.
- `reason="tool_execution_transient"` — transient error from the tool's
  remote call (network, 5xx, 408, 429).

Every retry attempt increments the counter by exactly 1; a successful
first attempt does not. See `openspec/changes/agent-tool-dispatch-retry/`
for the full rationale (per-step vs per-run budget, why a 422 Pydantic
error from the bridge must route to the validator-feedback path not the
transient path, OTel `retry_count` + `retry_reason` attributes on the
surrounding `agent.step.tool` span).

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