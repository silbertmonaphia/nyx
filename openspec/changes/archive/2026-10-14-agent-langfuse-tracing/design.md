# Design: agent-langfuse-tracing

## Context

See `proposal.md` — Why. Relevant current state (verified against the tree, 2026-10-14):

- All five LLM call sites build `langchain_openai.ChatOpenAI` ad hoc and invoke via non-streaming
  `ainvoke`: planner structured (`agent/planner.py:155`), planner legacy (`planner.py:382`),
  executor `llm` (`executor.py:235`), executor `react` loop (`executor.py:271`), tool re-prompt
  (`executor.py:685`). None read `usage_metadata` — token usage is estimated
  (`DEFAULT_ESTIMATED_TOKENS`, `executor.py:59`).
- The production invocation seam is `run_agent_stream` (`graph.py:141`); its config is built in
  `main.py:494-496` (`{"configurable": {"thread_id": run_id}}`) and threaded into `astream`
  (`graph.py:168-170`). `user_id` is in scope at `main.py:472`.
- `setup_observability` installs a **global** OTel `TracerProvider` at FastAPI lifespan
  (`observability.py:87`, `main.py:57`) exporting to Jaeger; Prometheus counters live beside it.
- Tools dispatch via raw MCP JSON-RPC (`tools_client`), **not** LangChain tools — a LangChain
  callback handler cannot see them.
- The Langfuse Python SDK (v4) is OTel-based: `LangfuseResourceManager` attaches its
  `LangfuseSpanProcessor` to the **global** provider when one already exists (upstream
  "Using Langfuse with an Existing OpenTelemetry Setup" FAQ\*), which is exactly our situation.
  The FAQ's documented isolation path is passing a dedicated `tracer_provider` to the client.
- The e2e house pattern drives the SPA chat UI via Playwright with a live dev stack as a
  precondition (`frontend/tests/e2e/chat-*.spec.ts`); `agent-planner-structured-rollout/probe/`
  already contains a stub OpenAI-compatible provider used for deterministic probes.

\* Fetched during planning (untrusted third-party doc, advisory only — the SDK/provider behavior is
re-verified against the pinned version at implementation time).

## Goals / Non-Goals

**Goals:**

- One stop for "what did the planner/executor actually send and receive, per run, per user".
- Zero impact on the existing Jaeger/OTel pipeline, Prometheus counters, SSE wire, and run
  outcomes. Enabling tracing must be invisible to a user of the chat UI.
- Deterministic local stack: `make up-langfuse` → keys already match the agent's config.

**Non-Goals:**

- No real token usage into `DoneEvent` / SSE (the `done.usage` zeros stay; separate slice).
- No Go→Python traceparent propagation, no Go-side or frontend-side LLM tracing.
- No prompt management / evals wiring (AgentStack.zh.md queued items 2 and 5 build on this later).
- No production topology work (HA, managed ClickHouse/S3, retention policy) — rollout sibling.

## Decisions

### D1 — Self-hosted Langfuse v4 as an opt-in compose profile

`langfuse-web` + `langfuse-worker` + dedicated `langfuse-db` (postgres:17), `langfuse-redis`,
`langfuse-clickhouse` (25.12), `langfuse-minio` under `profiles: ["langfuse"]` in the root
`docker-compose.yml`. Services are **renamed** from upstream (`langfuse-db` vs the existing `db`,
`langfuse-redis` vs the existing `redis`) — upstream's `postgres`/`redis` service names would
collide with this project's. Image tags are pinned; the adapted block carries a comment citing the
upstream compose URL + commit for future re-diffs.

Port policy: publish **only** `127.0.0.1:3000` (UI + ingestion API). All stateful component ports
are not published at all (upstream publishes them loopback for debugging; we drop them —
deny-by-default). `langfuse-web` additionally joins the `internal` network so the compose `agent`
service can ingest; the stateful services stay off `internal`.

First boot uses `LANGFUSE_INIT_ORG_ID/PROJECT_ID/ORG_NAME/PROJECT_NAME/PUBLIC_KEY/SECRET_KEY/
USER_*` so the instance always materializes the same dev key pair — the agent env in
`backend/agent_py/.env.example` and the compose `agent` service reference those values, making the
dev loop click-free and idempotent. `TELEMETRY_ENABLED=false` (upstream default phones home).

Makefile: `up-langfuse` / `down-langfuse` mirroring `up` / `down` semantics, plus `make status`
lines. Alternatives rejected: Langfuse Cloud (prompt/user content leaves the machine); joining the
default `make up` set (six extra containers and ~4–8 GB RAM on every boot, WSL2 — vllm-profile
precedent says opt-in).

### D2 — Langfuse gets its own OTel provider; the global one stays untouched

The client is constructed with an explicitly created, **never-globally-registered**
`TracerProvider` (`Langfuse(..., tracer_provider=...)`). Rationale: our global provider is already
installed by `setup_observability` before any tracing code runs; per upstream behavior the SDK
would attach its span processor to that global provider, mixing `langfuse-sdk`-scope spans into
the Jaeger export and making behavior order-dependent. Isolation keeps Jaeger's pipeline
byte-identical and Langfuse's tree self-contained (all Langfuse spans — handler + our observation
helpers — are created through the client, so parent/child relationships live inside the one
provider; no cross-provider parenting is needed because we explicitly do NOT link to Jaeger spans).
Alternatives rejected: shared provider (Option A in the upstream FAQ — single tree, but perturbs
the existing export path and requires processor filtering); OTLP dual-export of existing spans to
`/api/public/otel` (requires hand-mapping `langfuse.*` attributes, much more code for less data).

### D3 — One handler on the existing graph-config seam; identity via invocation metadata

`main.py` builds `graph_config` — the Langfuse `CallbackHandler` is appended to
`config["callbacks"]` and `config["metadata"]` gains `langfuse_session_id=str(run_id)` and
`langfuse_user_id=str(user_id)`. LangGraph propagates the config into node runnables, so all five
`ainvoke` sites are covered without touching them. **Fallback** (if a site escapes contextvar
propagation, e.g. the `bind_tools` react loop): pass `config={"callbacks": [handler]}` explicitly
at those `ainvoke` calls. The e2e asserts generations exist for planner and executor paths, which
catches a miss. Alternative rejected: threading the handler through `new_state`/planner/executor
signatures — invasive for zero benefit.

### D4 — Steps and tool calls get explicit observation helpers, mirroring existing OTel sites

A module-level helper (no-op context manager when disabled) wraps the places that already emit
`agent.step.*` OTel spans (`planner.py:100`, `executor.py:104`, `executor.py:297`) and the MCP
dispatch site, creating Langfuse spans named after the step kind with step id, name, status,
tool args, result status, and measured duration. Tools are not LangChain tools, so this is the only
way to capture them. Wrapping at the existing span sites (rather than new seams) keeps the Jaeger
story and the Langfuse story structurally identical.

### D5 — Settings are house-style `PYTHON_AGENT_LANGFUSE_*`; client is constructed from settings

`config.py` gains `langfuse_enabled` (False), `langfuse_host` (host-dev default
`http://localhost:3000` — the `mcp_url`/`otel_endpoint` precedent: host-run `make dev-agent`
must resolve it, and compose hostnames don't on the host; the compose `agent` service overrides
to the in-network `http://langfuse-web:3000`), `langfuse_public_key` (""), `langfuse_secret_key`
(SecretStr), plus a `model_validator` refusal when enabled without keys. The client is constructed explicitly from
these values rather than letting the SDK read raw `LANGFUSE_*` env — one config surface, one
refusal path, and secrets stay in the settings object. (The SDK is a process-wide singleton in
v4; `CallbackHandler()` picks up the explicitly constructed client — verified with the pinned
version in task 3.1.)

### D6 — Masking and best-effort delivery at our boundary

Every payload we construct (tool args/results, step attributes) passes a scrub function replacing
bearer/JWT/API-key-shaped substrings with a redaction marker; the bearer token is passed as an
HTTP header on MCP dispatch and is never placed into prompts or observation attributes. Whether
the pinned SDK exposes a client-level mask hook to also cover handler-captured payloads is checked
at implementation (task 3.2); if absent, the residual is documented (prompts carry no credentials
by construction). All Langfuse calls (handler attachment, observations, flush) are wrapped so an
exception logs a warn and returns — the run path never sees a tracing error. Flush happens on
shutdown via the existing `shutdown_observability` path, never on the SSE critical path.

### D7 — E2E: Playwright spec driving the chat UI, asserting through Langfuse's public API

`frontend/tests/e2e/chat-langfuse-tracing.spec.ts` follows the house pattern (`chat-rag.spec.ts`):
register a fresh user → open chat → send a message → assert the reply renders (the run happened) →
then poll `GET /api/public/traces?sessionId=…` (basic auth with the dev key pair) with a bounded
timeout (ingestion is async, worker → ClickHouse) and assert the run tree: session id = run_id,
user id matches, planner + executor generations with non-empty input/output and provider-reported
usage, and the tool/step spans for a tool-step run. Assertions run against Langfuse's own read API —
the consumer-side parse the wire-contract rule asks for. Preconditions (dev stack up incl.
`make up-langfuse`, agent with the flag on, reachable LLM) are documented in the spec header like
the existing specs; CI posture follows the existing suite (frontend server managed, rest as
preconditions).

## Risks / Trade-offs

- [SDK/provider interaction differs from the FAQ's description in the pinned version] → D2 isolates
  regardless; task 3.1 verifies with evidence: Jaeger trace count/export unchanged, Langfuse tree
  renders, and the disabled-flag path constructs nothing. Version is pinned in `pyproject.toml`.
- [Config-callback propagation misses a call site] → D3 fallback (explicit `config` at `ainvoke`);
  e2e + unit coverage of the planner and executor paths.
- [Ingestion lag makes the e2e flaky] → bounded poll loop (retry the Langfuse read API until the
  trace appears, then assert shape); unique user/session per test run like the existing specs.
- [Six new containers strain WSL2] → profile opt-in only; RAM cost documented in the README +
  `make status` visibility; `down-langfuse` frees them.
- [Upstream compose drift on version bumps] → pinned tags + cited upstream commit; the re-diff
  surface is one commented block.
- [Prompts (user-derived content) now persist locally] → loopback-only ports, masking (D6), and the
  retention/backup policy is an explicit rollout gate in `agent-langfuse-tracing-rollout`.
- [Pre-existing drift observed, deliberately out of scope: production `run_agent_stream` emits no
  `agent.run` OTel span (spec `agent-observability` says every run SHALL).] → recorded here; the
  Langfuse run tree is not affected (it is rooted by the handler's trace, not by `agent.run`).
  Fixing it is a separate slice.

## Migration Plan

Additive and gated. Land code + compose + docs with the flag default-off; developers opt in per
env file. Rollback: unset/`false` the flag (agent reverts to today's behavior immediately) and
`make down-langfuse` (stateful volumes survive; re-up restores). No DB migrations, no wire
changes, no Go/frontend changes — nothing to sequence across services.

## Open Questions

None blocking. Deferred by design: prod topology and retention/backup policy (rollout sibling);
`done.usage` real token counts and Go-side trace linkage (follow-up slices, enabled by this one).
