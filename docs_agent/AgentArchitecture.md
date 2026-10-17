# Nyx Python Agent — Architecture Notes

Learning doc for `../backend/agent_py/`. Paths are repo-root-relative unless a tree is shown. Reflects the working tree as of 2026-10-17,
after the run-state / planner-structured / tool-retry / stale-run-sweeper slices
and the later memory, tool, observability, and eval slices:
`agent-chat-history-storage` + `-ui`, `agent-feed-tools` (planner registry
awareness + HITL confirmation), `agent-web-tools`, `agent-langfuse-tracing`,
`agent-user-preferences`, `agent-gdpr-compliance`, `agent-chat-fixes`,
`agent-stream-timeout-frame`, and `agent-golden-eval`.

The "What's not done yet" section at the end is the historical 2026-10-12 gap
analysis, kept as a record; resolved items are annotated in place and its line
references are intentionally frozen at that snapshot.

---

## 1. Where it sits

```
┌────────┐  POST /api/chat          ┌──────────────┐   POST /agent/run     ┌──────────────┐
│  SPA   │ ───────────────────────▶ │  Go backend  │ ────────────────────▶ │  agent_py    │
│ React  │  /api/chat/approvals     │  (JWT verify)│   X-Nyx-User-Id +     │  FastAPI     │
│        │  /api/chat/sessions*     │  proxies     │   Authorization pass  │              │
│        │ ◀─────────────────────── │              │ ◀──────────────────── │              │
└────────┘  SSE bytes              └──────────────┘   SSE bytes            └──────┬───────┘
                                          │                                       │
              ┌───────────────────────────┼───────────────────────────┬───────────┤
              ▼                           ▼                           ▼           ▼
        Go /mcp (tools)      Go /internal/rag/retrieve         Postgres      Redis
        JSON-RPC 2.0         GET ?query= (JWT-scoped)        public /       budget +
        JWT re-verified      JWT re-verified                  langgraph /    approval
                                                              agent schemas  store

        Optional: Langfuse (LLM traces)      Tavily + public web (agent-native web tools)
```

- The Python service **never verifies JWTs**. Go verifies at the SPA edge and
  forwards the verified id in `X-Nyx-User-Id`. The bearer token is a passthrough
  that Go's `/mcp` and `/internal/rag/retrieve` re-verify themselves.
- Trust boundary at this hop is the **network** (compose bridge / k8s
  NetworkPolicy). Port 9000 is not published to the host; only 9001 (metrics).
- Defense in depth on top of that: per-IP rate limit on `/agent/run`
  (`ratelimit.py`), every metadata/chat/GDPR SQL read is scoped by `user_id`,
  and the mutating-tool approval gate fails closed.
- Owner-scoped subject endpoints: `/agent/sessions*` (history sidebar) and
  `/agent/me/export` + `DELETE /agent/me` (GDPR) read `X-Nyx-User-Id` directly and
  have **no admin override**. The SPA reaches sessions/approvals through Go
  (`/api/chat/sessions*`, `/api/chat/approvals`); the GDPR endpoints are
  operator-only and are not proxied.
- Approval decisions are Redis-brokered (`approvals.py`), so a decision recorded
  on any replica resolves the waiting run.

## 2. Request lifecycle

1. `POST /agent/run` (`main.py:642`) — no auth, but **400 if `X-Nyx-User-Id`
   missing or non-integer** (prevents silent `user_id=0` budget collisions).
2. Body validated as `ChatRequest` (`types.py:64`): `{messages, agent?, model?,
   run_id?}`. There is no `session_id` field; the client echoes the active
   session id as `run_id`.
3. `_stream_agent_run` (`main.py:778`) is the generator FastAPI wraps in a
   `StreamingResponse` (`text/event-stream`).
4. Run id resolution (`main.py:832-868`) is **three-way**:
   - echoed id + live `agent.runs` row (`status` not terminal) →
     **crash-resume** from checkpoint;
   - otherwise, echoed id that matches a live `chat_sessions` row →
     **session continuation**: same session, fresh graph, and
     `persist_run_state=False` (no new `agent.runs` row, no heartbeat);
   - otherwise → fresh run, minting a uuid4 (or using the echoed id verbatim
     when persistence is off).
5. Frames emitted **before** the graph starts: `run_id`, then
   `note{kind:"resumed"}` or `note{kind:"fresh_run"}`.
6. Liveness heartbeat starts only when `run_metadata is not None` **and**
   `persist_run_state` (`main.py:1067-1077`): a per-run task
   (`_heartbeat_loop`, `main.py:558`) touches `agent.runs.last_heartbeat_at`
   every `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (30 s, immediate first
   touch). Touch failures log a warning and are otherwise inert.
7. Graph streams. Delivery is **per LangGraph node snapshot**, via a `RunSink`
   in live mode (`graph.py:224-228,259-267`); events appended inside a node reach
   the wire at the next node boundary. The approval gate flushes mid-node at its
   two explicit flush points (`approvals.py:230,245`).
8. `finally`: cancel the heartbeat **before** the terminal write
   (`main.py:1158`), then persist the terminal state under `asyncio.shield`
   (`main.py:1174-1239`) so a client disconnect cannot orphan the row; mark
   `completed` (total tokens; clears `error_code`) or `failed`. `agent_unavailable`
   deliberately leaves the row `running` — the stale-run sweeper reaps it after
   `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` of heartbeat silence.
9. Terminal framing: `error`/`done` frames, then the `data: [DONE]` sentinel.
   The frontend parser returns on `event: done` (`frontend/src/features/chat/services/chatService.ts:347-353`) and
   `event: error` (`:406-412`) *before* reading `[DONE]`; `[DONE]` is the fallback
   for streams that omit both. The legacy `event: terminator` frame is still
   emitted for old clients but has no branch in the current parser.
10. The Go proxy bounds stream duration (`LLM_MAX_STREAM_DURATION`, default
    10 m) and closes with typed frames on timeout (see §5 and
    `AgentFaultTolerance.md` row 24).

## 3. The graph

`agent/graph.py:97` — `build_graph`; the LangGraph `StateGraph` is instantiated
at `:133`:

```
START → planner → executor → END
```

- **planner** (`planner.py:99`) — one `langchain-openai` call on the structured
  path (default). The client binds an explicit OpenAI `response_format`
  json_schema (`strict: false`) and validates the content JSON with Pydantic
  (`_build_structured_client`; deliberately not `with_structured_output` — its
  docstring documents the strict-mode `args:{}` trap). Failures
  (`ValidationError` / `OutputParserException`) re-prompt with the validator's
  error under a `PREVIOUS_ATTEMPT_FAILED` block, bounded by
  `planner_retry_max` (default 1); persistent failure → typed
  `error{code: "plan_invalid"}`. `plan_too_large` (> `plan_max_steps` = 10) and
  duplicate step ids are typed errors too. A legacy free-text path
  (fence-strip + `json.loads`, `_parse_and_validate` at `:534-560`) stays
  reachable via `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false` for vLLM
  parsers that don't honour `response_format`; both paths apply equivalent
  post-parse enrichment — `_enforce_plan_shape` on the structured path, an
  inline copy inside `_parse_and_validate` on the legacy path (parity pinned
  by `tests/test_planner_parity.py`), so the wire `plan` frame is identical.
  - The planner prompt injects the user's persisted preferences (top
    `prefs_max_injected`, default 50) and, when the registry call succeeds, a
    "Registry tools available" block built from the live MCP catalog
    (`planner.py:124-129,280-286`; `tool_dispatch.py:216-239`). A catalog outage
    degrades to the no-tools prompt; with zero prefs the prompt is byte-identical
    to the pre-injection form.
- **executor** (`executor.py:78`) — sequential over `plan.steps`. Four step
  kinds (`StepKind` in `types.py`):

  | kind  | handler   | mechanism                                                  |
  |-------|-----------|------------------------------------------------------------|
  | rag   | `_run_rag` (`:239`)| GET Go RAG bridge; unavailable → empty context (not fatal) |
  | tool  | `_run_tool` (`:253`)| `ToolDispatcher.call` — local tools first, then MCP `tools/call` — through `_dispatch_tool_with_retries` (`:737`) |
  | llm   | `_run_llm` (`:287`)| single LLM completion, result streamed as one `delta`      |
  | react | `_run_react` (`:328`)| nested model→tool loop, tools bound from `ToolDispatcher.list_tools` (local + MCP), capped by `step.max_iterations` or `step_react_max_iterations` (5); dispatches share the same retry path |

- **Tool dispatch funnel** (`tool_dispatch.py`) — merges agent-native tools
  (`web_search`, `web_fetch`) with the MCP registry, feeds the planner catalog,
  owns the read-only annotation policy, and evaluates the confirmation gate.
  Every non-read-only registry call pauses the run on a `confirm_request` frame
  (`approvals.py:195-257`) until `POST /agent/approvals` records a decision.
  Deny / TTL expiry (`PYTHON_AGENT_APPROVAL_TTL_SECONDS`, default 300) / broker
  outage all close as model-visible denials, never auto-approve. Local
  agent-native tools are exempt from confirmation.
- **Budget guard** is consulted before every **executor** model call
  (`_run_llm` `:296`, `_run_react` per iteration `:372`, tool-arg re-prompt
  `:976`). It is **not** consulted by the planner or by the post-run preference
  extraction. Exhaustion short-circuits the whole run with
  `error{code:"budget_exceeded"}` + `done` + `terminator`.
- Step failures are contained: a failed step appends `step_end{status:"failed"}`
  and the loop continues; when the plan diverged, the executor inserts
  `note{kind:"partial_failure", failed_steps:[...]}` **mid-list** (via
  `events.insert`, `executor.py:1315`) so it precedes the final answer's deltas
  (`_maybe_emit_partial_failure`, called at `:231` on normal exit and `:726` on
  error termination; defined at `:1298`). Only budget errors abort the whole run
  (`_terminate_with_error`, `:714`) — the `react` iteration-cap fails its step
  and the run continues, but `react_iterations_exceeded` is still in
  `PLAN_LEVEL_ERROR_CODES`, so the note fires either way.
- **Empty-answer guard**: a run that completes with no deltas emits
  `error{code:"empty_answer"}` + done + terminator (`graph.py:269-302`).
- **Checkpointer** (when Postgres is configured): `AsyncPostgresSaver` compiled
  into the graph; its tables are created by `checkpointer.setup()` during
  lifespan (`main.py:405-407`). Every node transition is checkpointed under
  `thread_id = run_id`. Resume passes `astream(None, ...)` only when the run is
  a crash-resume, a graph config exists, and the checkpoint has values
  (`graph.py:239-254`); otherwise the fresh state is passed. The executor skips
  any step whose output already exists in `step_outputs` (`executor.py:114-115`).
  Custom pydantic state types are governed by a serde allowlist
  (`main.py:521-529`). In tests, `checkpointer=None` → pure in-memory.

`run_agent` (`graph.py:151`) is the non-streaming `ainvoke` variant kept for
signature parity / tests; the live path is `run_agent_stream` (`graph.py:201`).
In live mode delivery rides the `RunSink` queue and the generator yields
nothing; the `last_len` watermark behavior applies only to sink-less test mode.

## 4. State

`AgentState` (`state.py:14`) is a `TypedDict(total=False)`:

- inputs: `user_id`, `run_id`, `request_id`, `messages`, `model`
- outputs: `plan`, `step_outputs` (per-step results keyed by step id),
  `cumulative_tokens`
- retry bookkeeping: `tool_retry_used` (per-step, per-tool consumption) and
  `tool_terminal_failures` (step → tools whose dispatch ended terminally; feeds
  the partial-failure banner without re-deriving exhaustion from frames)
- `events_so_far` — event list; the SSE emitter's source. It is **not strictly
  append-only**: the partial-failure note is inserted mid-list.
- terminal markers: `terminal_status` (`ok` | `failed` | `budget_exceeded` |
  `budget_unavailable`), `terminal_reason`

Node contract: mutate the state, append events, return state. Live delivery
does not diff `events_so_far` by length — `RunSink.flush` dedupes by object
identity (`approvals.py:76-94`); only sink-less mode uses the `last_len`
watermark.

## 5. SSE wire contract

`_sse_encode` (`main.py:1344`): `event: <kind>\ndata: <flat json>\n\n` — the
payload is the typed event object directly, no `{kind, data}` wrapper. Kinds
(`types.py:211-224`):

| kind        | payload                                             |
|-------------|-----------------------------------------------------|
| `run_id`    | `{run_id}` (always first frame)                     |
| `note`      | `{text, provider?, kind?, last_step_id?, run_id?, failed_steps?}` — `kind` ∈ `resumed` \| `fresh_run` \| `partial_failure` |
| `plan`      | `{steps: [...]}`                                    |
| `step_start`| `{step_id, kind}`                                   |
| `tool_call` | `{step_id, call_id, name, args}`                    |
| `tool_result`| `{step_id, call_id, ok, result?, error?, retry_count?}` |
| `step_end`  | `{step_id, status, reason?}`                        |
| `confirm_request` | `{run_id, step_id, tool, args, approval_id, expires_at}` — mutating-call approval card; after `expires_at` it closes as denied |
| `delta`     | `{delta: "<text>"}`                                 |
| `done`      | `{usage: {prompt_tokens, completion_tokens, total_tokens}}` |
| `error`     | `{error, code?, request_id?}`                       |
| `terminator`| `{reason?}` — legacy; the current parser doesn't read it |

Compatibility: additive-only. Old clients ignore unknown kinds and never send
`run_id`, so they always get a fresh run. The frontend parser is the consumer of
record — per repo convention, wire-shape changes must round-trip through *its*
parser, not just producer-side tests. Go's proxy adds one wire-level terminal
case of its own: `error{code:"stream_timeout"}` + `done` + `terminator` when
`LLM_MAX_STREAM_DURATION` cuts the stream.

## 6. Persistence

Three schemas, five persistence surfaces (LangGraph checkpoints, runs, chat,
prefs, audit):

| schema      | owner             | contents                                  |
|-------------|-------------------|-------------------------------------------|
| `public`    | Go (`golang-migrate`) | `users`, `refresh_tokens`, `feeds`, `feed_embeddings` |
| `langgraph` | LangGraph framework | `checkpoint_*` tables (opaque state)    |
| `agent`     | Python (alembic)  | `runs`, `chat_sessions`, `chat_messages`, `chat_user_prefs`, `data_audit_log`, `alembic_version` |

- DSN carries `options=-c search_path=langgraph,agent` (no `public` — a typo like
  `FROM feed` raises rather than silently touching Go's data).
  `config.py:319` auto-encodes the option so operators can paste a readable URL.
- **Run metadata** (`run_metadata.py`) — asyncpg pool, 3-retry backoff on
  `PostgresConnectionError`, otherwise `RunMetadataUnavailable`. Metadata writes
  are non-fatal and **not surfaced on the wire**: a `create_run` failure logs a
  warning and falls back to a local uuid; a terminal-write failure logs a
  warning. Terminal writes are authoritative over a premature reap
  (`mark_completed` clears `error_code`). A checkpoint-write failure is different:
  it propagates out of `_pump_events`, so the Go proxy logs and closes the stream
  with no typed frame.
- **Heartbeat + sweeper** (`agent-stale-run-sweeper`, `0a9b8f5`): while a run
  streams, `_heartbeat_loop` (`main.py:558`) touches `last_heartbeat_at` every
  30 s; `StaleRunSweeper` (`sweeper.py`) runs every 300 s and flips rows with a
  heartbeat older than 900 s to `failed`/`error_code='stale_run'` (conditional
  on `status='running'`; OTel span `agent.run.sweep` + counter
  `agent_runs_swept_total`). `running` means "heartbeating", and the timeout is
  also the crashed-run **resume window**. `PYTHON_AGENT_SWEEP_ENABLED=false`
  stops reaping; heartbeats keep writing.
- **Chat history** (`chat_store.py`, migration 0004): `chat_sessions`
  (with `deleted_at`) + `chat_messages` (with `tokens`). Five owner-scoped
  endpoints live in `api/chat_sessions.py`; the SPA reaches them through
  `../backend/internal/chat/sessions_proxy.go`. User messages are written at
  request entry, the assistant message at terminal; session titles derive
  deterministically from the first exchange (48 chars) and are refined by one
  fire-and-forget `planner_model` call. Writes are best-effort warn-only.
- **User preferences** (`prefs.py`, migration 0005): `chat_user_prefs` keyed
  `(user_id, key)`; post-run extraction is detached, never budget-charged, and
  injected into the planner **and** both executor answer paths. Derived rows are
  deleted with their source session, with a per-tick orphan sweep as backstop.
- **GDPR + audit** (`api/gdpr.py`, `audit.py`, migration 0006): export returns
  `{user_id, exported_at, sessions, messages, prefs, runs, checkpoint_thread_ids,
  audit_entries}`; erasure physically deletes messages → sessions → prefs (one
  transaction), then runs, then checkpoint threads (deduped run-id ∪
  session-id union), and appends a counts-only `data_audit_log` row. Audit
  retention defaults to 1095 d with a 365 d floor.
- **Migrations**: alembic, `0001_langgraph_checkpoint.py` is a no-op version
  anchor (checkpoint tables come from `checkpointer.setup()`) → `0002` runs
  metadata → `0003` heartbeat → `0004` chat sessions/messages → `0005` prefs →
  `0006` audit log. Dev: `migrations/_dev_upgrade_hook.py` runs on lifespan
  startup. Prod: dedicated `agent-migrate` one-shot; `agent` waits on
  `service_completed_successfully`; replicas never race on alembic.
- **Retention** (`retention.py`): asyncio cron (default hourly).
  - Run iteration hard-deletes terminal rows older than `retention_days` (30)
    and calls `adelete_thread` per deleted run.
  - Chat iteration soft-deletes then hard-deletes past the 365 d floor + 7 d
    grace, and calls `adelete_thread` per hard-deleted session.
  - Prefs iteration collects derived rows (plus an orphan sweep).
  - Audit iteration has its own 1095 d window, independent of the GDPR kill
    switch.
  - Per-row GC failure is logged at WARN and the metadata delete stands.
  - Uses Postgres `now()`; OTel span + Prometheus counter per iteration.
- **No DB configured** → loud warning at startup, runs not persisted, resume
  disabled. Fail-fast DSN check (`SELECT 1`) at boot so a bad URL dies in
  lifespan, not on first request.

## 7. Budget guard

`budget.py` — Redis counter `agent:budget:u={user_id}:h={UTC hour}`, TTL 90 min
(TTL only GCs old counters; the hourly key itself enforces the window).

- `INCRBY` first, then compare; over-cap increments are **rolled back**
  (`DECRBY`) so refused calls don't burn the user's budget.
- Fails **closed**: any Redis error → `decision="unavailable"` → run aborts
  with `budget_unavailable`. Never raises.
- Cap from unprefixed `AGENT_BUDGET_PER_USER_PER_HOUR` (default 200000) — a
  deliberate cross-runtime shared env var, historical contract with the Go
  agent.
- Scope: executor model calls only (§3). Planner and preference extraction are
  not budgeted.

## 8. Health & observability

- `/healthz`: Redis ping **and** LLM `GET {base}/models` — either failing → 503
  with a discriminating `status` field. 5xx from the provider counts as down;
  4xx (e.g. auth) does not.
- `/metrics`: Prometheus exporter on `:9001` (separate daemon thread).
  Counters: `agent_runs_total`, `agent_steps_total`, `agent_tool_calls_total`,
  `agent_tokens_total`, `agent_planner_parse_failures_total{retry}`,
  `agent_tool_retries_total`, `agent_run_retention_deleted_total`,
  `agent_runs_swept_total`, `agent_chat_retention_deleted_total`,
  `agent_chat_prefs_deleted_total`, `agent_audit_retention_deleted_total`,
  `agent_gdpr_audit_write_failed_total`.
- OTel: spans `agent.run`, `agent.step.{kind}`, `agent.step.plan`,
  `agent.step.react` (per iteration), `agent.planner.structured` /
  `agent.planner.retry` / `agent.planner.legacy`, `agent.budget`,
  `agent.run.retention`, `agent.run.sweep`, `agent.chat.retention`,
  `agent.audit.retention`, `agent.sessions.*`, `agent.approvals.decide`,
  `agent.gdpr.export` / `agent.gdpr.erase` → OTLP/HTTP → Jaeger, same collector
  as Go. `traceparent` continuity exists browser→Go only: the Go→agent
  proxy injects no context (`agent_proxy.go` uses a plain transport) and
  agent_py extracts none, so agent spans are currently Jaeger roots.
- **Langfuse (opt-in)**: `PYTHON_AGENT_LANGFUSE_ENABLED=true` exports per-run
  LLM traces (planner/executor prompts, replies, provider usage, plan/step/tool
  observations) to a self-hosted Langfuse. The LangChain callback is attached to
  `graph_config`; session id = run id, user id = `X-Nyx-User-Id`; credential-shaped
  strings are masked before export. Best-effort: init/callback/export failures
  never affect the run, and shutdown flush is bounded (5 s).
- Logging: root at WARNING; the `agent` package and the `nyx.agent.*` runtime
  tree at INFO with `timestamp level logger: message` lines, so startup state
  and tracing/export failures are visible by default.

## 9. Config (`config.py`)

- `pydantic-settings`, prefix `PYTHON_AGENT_`, `.env` auto-loaded. Process-wide
  lazy singleton (`get_settings`).
- Startup validators: refuse to boot if `llm_base_url` targets
  `api.openai.com` with no API key (vLLM/OpenRouter don't need one); DSN
  search_path normalization; retry caps non-negative; sweep intervals ≥ 1 and
  `stale_run_timeout_seconds` ≥ 2× `heartbeat_interval_seconds` (a smaller
  ratio could get a live run reaped); chat retention ≥ 365 d; audit retention
  ≥ 365 d — each refusal names the offending env var.
- Notable knobs: `database_url` / `migrations_disabled` / `retention_*` /
  `database_pool_*`, rate limit `requests_per_second` (100) + `burst` (200),
  `plan_max_steps` (10), `step_react_max_iterations` (5), `planner_retry_max`
  (1) + `planner_structured_enabled` (true), `tool_retry_max` (2) +
  `tool_retry_backoff_base_ms` (250) / `tool_retry_backoff_ceiling_ms` (5000),
  `heartbeat_interval_seconds` (30), `stale_run_timeout_seconds` (900),
  `sweep_interval_seconds` (300), `sweep_enabled` (true),
  `approval_ttl_seconds` (300), `chat_retention_days` (365) +
  `chat_retention_grace_days` (7) + `chat_retention_enabled` (true),
  `sessions_page_size` (10), `prefs_enabled` (true) + `prefs_max_injected` (50),
  `gdpr_enabled` (true) + `audit_retention_days` (1095),
  `langfuse_enabled` (false) + `langfuse_host` / key pair,
  `web_fetch_*` / `web_search_*` (both tools default off).

## 10. File map

```
backend/agent_py/
  agent/
    main.py          FastAPI app, lifespan wiring, routers, /agent/run SSE generator,
                     run-id resolution, heartbeat, chat/session/title writes
    graph.py         StateGraph build + streaming runner + empty-answer guard
    state.py         AgentState TypedDict + new_state()
    types.py         Pydantic wire models (plan, events, request) — the contract
    planner.py       plan_node: structured output, bounded retry, prefs + registry catalog
    executor.py      executor_node + rag/tool/llm/react handlers, retry funnel, approval gate
    tool_dispatch.py composite dispatcher (local + MCP), readOnlyHint policy, planner catalog
    web_tools.py     agent-native web_search (Tavily) + web_fetch (untrusted framing)
    net_guard.py     SSRF guard (deny-list, DNS pinning, redirect validation)
    approvals.py     RunSink, Redis-brokered ApprovalStore, ApprovalGate, denial text
    approvals_api.py POST /agent/approvals
    api/             chat_sessions.py (sessions routes), gdpr.py (export/erase), deps.py
    chat_store.py    agent.chat_* CRUD, titles, session lifecycle
    prefs.py         preference extraction + prompt block
    audit.py         append-only agent.data_audit_log
    budget.py        Redis hourly token budget (fail-closed)
    tools_client.py  MCP JSON-RPC client (tools/list, tools/call)
    rag_client.py    Go RAG bridge client (GET ?query=)
    ratelimit.py     per-IP limiter middleware (defense in depth)
    observability.py OTel setup/teardown, Prometheus counters, Langfuse tracing
    run_metadata.py  agent.runs CRUD + asyncpg pool (heartbeat, sweep, retention SQL)
    retention.py     cron: runs + chat + prefs + audit retention, checkpoint GC
    sweeper.py       stale-run reaper cron
  migrations/        alembic: 0001 checkpoint anchor, 0002 runs, 0003 heartbeat,
                     0004 chat, 0005 prefs, 0006 audit
  tests/             fakeredis + httpx.MockTransport; no live network needed
evals/               promptfoo golden-set + security suites (repo root)
```

## 11. Known gaps / next slices

Resolved since the 2026-10-12 gap analysis:

- Stale-run sweeper (heartbeat + reaper) — `agent-stale-run-sweeper`.
- Planner structured output + bounded retry — `agent-planner-structured`.
- Tool-arg / transient retries + partial-failure banner — `agent-tool-dispatch-retry`.
- Checkpoint GC — retention (`adelete_thread` on run and chat deletes) and GDPR
  erasure, not a single retention-only path anymore.
- Server-side conversation history + sidebar — `agent-chat-history-storage` +
  `-ui`, then `agent-chat-background-runs` and `agent-chat-fixes`.
- Cross-session preference memory — `agent-user-preferences` (planner + executor
  injection).
- GDPR export/erasure + audit log — `agent-gdpr-compliance`.
- HITL approval gate for mutating registry tools — `agent-feed-tools` +
  `agent-approval-countdown`.
- Web tools (Tavily search, SSRF-guarded fetch) — `agent-web-tools`.
- LLM tracing — `agent-langfuse-tracing`; log visibility — `agent-langfuse-log-visibility`.
- Golden-set / security evals — `agent-golden-eval` (`evals/`).

Still open:

- `/agent/runs` list endpoint: `run_metadata.list_runs_for_user` exists
  (`run_metadata.py:213`) but no HTTP route calls it.
- Transient retry for ReAct / `llm` model calls (fault index row 19) and
  loop/cycle detection.
- Provider failover: the Python agent builds `ChatOpenAI` directly against
  `llm_base_url`; Go's `llm.Router` does not cover agent runs.
- Cross-session conversation summary and top-K history recall (preference
  recall is done).
- Prompt registry / A-B harness; tool-policy deny layer (`agent-tool-policy-gate`
  is an active, unimplemented OpenSpec change — default-off `AGENT_TOOL_POLICY_PATH`).
- Python/TypeScript SDK distribution for third-party clients.
- The `*-rollout` siblings remain active and PM-owned.


# What's not done yet (historical — gap analysis as of 2026-10-12)

> **HISTORICAL SNAPSHOT.** The text below was written on 2026-10-12 against the
> then-current tree. Resolved items are annotated in place; all line references
> in this section are frozen at that snapshot and must not be read as current.
> Use §11 above for the live status.

Comparison against industry practice (cross-referenced with the actual code under backend/agent_py/):

1. Stuck-run sweeper — missing the industry baseline — **RESOLVED 2026-10-14** by `agent-stale-run-sweeper` (heartbeat + timeout reaper; the stale timeout is also the resume window). Historical analysis kept below.

Industry baseline (workflow engines: Temporal/Celery; durable agents: Inngest, Restate): heartbeat/lease — worker periodically bumps last_heartbeat_at; a sweeper reaps rows where status='running' AND last_heartbeat_at < now() - timeout. Or the orchestrator holds a server-side lease that auto-expires.

What the code did (historical line refs: agent/main.py:451-457, agent/retention.py):
- agent_unavailable deliberately leaves the row as running. No last_heartbeat_at column exists in agent.runs. *(Since 0a9b8f5 the column exists — migration 0003.)*
- RetentionCron._run_once only deletes status IN ('completed','failed') — running rows are untouched. There is no per-row timeout, no lease, no heartbeat thread. *(Since 0a9b8f5 the heartbeat and reaper exist.)*
- Comment at main.py:457 explicitly defers this to "slice 4." *(No such comment in the current tree.)*

Gap: A crash between mark_failed/SSE-flush and Postgres write leaves the row running indefinitely; retention never reaps it; the user's only escape is to ignore the run_id. Industry calls this worker GC, not optional.

2. Fault tolerance — primitive self-correction only — **RESOLVED 2026-10-13** by `agent-planner-structured` + `agent-tool-dispatch-retry` (planner structured output + retry; tool-dispatch retry budget; partial-failure banner). Historical analysis kept below.

Industry baseline (ReAct/Reflexion write-ups):
- Use native structured tool calling (LangChain with_structured_output, OpenAI tools=, Claude tool-use). JSON-parse of free text is the single biggest source of malformed-args failures.
- Schema validation (Pydantic/Zod) before invoking the tool; on validation failure, return a normalized error string to the model.
- Loop detection on hashed (action, args, observation) tuples; force-reflection on repetition.
- Retry budget: 2–3 retries on the same failure with the error string injected into context.

What the code did at the snapshot:
- planner.py:126-161 did json.loads(raw) on free text. No with_structured_output(Plan), no retry on plan_invalid, no self-check. *(Structured path + bounded retry landed; legacy path remains behind a flag.)*
- executor.py:_run_tool did no retry and no error feedback. *(Retry funnel landed: validator re-prompt + transient backoff.)*
- executor.py:_run_react built a ToolMessage on failure and continued — primitive Reflexion, no retry cap or duplicate-tuple detection. *(Retry budget landed; cycle detection still open.)*

Gap: Missing structured-output enforcement on the planner (the biggest single fix), schema-validated tool args, per-step retry budget, and cycle detection. The "loop continues on error" pattern exists in spirit but lacks the safety rails Reflexion-style agents ship with.

3. Checkpoint GC — the doc is wrong about LangGraph's "own retention story" — **RESOLVED 2026-10-14** by `agent-stale-run-sweeper` (`adelete_thread` on retention deletes). Historical analysis kept below.

Industry practice (LangGraph PostgresSaver docs / production write-ups):
- LangGraph's checkpointer has no built-in retention. delete_thread() exists but does not cascade across checkpoints, checkpoint_blobs, checkpoint_writes — production systems sweep all three.
- The standard pattern is: when a metadata row is retention-deleted, also call await checkpointer.adelete_thread(run_id); OR a dedicated checkpoint-TTL cron targeting langgraph.checkpoints WHERE checkpoint_id < ....

What the code did at the snapshot:
- agent/retention.py:4 said "langGraph checkpoint tables are pruned via langGraph's own retention story and are NOT touched here." LangGraph ships no such story — the comment was wishful thinking. *(The current file states the opposite and calls `adelete_thread`.)*
- delete_thread / adelete_thread was never called anywhere. *(Now called by retention for runs and chat sessions, and by GDPR erasure.)*

Gap: retention.py needed to either (a) call await checkpointer.adelete_thread(run_id) for each row it deletes, or (b) own a parallel cron over langgraph.checkpoint_* directly with a separate TTL.

4. Run history + conversation history — **RESOLVED 2026-10-14** (history storage/UI, preferences, GDPR, background runs). Historical analysis kept below.

Industry practice (LangChain memory write-ups):
- Two-tier memory: short-term thread-scoped (LangGraph checkpoint) plus long-term user-scoped facts (vector store / key-value). CompositeBackend is the canonical LangChain v1 pattern.
- Conversation history is normally a separate queryable table ((user_id, thread_id, role, content, created_at)), distinct from the opaque checkpoint blob.
- Per-thread history loader seeds state["messages"] before the planner runs; trim/summarize middleware prevents context-window blow-up.
- A user-visible run list is table stakes — it's how users discover failed runs and resume.

What the code did at the snapshot:
- RunMetadata.list_runs_for_user existed but no HTTP endpoint called it. There was no /agent/runs or /chat/history.
- Conversation history was whatever the client sent in chat_req.messages. No server-side history table, no per-user prior-turn loader, no trim/summarize middleware.
- The LangGraph blob held the messages, but it was opaque.

Gap at the snapshot: A chat_messages table (append-only, user-scoped), an /agent/runs endpoint, and a per-thread history loader that runs before the planner. Cross-thread long-term memory (LangChain StoreBackend pattern) is the next tier beyond MVP. *(chat_sessions/chat_messages, the sessions API + sidebar, preference memory, and GDPR landed; `/agent/runs` and cross-session summary/recall remain open.)*

---

Summary table (historical statuses updated where the gap closed)

Gap (doc §11): Stale-run sweeper
Today's posture: Heartbeat + reaper (landed)
Industry baseline: Heartbeat/lease + timeout reaper
Gap severity: High (data hygiene)
Status: RESOLVED 2026-10-14 (agent-stale-run-sweeper)
────────────────────────────────────────
Gap (doc §11): Planner self-check
Today's posture: Structured output + bounded retry (landed)
Industry baseline: with_structured_output(Plan) + retry-on-parse-error
Gap severity: High (planner is the most failure-prone node)
Status: RESOLVED 2026-10-13 (agent-planner-structured)
────────────────────────────────────────
Gap (doc §11): Tool retry on malformed args
Today's posture: Validator re-prompt + transient backoff (landed)
Industry baseline: Structured tool calling + schema validation + retry budget +
cycle detection
Gap severity: Medium (ReAct has it, plain tool step doesn't)
Status: RESOLVED 2026-10-13 (agent-tool-dispatch-retry; cycle detection still
open)
────────────────────────────────────────
Gap (doc §11): Checkpoint GC
Today's posture: `adelete_thread` on run/chat retention + GDPR erase (landed)
Industry baseline: adelete_thread on metadata delete, or parallel TTL cron
Gap severity: Medium (storage growth, not correctness)
Status: RESOLVED 2026-10-14 (agent-stale-run-sweeper; extended by chat history/GDPR)
────────────────────────────────────────
Gap (doc §11): Run-history UX
Today's posture: Session sidebar + replay landed; `list_runs_for_user` still has
no `/agent/runs` endpoint
Industry baseline: /agent/runs endpoint + paginated list + click-to-resume
Gap severity: Low for the sidebar; run list still missing
Status: PARTIALLY RESOLVED 2026-10-14
────────────────────────────────────────
Gap (doc §11): Conversation history
Today's posture: `agent.chat_sessions` / `chat_messages` + sessions API + replay;
no cross-session summary or top-K recall
Industry baseline: Per-user chat_messages table + per-thread loader +
trim/summarize
Gap severity: Low for storage; memory recall still open
Status: RESOLVED for storage/replay 2026-10-14; recall remains open

The highest-leverage fixes, in order: (1) planner with_structured_output + retry; (2) stale-run sweeper with last_heartbeat_at; (3) hook adelete_thread into the existing retention cron; (4) _run_tool retry budget. **All four landed** (agent-planner-structured, agent-stale-run-sweeper, agent-tool-dispatch-retry) — this ranking is kept as the historical record.

Sources:
- Running LangGraph's Postgres Checkpointer in Production (2026)
- LangGraph Checkpointing: Persistence and Time Travel
- Never Lose AI Memory in Production: PostgresSaver for LangGraph
- The ReAct Pattern Explained (in depth)
- Autonomous ReAct Loops: Failure Modes and Deterministic Guardrails
- Why ReAct agents get stuck in loops (and how to stop it)
- Building Effective Agents: Patterns That Survive Production
- Managing Memory and Conversation State in LangChain Python
- The Goldfish Problem: Long-Term Memory for AI Agents
- Persistent Memory & Context Management
