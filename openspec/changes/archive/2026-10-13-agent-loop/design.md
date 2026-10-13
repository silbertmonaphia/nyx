# Design

## Context

The current chat path is single-turn text completion with optional RAG context injection. The provider seam is text-in / text-out (`backend/internal/llm/provider.go:39-102`); the vLLM client actively drops tool-call deltas (`vllm/client.go:440-443`); the chat handler is a one-shot LLM call (`internal/chat/service.go:109-195`); the MCP server exposes five feed tools externally only and is never invoked from chat (`internal/mcp/` vs `internal/chat/` — confirmed by grep). RAG retrieval is pre-LLM prompt injection today, not a discrete step in any plan. OTel already covers `llm.chat` / `llm.router` / `rag.retrieve` / `mcp.dispatch.*`; per-provider failover (`llm.Router`) handles "provider down" but not "model emits illegal tool args". This change turns the chat path into a real Plan-and-Execute agent loop without disrupting the existing wire contract or the MCP externally-facing contract. The P0–P2 roadmap lives in `FUTURE_AGENT.md`; this design covers slice 1 (provider tools + tool registry + Plan-and-Execute loop + observability + per-user budget guard).

## Goals / Non-Goals

**Goals:**
- Provider seam carries tool calls identically for OpenAI and vLLM; existing single-turn callers (zero-tools) are byte-for-byte unaffected.
- Internal tool registry is the single source of truth, consumed by both the agent loop and the MCP server — no duplicated tool metadata.
- Chat becomes a Plan-and-Execute loop streaming per-step SSE frames alongside today's `delta` / `done` / `note` / `error` / `terminator` wire shape; old clients keep rendering the token stream, new clients render intermediate progress.
- Per-user token budget guard fails closed; agent is default-off (`AGENT_ENABLED=false`) so misconfiguration cannot cause runaway cost.
- OTel spans for `agent.run` / `agent.step.*` extend the existing trace surface; Prometheus counters ride alongside the existing `cache_operations_total` style.

**Non-Goals:**
- DB-backed run state / cross-session memory / conversation tables. `FUTURE_AGENT.md` §P1.1 + §P2.1 ship in follow-up changes.
- Task routing (intent → plan template). `FUTURE_AGENT.md` §P0.4 ships in a follow-up change after slice 1 has been exercised.
- Fault tolerance (retry on malformed tool args / planner self-check). `FUTURE_AGENT.md` §P1.3 ships in a follow-up change after slice 1 has failure data.
- Self-reflection (CRAG / Adaptive). Explicitly out of scope per `FUTURE_AGENT.md` §P2.2.
- JSON mode / `ResponseFormat`. Deferred per `FUTURE.md:86` — added when first caller needs structured output beyond tool calls.

## Decisions

### 1. Streaming `Delta` is a typed union, not a string

The `onDelta` callback carries a `Delta` value with a discriminator (`Content` / `ToolCall` / `Usage` / `Note`) and the relevant fields. Pre-change callers that pass `(string, error)` get a thin wrapper that auto-extracts content from `Delta{Content: …}` and forwards the rest as empty-string + error frames — so existing code paths remain `onDelta(text, nil)`-shaped. vLLM's wire-shape divergence from OpenAI is normalised inside the vLLM client so the caller never sees it.

**Alternatives considered:**
- *Keep `onDelta(string, error)` and encode tool calls as JSON inside the string*: rejected. Loses type safety, breaks downstream consumers that grep for content frames, and pushes vLLM's wire shape into the chat layer.
- *Two callbacks (`onContent` and `onToolCall`)*: rejected. Requires every existing caller to be touched even when the path is single-turn — violates the "zero-tools callers unchanged" goal.

### 2. Per-step SSE frames live in the same `/api/chat` stream

The new `plan` / `step_start` / `tool_call` / `tool_result` / `step_end` frames are emitted on the same SSE response as today's `delta` / `done` / `note` / `error` / `terminator`. The wire shape is purely additive: new frames carry new fields; old frames keep their shape. Old clients ignore the new frames; new clients ignore the `error` codes they don't recognise.

**Alternatives considered:**
- *New endpoint `/api/agent/run`*: rejected. Splits the chat surface, breaks the existing frontend's `chatService.streamMessage` shape, and forces a migration for a behaviour that should feel like an upgrade, not a parallel API.
- *WebSocket*: rejected. SSE is already the wire contract; the existing refresh-on-401 single-flight replay (`chatService.ts:60-276`) is built around SSE; switching transports is unjustified churn for an additive feature.

### 3. Tool registry lives in-process; tools are static at boot

`internal/tool/registry.go` holds a `sync.RWMutex`-protected `map[string]Tool` populated at boot from a builder closure. Tools don't change at runtime. The registry is the only place tool metadata (name, version, arg schema, dispatcher) lives; both the agent loop and the MCP server look up tools there.

**Alternatives considered:**
- *Redis-backed tool registry*: rejected. Tools are static Go values; round-tripping them through Redis adds latency and a failure mode without any runtime dynamism benefit. Re-evaluate if/when remote MCP servers become a thing.
- *Each domain holds its own tool definitions*: rejected. This is the current MCP anti-pattern — duplicate metadata, drift risk. The whole point of this slice is to centralise.

### 4. MCP handler becomes a thin shim over the registry

`internal/mcp/service.go:64-150` deletes its hand-rolled tool definitions and instead maps `tools/list` to the registry's snapshot and `tools/call` to the registry's dispatcher. Per-owner scoping and safe-error mapping move into the registry's dispatcher so both surfaces inherit them.

**Alternatives considered:**
- *Keep MCP's tool definitions, add a parallel agent-side registration*: rejected. Drift is the failure mode we're explicitly avoiding.
- *Embed the registry into MCP and re-export from chat*: rejected. The registry is the inner core; both consumers are clients of it, not the other way round.

### 5. RAG becomes a step kind, not a pre-LLM injection

The planner MAY emit a step of kind `rag` that runs `rag.Service.Retrieve` with the current user message (or a planner-supplied query) and renders the context block as the step's output. Subsequent steps see that output via the plan's step-output map. The pre-LLM context-injection in today's `chat.Service.Chat` is removed and replaced by `if !AGENT_ENABLED { … legacy path … }`.

**Alternatives considered:**
- *Keep RAG as a pre-LLM injection alongside the agent loop*: rejected. Two ways to fetch context invites drift and makes the planner's plan incomplete (the planner doesn't know what context was injected).
- *Run RAG inside the executor before every `llm` step*: rejected. Burns tokens on retrievals the planner might not need.

### 6. Planner JSON schema locks step identifiers in the plan

The planner emits JSON of shape:
```
{
  "steps": [
    {"id": "step_1", "kind": "rag", "query": "..."},
    {"id": "step_2", "kind": "tool", "tool": "list_feeds", "args": {"limit": 10}},
    {"id": "step_3", "kind": "llm", "prompt": "...", "uses": ["step_2"]},
    {"id": "step_4", "kind": "react", "tool": "get_feed", "args": {...}, "max_iterations": 3}
  ]
}
```
Identifiers are mandatory and unique within the plan; the executor correlates SSE frames via identifier. New kinds can be added without breaking older plans.

**Alternatives considered:**
- *Position-dependent step ordering*: rejected. Tool calls are async; frames arrive interleaved; clients need stable IDs to render progress correctly.
- *Free-form planner output parsed by an LLM judge downstream*: rejected. Latency, cost, and failure mode — JSON-schema-as-code is what the provider's structured output mode is designed for; if a step shape ever becomes ambiguous, the planner JSON-schema validates at parse time.

### 7. Per-user budget counter is a Redis hourly TTL key

`agent:budget:u={user_id}:h={utc_hour}` is an `INCRBY`-style counter. TTL set to 90 minutes on first increment; the counter resets at the hour boundary. Before each `provider.Chat` inside the loop, the agent does `GET tokens_remaining = cap − counter_value` and either proceeds or terminates with `budget_exceeded`. On Redis unavailability, the agent terminates with `budget_unavailable` and the run trace records the decision.

**Alternatives considered:**
- *Per-request counter in the request context*: rejected. A second request from the same user in the same hour has its own counter — cap not enforced.
- *Postgres counter*: rejected. Redis is already in the stack (cache-aside for `GET /api/feeds`) and the budget check is in the hot path of every agent call; PostgreSQL round-trip per call is unjustified.

### 8. Provider failover stays off for tool-bearing requests until vLLM parity is verified

`LLM_FALLBACK_TOOLS_ENABLED` defaults to `false`. The router refuses a tool-bearing request on a provider whose tool-call integration test is currently red in CI (per spec `llm-structured-output` / `Provider failover honours tool calls`). Both providers' tool-call integration tests must be green in CI before the flag flips in any environment.

**Alternatives considered:**
- *Always failover regardless of parity*: rejected. vLLM's tool-call wire shape is known to diverge from OpenAI's; failing open here is the vLLM-drops-tool-calls problem we're explicitly fixing.
- *Per-tool-call parity tracking*: rejected. Tool-call support is a property of the client, not a per-tool decision.

### 9. Nested ReAct inside a `react` step is opt-in via step kind, default plan-driven

The planner emits `kind: "react"` only when it explicitly wants a model-tool-model sub-loop within a step. The default step kinds are `rag` / `tool` / `llm` — no nested loop. The executor's ReAct caps iterations at `AGENT_STEP_REACT_MAX_ITERATIONS` (default 5) and emits `step_failed {reason:"react_max_iterations"}` on cap, per spec `agent-loop` / `Step execution is bounded`.

**Alternatives considered:**
- *Always run a ReAct loop inside every step*: rejected. Adds token cost on every step, with no benefit for steps that are pure RAG or single tool calls.
- *Pick ReAct vs Plan-and-Execute per request*: rejected. Two loops = two implementations = two failure modes. Nested-ReAct-inside-a-step gives 80% of the flexibility for 20% of the surface.

### 10. Run state is in-memory for slice 1

The agent run struct (`Run`, `Step`, intermediate results) lives in process memory, flowed via `context.Context` with typed values in `internal/reqctx/`. Backend restart drops the run — same as today's single-turn chat. Persistence ships in `FUTURE_AGENT.md` §P1.1.

**Alternatives considered:**
- *Persist runs in slice 1*: rejected. The plan shape and intermediate-result format aren't stable yet; persisting them locks us into a schema we may want to revise after slice 1 has been exercised.

## Risks / Trade-offs

- **Latency multiplies per step** → SSE frames stream per-step, not per-run. Frontend must render intermediate steps. Hard verification gate per `FUTURE_AGENT.md` / Risks: `step_start` emitted within `AGENT_FIRST_STEP_LATENCY_MS` (default 1500ms) of request start. End-to-end test asserts this.
- **Cost multiplies per step** → Per-user budget guard is load-bearing; planner can use a cheaper model (`LLM_PLANNER_MODEL`); executor step caps bound worst case. Hard verification gate: `agent_budget_exhausted_total` increments under load test; `agent_tokens_total{kind="planner"}` stays <30% of `agent_tokens_total{kind="executor"}`.
- **vLLM tool-call wire divergence from OpenAI** → Normalisation layer inside the vLLM client maps vLLM's wire shape to the same `Delta` shape the OpenAI client produces. Hard verification gate: canned-SSE integration test on the vLLM client asserts no tool-call delta is dropped. Failover to vLLM for tool-bearing requests stays off until CI is green.
- **Tool safety is unchanged** → the registry's per-owner scoping + safe-error mapping is the same pattern MCP uses; this is reuse, not new risk. Hard verification gate: `internal/tool/registry_test.go` includes a concurrency test asserting scoping is enforced under panic.
- **MCP wire contract is preserved** → MCP becomes a thin shim over the registry; externally observable tool names, arg shapes, and error semantics do not change. Hard verification gate: existing MCP integration tests pass against the new registry-backed handler without modification.
- **Planner JSON-schema drift** → If the planner emits a step kind or arg shape the executor doesn't recognise, the run terminates with `plan_invalid` (a single retry via the retry-on-malformed-output path in `FUTURE_AGENT.md` §P1.3, when that lands). For slice 1, one parse-error → run terminates; no silent fallback.
- **Budget counter single-point-of-failure** → If Redis is down, every run terminates with `budget_unavailable`. Fails closed. Documented in `FUTURE_AGENT.md` / Risks.

## Migration Plan

1. **Land `llm-structured-output` first, no consumers**. Provider seam extensions and the canned-SSE integration tests for both providers. Existing callers untouched. PR title: `feat(llm): add tool-call support to provider seam`.
2. **Land `agent-tool-registry`**. Move the five MCP tools into the registry; delete MCP's hand-rolled tool definitions; update MCP tests to assert the registry-backed shim. PR title: `refactor(mcp): consume internal tool registry`.
3. **Land `agent-loop` + `agent-observability` together** behind `AGENT_ENABLED=false`. Add the agent package, planner, executor, SSE frame types, OTel spans, Prometheus counters, per-user budget guard. Existing chat path is the `AGENT_ENABLED=false` branch. PR title: `feat(agent): Plan-and-Execute loop with per-step SSE + per-user budget`.
4. **Frontend parses new SSE kinds** without rendering them yet (`frontend/src/features/chat/services/chatService.ts` adds parsers, `ChatPanel.tsx` is unchanged). PR title: `feat(chat): parse agent step SSE frames`.
5. **Flip `AGENT_ENABLED=true` in `backend/.env.example` for local dev only** (not production). Frontend renders intermediate steps. Manual end-to-end smoke test against `list_feeds` plan.
6. **Production flip** via viper config in the deploy environment, gated on a `FUTURE_AGENT.md` checkbox. `LLM_FALLBACK_TOOLS_ENABLED` flips only after both providers' tool-call integration tests are green in CI for ≥1 week.
7. **Rollback**: setting `AGENT_ENABLED=false` restores the single-turn chat path. The provider seam extensions are non-breaking; the registry migration is non-breaking; the agent package is dead code when disabled.

## Open Questions

- *Can a tool be marked "agent-only" or "external-only"?* — defer until the first such tool exists. Today every tool is exposed to both surfaces by default, matching the slice-1 goal of no duplicate metadata.
- *Should the planner JSON schema allow a free-form `llm` step with a custom prompt template, or always require a tool / rag query argument?* — current answer: allow `prompt` as an arg on `llm` steps, but most steps will be tool calls in practice. Lock the schema in a follow-up change after the first planner prompt templates stabilise.
- *Per-user budget counter as a hard cap, or a soft cap with a "warning" SSE event before exhaustion?* — current answer: hard cap only. Soft caps require either an extra model call to estimate (more cost) or a heuristic that drifts. Re-evaluate after slice 1 has user data on hit rate.