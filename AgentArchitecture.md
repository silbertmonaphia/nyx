# Nyx Python Agent — Architecture Notes

Learning doc for `backend/agent_py/`. Reflects the working tree as of 2026-10-14,
after `agent-run-state`, `agent-planner-structured`, `agent-tool-dispatch-retry`,
and `agent-stale-run-sweeper` landed. The "What's not done yet" section below is a
historical gap analysis (2026-10-12) — resolved gaps are annotated in place.

---

## 1. Where it sits

```
┌────────┐  POST /api/chat   ┌──────────────┐   POST /agent/run    ┌──────────────┐
│  SPA   │ ────────────────▶ │  Go backend  │ ──────────────────▶  │  agent_py    │
│ React  │  (Bearer JWT)     │  (JWT verify)│  X-Nyx-User-Id +     │  FastAPI     │
│        │ ◀──────────────── │  agent_proxy │  Authorization pass  │              │
└────────┘   SSE bytes,      └──────────────┘ ◀──────────────────└──────┬───────┘
             unchanged                              SSE bytes           │
                                                                        │
              ┌─────────────────────────────────────────────────────────┤
              ▼                        ▼                    ▼           ▼
        Go /mcp (tools)    Go /internal/rag/retrieve    Postgres      Redis
        JSON-RPC 2.0       GET ?query= (JWT-scoped)   langgraph.*    budget
        JWT re-verified    JWT re-verified            agent.*        counter
```

- The Python service **never verifies JWTs**. Go verifies at the SPA edge and
  forwards the verified id in `X-Nyx-User-Id`. The bearer token is a passthrough
  that Go's `/mcp` and `/internal/rag/retrieve` re-verify themselves.
- Trust boundary at this hop is the **network** (compose bridge / k8s
  NetworkPolicy). Port 9000 is not published to the host; only 9001 (metrics).
- Defense in depth on top of that: per-IP rate limit on `/agent/run`
  (`ratelimit.py`), and every metadata SQL read is scoped by `user_id`.

## 2. Request lifecycle

1. `POST /agent/run` (`main.py:325`) — no auth, but **400 if `X-Nyx-User-Id`
   missing or non-integer** (prevents silent `user_id=0` budget collisions).
2. Body validated as `ChatRequest` (`types.py:64`): `{messages, agent?, model?,
   run_id?}`. `run_id` is the resume hook.
3. `_stream_agent_run` (`main.py:387`) is the generator FastAPI wraps in a
   `StreamingResponse` (`text/event-stream`).
4. Run id resolution (`main.py:418-437`):
   - client echoed `run_id` + row exists + owned by user + status not terminal
     → **resume**;
   - otherwise → `RunMetadata.create_run(user_id)` mints a fresh uuid4 (or a
     local uuid4 if persistence is off).
   Since the sweeper reaps dead rows to terminal, "not terminal" now means
   "live, or crashed but still inside the resume window" (§6).
5. Frames emitted **before** the graph even starts: `run_id`, then
   `note{kind:"resumed"}` or `note{kind:"fresh_run"}`.
6. Liveness heartbeat starts (`main.py:506`): a per-run task
   (`_heartbeat_loop`, `main.py:241`) touches `agent.runs.last_heartbeat_at`
   every `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (30 s, immediate first
   touch). Touch failures log a warning and are otherwise inert.
7. Graph streams; each new event from `events_so_far` is SSE-encoded immediately.
8. `finally`: cancel the heartbeat **before** the terminal write (`main.py:537`),
   then mark `completed` (with total tokens; clears `error_code`) or `failed`
   (with error code) in `agent.runs`. `agent_unavailable` deliberately leaves
   the row `running` — the stale-run sweeper reaps it after
   `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` of heartbeat silence
   (`agent-stale-run-sweeper`).
9. `data: [DONE]` sentinel — the frontend parser (`chatService.ts`) uses this to
   yield its own terminator; the legacy `event: terminator` frame is still
   emitted for old clients.

## 3. The graph

`agent/graph.py:80` — a LangGraph `StateGraph`:

```
START → planner → executor → END
```

- **planner** (`planner.py:91`) — one `langchain-openai` call on the structured
  path (default). The client binds an explicit OpenAI `response_format`
  json_schema (`strict: false`) and validates the content JSON with Pydantic
  (`_build_structured_client`; deliberately not `with_structured_output` — its
  docstring documents the strict-mode `args:{}` trap). Failures
  (`ValidationError` / `OutputParserException`) re-prompt with the validator's
  error under a `PREVIOUS_ATTEMPT_FAILED` block, bounded by
  `planner_retry_max` (default 1); persistent failure → typed
  `error{code: "plan_invalid"}`. `plan_too_large` (> `plan_max_steps` = 10) and
  duplicate step ids are typed errors too. A legacy free-text path
  (fence-strip + `json.loads`) stays reachable via
  `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false` for vLLM parsers that don't
  honour `response_format`; both paths share `_enforce_plan_shape`, so the
  wire `plan` frame is identical.
- **executor** (`executor.py:68`) — sequential over `plan.steps`. Four step
  kinds (`StepKind` in `types.py`):

  | kind  | handler   | mechanism                                                  |
  |-------|-----------|------------------------------------------------------------|
  | rag   | `_run_rag` (`:181`)| GET Go RAG bridge; unavailable → empty context (not fatal) |
  | tool  | `_run_tool` (`:195`)| MCP `tools/call` JSON-RPC through `_dispatch_tool_with_retries` (`:512`) — per-(step, tool) retry budget |
  | llm   | `_run_llm` (`:227`)| single LLM completion, result streamed as one `delta`      |
  | react | `_run_react` (`:260`)| nested model→tool loop, tools bound from MCP `tools/list`, capped by `step_react_max_iterations` (5); dispatches share the same retry path |

- **Budget guard** is consulted before *every* model call (`_run_llm`,
  `_run_react` per iteration). Exhaustion short-circuits the whole run with
  `error{code:"budget_exceeded"}` + `done` + `terminator`.
- Step failures are contained: a failed step appends `step_end{status:"failed"}`
  and the loop continues; when the plan diverged, the executor inserts
  `note{kind:"partial_failure", failed_steps:[...]}` before the final answer's
  deltas (post-loop `_maybe_emit_partial_failure`, called at `executor.py:173`;
  definition `:986`). Only budget errors abort the whole run
  (`_terminate_with_error`, `executor.py:489`) — the `react` iteration-cap
  fails its step and the run continues, but `react_iterations_exceeded` is
  still in `PLAN_LEVEL_ERROR_CODES`, so the note fires either way.
- **Checkpointer** (when Postgres is configured): `AsyncPostgresSaver` compiled
  into the graph. Every node transition is checkpointed under
  `thread_id = run_id`. Resume = `astream(None, config={"configurable":
  {"thread_id": run_id}})` — a non-None input would be treated as a state
  *update* and clobber checkpointed channels, so resume passes `None`
  (`graph.py:183-189`); LangGraph rehydrates, and the executor skips any step
  whose output already exists in `step_outputs` (`executor.py:96-103`).
  In tests, `checkpointer=None` → pure in-memory.

`run_agent` (`graph.py:91`) is the non-streaming `ainvoke` variant kept for
signature parity / tests; the live path is `run_agent_stream` (`graph.py:141`),
which uses `astream(stream_mode="values")` and yields only the **new** events
per snapshot (`last_len` watermark).

## 4. State

`AgentState` (`state.py:14`) is a `TypedDict(total=False)`:

- inputs: `user_id`, `run_id`, `request_id`, `messages`, `model`
- outputs: `plan`, `step_outputs` (per-step results keyed by step id),
  `cumulative_tokens`
- retry bookkeeping: `tool_retry_used` (per-step, per-tool consumption) and
  `tool_terminal_failures` (step → tools whose dispatch ended terminally; feeds
  the partial-failure banner without re-deriving exhaustion from frames)
- `events_so_far` — append-only event list; the SSE emitter's only source
- terminal markers: `terminal_status` (`ok` | `failed` | `budget_exceeded` |
  `budget_unavailable`), `terminal_reason`

Node contract: mutate the state, append events, return state. The emitter never
inspects intermediate state directly — it diffs `events_so_far` length.

## 5. SSE wire contract

`_sse_encode` (`main.py:578`): `event: <kind>\ndata: <flat json>\n\n` — the
payload is the typed event object directly, no `{kind, data}` wrapper. Kinds
(`types.py:192`):

| kind        | payload                                             |
|-------------|-----------------------------------------------------|
| `run_id`    | `{run_id}` (new in slice 2; always first frame)     |
| `note`      | `{text, provider?, kind?, last_step_id?, run_id?, failed_steps?}` — `kind` ∈ `resumed` \| `fresh_run` \| `partial_failure` |
| `plan`      | `{steps: [...]}`                                    |
| `step_start`| `{step_id, kind}`                                   |
| `tool_call` | `{step_id, call_id, name, args}`                    |
| `tool_result`| `{step_id, call_id, ok, result?, error?, retry_count?}` |
| `step_end`  | `{step_id, status, reason?}`                        |
| `delta`     | `{delta: "<text>"}`                                 |
| `done`      | `{usage: {prompt_tokens, completion_tokens, total_tokens}}` |
| `error`     | `{error, code?, request_id?}`                       |
| `terminator`| `{reason?}` — legacy; frontend keys off `[DONE]`    |

Compatibility: additive-only. Old clients ignore unknown kinds and never send
`run_id`, so they always get a fresh run. The frontend parser is the consumer of
record — per repo convention, wire-shape changes must round-trip through *its*
parser, not just producer-side tests.

## 6. Run persistence (landed 2026-10-13; sweeper additions landed 2026-10-14 in 0a9b8f5)

Three schemas, disjoint ownership:

| schema      | owner             | contents                                  |
|-------------|-------------------|-------------------------------------------|
| `public`    | Go (`golang-migrate`) | feeds, users, chat_*                  |
| `langgraph` | LangGraph framework | `checkpoint_*` tables (opaque state)    |
| `agent`     | Python (alembic)  | `runs` metadata + `alembic_version`       |

- DSN carries `options=-c search_path=langgraph,agent` (no `public` — a typo like
  `FROM feed` raises rather than silently touching Go's data).
  `config.py:263` auto-encodes the option so operators can paste a readable URL.
- **Two layers of persistence**: LangGraph's saver holds the *state*; our
  `agent.runs` table holds *queryable metadata* the checkpoint blob doesn't
  expose (`user_id`, `status`, `total_tokens`, `error_code`, timestamps +
  `runs_user_started_idx` index).
- `RunMetadata` (`run_metadata.py`) — asyncpg pool, 3-retry backoff on
  `PostgresConnectionError`, otherwise `RunMetadataUnavailable`. Metadata writes
  are **non-fatal**: failure emits `error{code:"run_metadata_unavailable"}` but
  the user's run continues. Terminal writes are authoritative over a premature
  reap: `mark_completed` clears `error_code`.
- **Heartbeat + sweeper** (landed 2026-10-14 in 0a9b8f5, `agent-stale-run-sweeper`): while a run
  streams, `_heartbeat_loop` (`main.py:241`) touches `last_heartbeat_at` every
  30 s; `StaleRunSweeper` (`sweeper.py`) runs every 300 s and flips rows with a
  heartbeat older than 900 s to `failed`/`error_code='stale_run'` (conditional
  on `status='running'`; OTel span `agent.run.sweep` + counter
  `agent_runs_swept_total`). `running` now means "heartbeating", and the
  timeout doubles as the crashed-run **resume window** — inside it an echoed
  `run_id` still resumes from checkpoint; after reaping the row is terminal and
  the same echo starts a fresh run (`note{kind:"fresh_run"}`).
  `PYTHON_AGENT_SWEEP_ENABLED=false` stops reaping (rollback); heartbeats keep
  writing.
- Migrations: alembic, `0001_langgraph_checkpoint.py` (programmatically invokes
  LangGraph's bundled migration) → `0002_runs_metadata.py` (creates `agent.runs`)
  → `0003_add_run_heartbeat.py` (`last_heartbeat_at timestamptz NOT NULL
  DEFAULT now()`). Dev: `migrations/_dev_upgrade_hook.py` runs on lifespan
  startup. Prod: dedicated `agent-migrate` one-shot; `agent` waits on
  `service_completed_successfully`; replicas never race on alembic.
- **Retention** (`retention.py`): asyncio cron (default hourly) hard-deletes
  terminal rows older than `retention_days` (30) — `DELETE ... RETURNING id` —
  then prunes each deleted run's langGraph checkpoint thread via
  `adelete_thread` (`agent-stale-run-sweeper`; langGraph ships no retention of
  its own, so this is the service's only checkpoint-GC path — the sweeper
  deliberately preserves checkpoints until retention collects them). A per-run
  GC failure is logged at WARN and the metadata delete stands. Uses Postgres
  `now()` (no clock-skew dependency). OTel span + Prometheus counter per
  iteration.
- **No DB configured** → loud warning at startup, runs not persisted, resume
  disabled. Fail-fast DSN check (`SELECT 1`) at boot so a bad URL dies in
  lifespan, not on first request.

## 7. Budget guard

`budget.py` — Redis counter `agent:budget:u={user_id}:h={UTC hour}`, TTL 90 min
(outlives the hour so a check straddling the boundary sees the same key).

- `INCRBY` first, then compare; over-cap increments are **rolled back**
  (`DECRBY`) so refused calls don't burn the user's budget.
- Fails **closed**: any Redis error → `decision="unavailable"` → run aborts
  with `budget_unavailable`. Never raises.
- Cap from unprefixed `AGENT_BUDGET_PER_USER_PER_HOUR` (default 200000) — a
  deliberate cross-runtime shared env var, historical contract with the Go
  agent.

## 8. Health & observability

- `/healthz`: Redis ping **and** LLM `GET {base}/models` — either failing → 503
  with a discriminating `status` field. 5xx from the provider counts as down;
  4xx (e.g. auth) does not.
- `/metrics`: Prometheus exporter on `:9001` (separate daemon thread).
  Counters: `agent_runs_total`, `agent_steps_total`, `agent_tool_calls_total`,
  `agent_tokens_total`, `agent_planner_parse_failures_total{retry}`,
  `agent_tool_retries_total`, `agent_run_retention_deleted_total`,
  `agent_runs_swept_total`.
- OTel: spans `agent.run`, `agent.step.{kind}`, `agent.step.plan`,
  `agent.step.react` (per iteration), `agent.planner.structured` /
  `agent.planner.retry` / `agent.planner.legacy`, `agent.budget`,
  `agent.run.retention`, `agent.run.sweep` → OTLP/HTTP → Jaeger, same collector
  as Go. Continuity with browser traces via W3C `traceparent` from the SPA.

## 9. Config (`config.py`)

- `pydantic-settings`, prefix `PYTHON_AGENT_`, `.env` auto-loaded. Process-wide
  lazy singleton (`get_settings`).
- Startup validators: refuse to boot if `llm_base_url` targets
  `api.openai.com` with no API key (vLLM/OpenRouter don't need one); DSN
  search_path normalization; retry caps non-negative; sweep intervals ≥ 1 and
  `stale_run_timeout_seconds` ≥ 2× `heartbeat_interval_seconds` (a smaller
  ratio could get a live run reaped) — each refusal names the offending env var.
- Notable knobs: `database_url` / `migrations_disabled` / `retention_*` /
  `database_pool_*`, rate limit `requests_per_second` (100) + `burst` (200),
  `plan_max_steps` (10), `step_react_max_iterations` (5), `planner_retry_max`
  (1) + `planner_structured_enabled` (true), `tool_retry_max` (2) +
  `tool_retry_backoff_base_ms` (250) / `tool_retry_backoff_ceiling_ms` (5000),
  `heartbeat_interval_seconds` (30), `stale_run_timeout_seconds` (900),
  `sweep_interval_seconds` (300), `sweep_enabled` (true).

## 10. File map

```
agent/
  main.py          FastAPI app, lifespan wiring, /agent/run SSE generator + heartbeat task
  graph.py         StateGraph build + streaming runner
  state.py         AgentState TypedDict + new_state()
  types.py         Pydantic wire models (plan, events, request) — the contract
  planner.py       plan_node: structured-output Plan + bounded retry (legacy free-text fallback)
  executor.py      executor_node + rag/tool/llm/react handlers, tool-dispatch retry budget
  budget.py        Redis hourly token budget (fail-closed)
  tools_client.py  MCP JSON-RPC client (tools/list, tools/call)
  rag_client.py    Go RAG bridge client (GET ?query=)
  ratelimit.py     per-IP limiter middleware (defense in depth)
  observability.py OTel setup/teardown + Prometheus counters
  run_metadata.py  agent.runs CRUD + asyncpg pool (heartbeat, sweep, retention SQL)
  retention.py     hourly hard-delete cron + checkpoint GC via adelete_thread
  sweeper.py       stale-run reaper cron (agent-stale-run-sweeper, landed 2026-10-14)
migrations/        alembic: 0001 langgraph checkpoint, 0002 runs metadata, 0003 run heartbeat
tests/             fakeredis + httpx.MockTransport; no live network needed
```

## 11. Known gaps / next slices

- ~~Stale-run sweeper: rows left `running` after `agent_unavailable` are never
  reaped yet (slice 4).~~ **Landed** in `agent-stale-run-sweeper` (heartbeat +
  reaper to `failed`/`stale_run`; the stale timeout is the resume window).
- ~~Fault tolerance: retry-on-malformed-tool-args, planner self-check (slice 4).~~
  **Landed**: `agent-planner-structured` (planner self-check + structured
  output) and `agent-tool-dispatch-retry` (tool-arg / transient retries +
  partial-failure banner).
- ~~LangGraph `delete_thread` cleanup not scheduled; orphan checkpoints may
  linger (accepted for now).~~ **Landed**: `adelete_thread` runs for every
  retention-deleted terminal row (`agent-stale-run-sweeper`).
- Run-history UX and DB-backed conversation history are later slices
  (`FUTURE_AGENT.md` §1.1 / §2.1).
- 2026-10-14 status: run-state, planner-structured, and tool-retry slices
  committed and archived; `agent-stale-run-sweeper` committed (`0a9b8f5`) and
  archived (canonical spec: `openspec/specs/agent-run-sweeper/`); rollout
  siblings remain (PM-owned).


# What's not done yet(gap to MVP agent)
Comparison against industry practice (cross-referenced with the actual code under backend/agent_py/):

1. Stuck-run sweeper — missing the industry baseline — **RESOLVED 2026-10-14** by `openspec/changes/agent-stale-run-sweeper` (heartbeat + timeout reaper; the stale timeout is also the resume window). Historical analysis kept below.

Industry baseline (workflow engines: Temporal/Celery; durable agents: Inngest, Restate): heartbeat/lease — worker periodically bumps last_heartbeat_at; a sweeper reaps rows where status='running' AND last_heartbeat_at < now() - timeout. Or the orchestrator holds a server-side lease that auto-expires.

What the code does (agent/main.py:451-457, agent/retention.py):
- agent_unavailable deliberately leaves the row as running. No last_heartbeat_at column exists in agent.runs.
- RetentionCron._run_once only deletes status IN ('completed','failed') — running rows are untouched. There is no per-row timeout, no lease, no heartbeat thread.
- Comment at main.py:457 explicitly defers this to "slice 4."

Gap: A crash between mark_failed/SSE-flush and Postgres write leaves the row running indefinitely; retention never reaps it; the user's only escape is to ignore the run_id. Industry calls this worker GC, not optional.

2. Fault tolerance — primitive self-correction only — **RESOLVED 2026-10-13** by `agent-planner-structured` + `agent-tool-dispatch-retry` (planner structured output + retry; tool-dispatch retry budget; partial-failure banner). Historical analysis kept below.

Industry baseline (ReAct/Reflexion write-ups):
- Use native structured tool calling (LangChain with_structured_output, OpenAI tools=, Claude tool-use). JSON-parse of free text is the single biggest source of malformed-args failures.
- Schema validation (Pydantic/Zod) before invoking the tool; on validation failure, return a normalized error string to the model.
- Loop detection on hashed (action, args, observation) tuples; force-reflection on repetition.
- Retry budget: 2–3 retries on the same failure with the error string injected into context.

What the code does:
- planner.py:126-161 does json.loads(raw) on free text. No with_structured_output(Plan), no retry on plan_invalid, no self-check. One bad parse → typed error → run terminates.
- executor.py:_run_tool (:138-176): on ToolError → emit failed tool_result → raise _step_runtime_error → step is marked failed, loop continues to next step. No retry, no error feedback to the model.
- executor.py:_run_react (:283-302): on tool failure, builds ToolMessage(content=repr(exc)) and continues the loop — that's a primitive form of "inject failure into context," equivalent to Reflexion's first stage, but with no retry cap, no duplicate-tuple detection, no critique pass.
- executor.py:_run_llm (:197-200): no retry; one exception → step fails.
- bind_tools is only used in _run_react (executor.py:235); _run_tool calls tools.call(name, args) directly with no schema validation.

Gap: Missing structured-output enforcement on the planner (the biggest single fix), schema-validated tool args, per-step retry budget, and cycle detection. The "loop continues on error" pattern exists in spirit but lacks the safety rails Reflexion/Reflexion-style agents ship with.

3. Checkpoint GC — the doc is wrong about LangGraph's "own retention story" — **RESOLVED 2026-10-14** by `openspec/changes/agent-stale-run-sweeper` (`adelete_thread` on retention deletes). Historical analysis kept below.

Industry practice (LangGraph PostgresSaver docs / production write-ups):
- LangGraph's checkpointer has no built-in retention. delete_thread() exists but does not cascade across checkpoints, checkpoint_blobs, checkpoint_writes — production systems sweep all three.
- The standard pattern is: when a metadata row is retention-deleted, also call await checkpointer.adelete_thread(run_id); OR a dedicated checkpoint-TTL cron targeting langgraph.checkpoints WHERE checkpoint_id < ....

What the code does:
- agent/retention.py:4 says "langGraph checkpoint tables are pruned via langGraph's own retention story and are NOT touched here." LangGraph ships no such story — the comment is wishful thinking.
- delete_thread / adelete_thread is never called anywhere. The langgraph.checkpoint_* tables grow unbounded; rows for running-status runs that never terminate accumulate forever.

Gap: retention.py needs to either (a) call await checkpointer.adelete_thread(run_id) for each row it deletes, or (b) own a parallel cron over langgraph.checkpoint_* directly with a separate TTL.

4. Run history + conversation history — only the metadata half exists (still open)

Industry practice (LangChain memory write-ups):
- Two-tier memory: short-term thread-scoped (LangGraph checkpoint) plus long-term user-scoped facts (vector store / key-value). CompositeBackend is the canonical LangChain v1 pattern.
- Conversation history is normally a separate queryable table ((user_id, thread_id, role, content, created_at)), distinct from the opaque checkpoint blob.
- Per-thread history loader seeds state["messages"] before the planner runs; trim/summarize middleware prevents context-window blow-up.
- A user-visible run list is table stakes — it's how users discover failed runs and resume.

What the code does:
- RunMetadata.list_runs_for_user exists (run_metadata.py:207) but no HTTP endpoint calls it. There is no /agent/runs or /chat/history.
- Conversation history is whatever the client sends in chat_req.messages (seeded into AgentState in _stream_agent_run). No server-side history table, no per-user prior-turn loader, no trim/summarize middleware.
- The LangGraph blob holds the messages, but it's opaque — you can't query "last 5 turns for user X."
- FUTURE_AGENT.md §1.1 / §2.1 explicitly defers this — meaning even the plan for history persistence isn't written.

Gap: A chat_messages table (append-only, user-scoped), an /agent/runs endpoint, and a per-thread history loader that runs before the planner. Cross-thread long-term memory (LangChain StoreBackend pattern) is the next tier beyond MVP.

---

Summary table

Gap (doc §11): Stale-run sweeper
Today's posture: None — running rows leak forever
Industry baseline: Heartbeat/lease + timeout reaper
Gap severity: High (data hygiene)
Status: RESOLVED 2026-10-14 (agent-stale-run-sweeper)
────────────────────────────────────────
Gap (doc §11): Planner self-check
Today's posture: JSON.parse of free text, one shot, no retry
Industry baseline: with_structured_output(Plan) + retry-on-parse-error
Gap severity: High (planner is the most failure-prone node)
Status: RESOLVED 2026-10-13 (agent-planner-structured)
────────────────────────────────────────
Gap (doc §11): Tool retry on malformed args
Today's posture: _run_react continues with error string; _run_tool skips step
Industry baseline: Structured tool calling + schema validation + retry budget +
cycle detection
Gap severity: Medium (ReAct has it, plain tool step doesn't)
Status: RESOLVED 2026-10-13 (agent-tool-dispatch-retry; cycle detection still
open)
────────────────────────────────────────
Gap (doc §11): Checkpoint GC
Today's posture: None — orphan langgraph.checkpoint_* accumulates
Industry baseline: adelete_thread on metadata delete, or parallel TTL cron
Gap severity: Medium (storage growth, not correctness)
Status: RESOLVED 2026-10-14 (agent-stale-run-sweeper — retention-owned GC)
────────────────────────────────────────
Gap (doc §11): Run-history UX
Today's posture: list_runs_for_user exists, no endpoint
Industry baseline: /agent/runs endpoint + paginated list + click-to-resume
Gap severity: Low (functional MVP without it)
────────────────────────────────────────
Gap (doc §11): Conversation history
Today's posture: Client-supplied messages only, no DB
Industry baseline: Per-user chat_messages table + per-thread loader +
trim/summarize
Gap severity: Low (functional MVP without it; needed for cross-session UX)

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