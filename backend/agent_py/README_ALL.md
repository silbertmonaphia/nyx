# nyx-agent

> Short version: [`README.md`](README.md) · 中文 (brief): [`README.zh.md`](README.zh.md) — this file is the full reference.

Python Plan-and-Execute agent orchestrator for Nyx. Replaces the Go `internal/agent/` package
that shipped in the first OpenSpec change (`openspec/changes/archive/2026-10-13-agent-loop/`). RAG stays in Go
(unchanged); tools dispatch through the existing MCP JSON-RPC endpoint; the chat SSE wire
contract is byte-for-byte identical to what the Go chat handler emits today.

## Endpoints

- `POST /agent/run` — accepts `{messages, agent?, model?, run_id?}` (`run_id` echoes a prior run to resume it), streams SSE in the existing envelope
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

See `openspec/changes/archive/2026-10-13-agent-planner-structured/` for the full rationale
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
first attempt does not. See `openspec/specs/agent-tool-retry/spec.md`
for the full rationale (per-step vs per-run budget, why a 422 Pydantic
error from the bridge must route to the validator-feedback path not the
transient path, OTel `retry_count` + `retry_reason` attributes on the
surrounding `agent.step.tool` span).

## Web tools: agent-native search + fetch (opt-in)

Two agent-native tools dispatched through the same retry funnel as the
MCP registry tools (openspec/changes/archive/2026-10-14-agent-web-tools):

- **`web_fetch`** — fetches a public http(s) URL and returns its readable
  text (stdlib HTML→text extraction, byte + char caps, ports 80/443 only,
  content-type allowlist).
- **`web_search`** — Tavily Search API queries returning compact ranked
  results (title / URL / snippet). Runs keyless by default (no account,
  rate-limited); an optional card-free key (1k searches/month) raises the
  limits.

Both default **off**; with them off the merged tool list equals the MCP
registry list and the planner prompt is byte-identical to its
pre-web-tools form.

| Env var | Default | Purpose |
|---|---|---|
| `PYTHON_AGENT_WEB_FETCH_ENABLED` | `false` | Register the `web_fetch` tool. |
| `PYTHON_AGENT_WEB_SEARCH_ENABLED` | `false` | Register the `web_search` tool. |
| `PYTHON_AGENT_TAVILY_API_KEY` | — (empty = keyless) | Optional Tavily key; empty runs the keyless tier. |
| `PYTHON_AGENT_TAVILY_BASE_URL` | Tavily endpoint | Overridden by tests/e2e to point at a stub. |
| `PYTHON_AGENT_WEB_SEARCH_MAX_RESULTS` | `5` | Results per search (hard cap 10). |
| `PYTHON_AGENT_WEB_FETCH_MAX_BYTES` | `2097152` | Response byte cap (streamed — an oversized body never fully enters memory). |
| `PYTHON_AGENT_WEB_FETCH_TIMEOUT_SECONDS` | `10` | Request timeout (also used for the search provider call). |
| `PYTHON_AGENT_WEB_FETCH_MAX_CHARS` | `20000` | Extracted-text cap, with truncation marker. |
| `PYTHON_AGENT_WEB_FETCH_MAX_REDIRECTS` | `5` | Redirect hop cap; every hop re-validated by the SSRF guard. |
| `PYTHON_AGENT_WEB_FETCH_ALLOW_PRIVATE` | `false` | Dev/e2e escape hatch: lifts the private-address deny-list AND the 80/443 port rule (local fixture servers). Logged loudly at startup. Never in a deployed environment. |

**SSRF guard** (mirrors the Go backend's `checkHostNotPrivate` +
per-dial re-check): scheme/port/userinfo validation, then every resolved
address must pass the deny-list (loopback / RFC1918 / link-local incl.
`169.254.169.254` / multicast / unspecified / reserved — IPv4 and IPv6,
including IPv4-mapped and unique-local forms). The connection is **pinned
to the validated resolved address** (Host header + TLS SNI keep the
original hostname), so a name that re-resolves between validation and
connect cannot reach a denied address. The fetch client ignores ambient
proxy configuration (`trust_env=False`) by design — the pinned-dial
guarantee only holds when the agent's own transport makes the connection —
so public fetches require **direct egress** (proxy-only environments fail
closed as transient connect errors). Guard refusals are correctable
(model re-prompt); timeouts/DNS failures are transient (backoff retry);
provider 4xx (e.g. a bad key) fails fast.

**Untrusted content**: fetched text and search snippets are wrapped in
`<untrusted_web_content>` markers, and the tool descriptions state the
content is data to summarize or quote, never instructions. The framing
reduces — it does not eliminate — prompt-injection risk from web content;
the tools are read-only and the per-owner data tools remain scoped.

Retry behaviour rides the existing classes (`agent_tool_retries_total`
labels unchanged): correctable → `tool_arg_invalid`, transient →
`tool_execution_transient`.

Wire-shape regression guard: `frontend/tests/e2e/chat-web-tools.spec.ts`
replays a fixture that
`tests/test_executor.py::test_web_tools_wire_fixture_matches_producer`
generates from the real executor and pins byte-for-byte.

Note: the compose `internal` network is a closed *bridge* but not
`internal: true` — the agent container has egress by default, so enabling
web tools in a deployed stack is an egress decision, not just a flag.

## Prior-data context bounding: small-model defaults

The answer step (`llm` / `react`) is fed the data produced by earlier
`rag` / `tool` steps (most recent first; `llm` / `react` outputs are not
carried forward — they already streamed as deltas). That block is capped
by module constants in `agent/executor.py` so a multi-result
`web_search` cannot push the answer prompt past a small model's context
window — the in-the-wild repro (2026-10-14) was Qwen3-4B + 5 search
results → provider `400` at 4097 input tokens:

- `PRIOR_DATA_MAX_CHARS` (`2800`) — total cap, in characters not tokens
  (CJK tokenizes at ~1 token/char); oldest sections drop first.
- `PRIOR_DATA_PER_SECTION_MAX_CHARS` (`1500`) — per-section cap, applied
  before the total so one huge step cannot starve later ones; overflow
  gets a truncation marker.
- Wire-side `<untrusted_web_content>` markers are stripped from the copy
  that goes into the answer prompt (the `tool_result` frame on the wire
  keeps the framing — it is preserved end-to-end, just not doubled).

Data-free plans (e.g. a greeting) produce no block — their prompt shape
is unchanged.

## Stale-run sweeper: reaping zombie runs

A crashed or killed agent process leaves its `agent.runs` row stuck in
`running` — indistinguishable from a live run. While a run's SSE stream
is in flight, a background task touches its `last_heartbeat_at`; the
sweeper marks any row still `running` with a heartbeat older than the
stale timeout as `status='failed'`, `error_code='stale_run'`. The
timeout doubles as the window in which a crashed run can still be
**resumed** by echoing its `run_id`; after reaping, resume follows the
terminal-row path (fresh run + `note{kind:"fresh_run"}`).

| Env var                                  | Default | Purpose                                                                 |
|------------------------------------------|---------|-------------------------------------------------------------------------|
| `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS`| `30`    | How often a live run's heartbeat is refreshed. Failure is non-fatal.    |
| `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` | `900`   | Heartbeat silence past this marks the row dead. MUST be >= 2x heartbeat (startup refuses otherwise). |
| `PYTHON_AGENT_SWEEP_INTERVAL_SECONDS`    | `300`   | Sweeper cron cadence. Worst-case reap latency = timeout + interval.     |
| `PYTHON_AGENT_SWEEP_ENABLED`             | `true`  | Rollback switch: `false` stops reaping (heartbeats keep writing).       |

Reaping does **not** delete langGraph checkpoint data — a reaped run's
checkpoint thread is collected by the retention cron when it later
deletes the terminal row, so a crashed run's state survives the same
retention window as any other run (see
`openspec/changes/archive/2026-10-14-agent-stale-run-sweeper/` for the full rationale).

## Run retention: pruning terminal rows + checkpoints

An hourly cron (`RetentionCron`, `agent/retention.py`) hard-deletes
terminal (`completed` / `failed`) `agent.runs` rows whose `ended_at` is
older than the retention window and, per deleted row, deletes its
langGraph checkpoint thread (`adelete_thread`) — langGraph ships no
retention of its own, so this is the only path that prunes
`langgraph.checkpoint_*`. Per-run GC failure is logged at `WARN` and
does not abort the batch. Counter `agent_run_retention_deleted_total`;
OTel span `agent.run.retention`.

| Env var                                   | Default | Purpose                                    |
|-------------------------------------------|---------|--------------------------------------------|
| `PYTHON_AGENT_RETENTION_ENABLED`          | `true`  | Rollback switch: `false` stops the cron.   |
| `PYTHON_AGENT_RETENTION_DAYS`             | `30`    | Terminal rows older than this are deleted. |
| `PYTHON_AGENT_RETENTION_INTERVAL_SECONDS` | `3600`  | Cron cadence.                              |

## Chat history, session API, and user preferences

Coupled surfaces sharing the same DB and lifecycle (see
`openspec/changes/archive/2026-10-14-agent-chat-history-storage/`,
`2026-10-14-agent-chat-history-ui/`, `2026-10-14-agent-user-preferences/`, and
`2026-10-16-agent-chat-fixes/`):

- **Chat history** — every turn is persisted (`agent.chat_sessions` /
  `agent.chat_messages`); a two-phase retention (soft-delete + grace,
  then hard-delete) collects it after the window. Hard-deletes also
  collect the session's derived preference rows, with a per-tick orphan
  sweep as a backstop.
- **Session API** — `GET/POST/DELETE /agent/sessions*` serves the SPA's
  history sidebar (list, replay, create, resume, soft-delete),
  owner-scoped via `X-Nyx-User-Id`. An echoed `run_id` on
  `/agent/run` continues the matching live session.
- **Session titles** — the session's first exchange titles it: the
  session-init block derives a deterministic label from the first user
  message (whitespace collapsed, 48 characters max, CJK-safe) and writes
  it through `set_title_if_empty`, whose `title=''` predicate makes the
  winning run the session's first exchange by construction. That run then
  spawns ONE fire-and-forget refinement — a small `planner_model` call
  with bounded `max_tokens`/`timeout`, never charged to the chat budget —
  which overwrites the label through the ownership + not-soft-deleted
  guarded `set_title`. Any failure (store, LLM, unusable output) logs a
  WARN, keeps the deterministic label, never retries, and never touches
  the SSE stream.
- **User preferences** — after a reply completes, a fire-and-forget
  task extracts stated preferences from the turn's last user message
  (gated by a conservative EN/ZH heuristic, never charged to the chat
  budget) and upserts them per `(user_id, key)`. The planner AND the
  executor (both answer paths, `llm` and `react`) gain a preferences
  block whose lead-in also covers recall questions; with zero prefs the
  prompt is byte-identical to the pre-injection form.
- **GDPR export + erasure** — `GET /agent/me/export` returns one JSON
  body with every row tied to the `X-Nyx-User-Id` subject (sessions
  incl. soft-deleted, messages, prefs, runs, checkpoint thread ids);
  `DELETE /agent/me` physically erases the same footprint (chat →
  prefs → runs → langGraph checkpoint threads) and responds with
  per-table counts, idempotent on a second call. Every served
  operation appends a counts-only row to `agent.data_audit_log`, whose
  own retention window is separate (3y default). See
  `openspec/changes/archive/2026-10-14-agent-gdpr-compliance/`; operator playbook in
  `docs/agent-deployment.md` » GDPR data handling.

| Env var                              | Default | Purpose                                                                 |
|--------------------------------------|---------|-------------------------------------------------------------------------|
| `PYTHON_AGENT_CHAT_RETENTION_DAYS`   | `365`   | Chat-history floor (one year). Values below 365 refuse startup.          |
| `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS` | `7` | Soft-delete grace before the hard-delete.                                |
| `PYTHON_AGENT_CHAT_RETENTION_ENABLED`| `true`  | Rollback switch: `false` halts the chat-history GC only.                 |
| `PYTHON_AGENT_SESSIONS_PAGE_SIZE`    | `10`    | Default sidebar page size for `GET /agent/sessions` (`[1, 50]`).         |
| `PYTHON_AGENT_PREFS_ENABLED`         | `true`  | Rollback switch for preference extraction AND planner injection.         |
| `PYTHON_AGENT_PREFS_MAX_INJECTED`    | `50`    | Top-N preferences injected into the planner prompt (≥ 0).                |
| `PYTHON_AGENT_GDPR_ENABLED`          | `true`  | Kill switch for `GET /agent/me/export` + `DELETE /agent/me` (503; nothing read or written). |
| `PYTHON_AGENT_AUDIT_RETENTION_DAYS`  | `1095`  | `agent.data_audit_log` window (3y); values below 365 refuse startup.     |

Counters `agent_chat_retention_deleted_total` (session hard-deletes),
`agent_chat_prefs_deleted_total` (orphan-sweep recoveries — a nonzero
rate there means the hot-path prefs cleanup is failing), and
`agent_audit_retention_deleted_total` (audit rows reaped; store-owned —
incremented by `AuditLog.retention_delete`, not the cron); spans
`agent.chat.retention`, `agent.audit.retention`, `agent.gdpr.export` /
`agent.gdpr.erase` + per-endpoint `agent.sessions.*`.

## OTel spans: Jaeger (on by default)

Run/step/tool spans export over OTLP/HTTP to Jaeger (`make up` publishes
the `:16686` UI and `:4318` OTLP/HTTP receiver). Export is on by default;
`PYTHON_AGENT_OTEL_ENABLED=false` skips the Jaeger exporter entirely (the
separate Langfuse pipeline is unaffected), so a deployment without a
collector does not emit background export-retry diagnostics. `PYTHON_AGENT_OTEL_ENDPOINT` defaults to
`http://localhost:4318` for host dev; the compose agent service sets
`http://jaeger:4318`.

## LLM tracing: Langfuse (opt-in)

Structured, per-run LLM traces — planner and executor prompts, model
replies, provider-reported token usage, plan steps, and tool calls —
exported to a self-hosted [Langfuse](https://langfuse.com) instance.
This is the prompt/response-level counterpart to the OTel spans in
Jaeger (which stay metadata-only). Tracing is **best-effort**: an
unreachable or slow Langfuse never affects a run's SSE stream.

```
make dev-up-langfuse      # langfuse-web/worker + clickhouse/minio/redis/postgres
# UI: http://localhost:3000  (org/project/keys bootstrapped from LANGFUSE_INIT_*)
make dev-down-langfuse    # stop the stack only (volumes preserved)
```

| Env var                            | Default                  | Purpose                                                                 |
|------------------------------------|--------------------------|-------------------------------------------------------------------------|
| `PYTHON_AGENT_LANGFUSE_ENABLED`    | `false`                  | Opt-in switch. `true` without a key pair refuses startup.               |
| `PYTHON_AGENT_LANGFUSE_HOST`       | `http://localhost:3000`  | UI/API base URL. Compose sets `http://langfuse-web:3000` for the container. |
| `PYTHON_AGENT_LANGFUSE_PUBLIC_KEY` | —                        | Must match the root `.env`'s `LANGFUSE_INIT_PROJECT_PUBLIC_KEY`.        |
| `PYTHON_AGENT_LANGFUSE_SECRET_KEY` | —                        | Must match the root `.env`'s `LANGFUSE_INIT_PROJECT_SECRET_KEY`.        |

Traces are keyed by run: `run_id` (= langGraph `thread_id`) is the
session id and the verified `X-Nyx-User-Id` is the trace user, so a run
is replayable end-to-end from the UI and filterable per user.
Credential-shaped strings in tool arguments/results are masked before
export. See `openspec/changes/archive/2026-10-14-agent-langfuse-tracing/` for the design
(including why the Langfuse client uses its own isolated OTel provider:
the global provider belongs to the Jaeger export pipeline).

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
# fill in PYTHON_AGENT_LLM_BASE_URL + PYTHON_AGENT_LLM_API_KEY + AGENT_SERVICE_TOKEN
uv sync --locked --extra dev
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

The Python service does **not** verify JWTs. Inbound requests are authenticated with the
shared `AGENT_SERVICE_TOKEN` (at least 32 bytes, the same value on the Go backend and the
agent; the Python service refuses to start without it, while the Go API keeps running and
answers its agent-backed routes 503):

- **Service-token gate**: a pure-ASGI middleware requires `X-Nyx-Service-Token` on every
  route except the exact `/healthz` probe path. Missing or mismatched values get HTTP 401
  `{"detail":"unauthorized"}` before any handler or rate-limit state; the comparison is
  constant-time over bytes, so non-ASCII header values are rejected cleanly.
  `/metrics` on the FastAPI port is gated too; the separate Prometheus exporter on 9001
  is outside FastAPI and unchanged.
- **Go's chat handler** verifies the user JWT at the SPA edge, then forwards the
  verified user id in the `X-Nyx-User-Id` header. Python reads this header directly
  and uses it for the per-user budget counter and per-owner scoping on tool calls.
- The `Authorization` bearer token is forwarded as a passthrough to Go's `/mcp` and
  `/internal/rag/retrieve` endpoints, which re-verify it themselves (Python never
  decodes the token).
- **Defense-in-depth**: the compose `internal` network, the unpublished 9000 port, and
  an in-process per-IP sliding-window limiter on `/agent/run`
  (`PYTHON_AGENT_REQUESTS_PER_SECOND`, default `100`; `PYTHON_AGENT_BURST`, default
  `200` — burst is the ceiling within any 1-second window). `/healthz` and `/metrics`
  are exempt from the limiter; `/metrics` still requires the service token.

Rotation is ordered (Go first, then the agent) and briefly rejects whichever side still
holds the old value. See [`docs/agent-deployment.md`](../../docs/agent-deployment.md) for
the env setup, health-path precheck, deploy order, rotation, and rollback runbook.