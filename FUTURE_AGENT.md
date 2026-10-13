# Nyx Agent Capability: Architectural Evolution

The roadmap for turning Nyx from "RAG-augmented single-turn chat" into a real Plan-and-Execute agent. For the high-level "done / next" checklist and other roadmap tracks, see `FUTURE.md`.

Scope: every change that adds an agent loop, tool dispatch, run/step state, planning, or self-reflection to the backend. **The provider LLM client (`internal/llm/`) and the chat SSE handler (`internal/chat/`) change under this roadmap only**. The MCP server (`internal/mcp/`) is consumed by this roadmap as a tool surface — its externally-facing contract is preserved. The frontend chat surface (`frontend/src/features/chat/`) gains intermediate-step rendering but its token-stream contract is additive.

Why a new file: agent capability is significant enough that scattering it across `FUTURE_BACKEND.md` dilutes both. Future cross-cutting concerns (e.g. agent-aware rate limits, MCP ↔ agent unification) live here. `FUTURE_BACKEND.md` covers non-agent backend work; `FUTURE_FRONTEND.md` covers non-chat frontend work; the chat surface itself spans both — its agent-related items move here.

---

## Current state (as of last verification)

- **`internal/llm/provider.go`** — single-turn text completion only. `ChatRequest` has `Model / Messages / MaxTokens / Temperature / OnNote`. No `Tools`, no `ToolChoice`, no `ResponseFormat`. `Message` is `(Role, Content)` only — no `ToolCallID` / `Name` / `ToolCalls`; `RoleTool` does not exist.
- **`internal/llm/vllm/client.go:440-443`** — explicitly drops tool-call deltas ("intentionally ignored").
- **`internal/chat/service.go:109-195`** — one-shot: prepend system prompt → optional RAG retrieval → single `provider.Chat` → stream. No loop, no tool dispatch, no reflection.
- **`internal/mcp/`** — five feed tools exposed *externally* only. `internal/chat/` and `internal/llm/` grep-verified never call `internal/mcp/`.
- **Storage** — pgvector + per-user HNSW + lazy backfill work for RAG; no `chat_sessions` / `chat_messages` / `conversations` tables exist.
- **Observability** — OTel covers `llm.chat` / `llm.router` / `rag.retrieve` / `mcp.dispatch.*`; needs new `agent.run` / `agent.step.*` spans.
- **Provider failover** — `llm.Router` handles "provider down". Does not solve "model emits malformed tool args".
- **Frontend** — `ChatPanel.tsx` renders tokens only; `chatStore` is ephemeral (XSS surface concern, stays that way for the streaming layer; per-user history lands in DB later).

**Verdict**: today is retrieval-augmented prompt stuffing, not an agent. The first OpenSpec change (`openspec/changes/agent-loop/`) shipped the Go infra (provider seam + tool registry + MCP shim + chat SSE translator). Slice 1's agent orchestration is being implemented in **Python LangGraph** in the new OpenSpec change `agent-langgraph` (`openspec/changes/agent-langgraph/`) — the Go orchestrator was removed; RAG stays in Go unchanged.

---

## P0 — Core skeleton

### 0.1 Provider seam: tool calls and structured output

- [ ] **`llm.Provider.Chat` carries `Tools` / `ToolChoice`** (`internal/llm/provider.go`). `ChatRequest` gains `Tools []Tool` and `ToolChoice any` (string enum or struct). `Message` gains `ToolCallID`, `Name`, `ToolCalls []ToolCall`. New `RoleTool` role.
- [ ] **Typed streaming `Delta` union** — `onDelta` callback carries `Delta { Kind: content|tool_call|usage|note, … }` instead of plain strings. Old callers treat `Delta{content}` as today's `(string, nil)`.
- [ ] **OpenAI client implements tool call streaming** (`internal/llm/openai/client.go`). Maps OpenAI `tool_calls` deltas into the union.
- [ ] **vLLM client implements tool call streaming** (`internal/llm/vllm/client.go`). Rewrite the SSE parser — currently drops tool-call deltas at `vllm/client.go:440-443`.
- [ ] **Both providers get tool-call integration tests** (`backend/internal/llm/openai/client_test.go` + `vllm/client_test.go`). Use `httptest.Server` replaying canned SSE — no live API needed. **Risk note: vLLM tool_calls wire format has historically diverged from OpenAI's; both providers must be exercised even when OpenAI passes.**
- [ ] **`llm.Router` (OpenAI ⇄ vLLM failover) preserved** — tool-call support lands symmetrically on both providers before failover is enabled for tool-bearing requests (`LLM_FALLBACK_TOOLS_ENABLED` default `false`).

**Why this is P0**: nothing else on the roadmap works without `Provider` carrying tool calls. Existing single-turn callers (the RAG path) keep working — zero-tools requests are no-ops in the new code.

**Out of scope for 0.1**: `ResponseFormat` / JSON-mode. Add when first caller needs structured output beyond tool calls (FUTURE.md:86).

### 0.2 Internal tool registry

- [x] **NEW `internal/tool/`** — tool registry owning registration, per-owner scoping (`reqctx.UserIDFromContext`), input validation (lightweight struct-tag validator in `internal/tool/validate.go` — no new dependencies), and safe-error mapping (`api.ClassifyAndLog` with a tool-specific `safeDetail`). Dispatch recovers panics, returns `ErrPanic`, and never copies underlying text to the caller.
- [x] **MCP's five feed tools migrate into the registry** (`internal/tool/feeds.go`; `internal/mcp/service.go` is now a thin shim that calls `registry.Dispatch`). Per-owner scoping and error mapping move with them — `reqctx.UserIDFromContext` is the per-owner seam.
- [x] **`mcp-go` server becomes a thin shim over the registry** — no duplicated tool metadata, no drift risk between internal and external surfaces; `grep "Tool{Name:" internal/mcp/` returns zero matches.
- [x] **Tool registry tests** — `internal/tool/registry_test.go` covers registration, owner-scope enforcement (`TestDispatch_RequiresOwner`), input validation (`TestDispatch_RejectsInvalidArgs`), error mapping (`TestDispatch_SafeErrorMapping`: sentinel pass-through, unknown logs safe detail, panic recovered + logged), and concurrent-dispatch isolation (`TestDispatch_OwnerScopingUnderPanic`).
- [x] **Tool safety audit** — `TestDispatch_OwnerScopingUnderPanic` fires N concurrent goroutines across distinct user ids; one body panics, the others still see per-owner data, and the panicking dispatch returns the safe-detail mapped error.

**Why this is P0.2**: chat loop and MCP must consume the same registration. Otherwise adding a tool once means wiring it twice (or worse — chat goes through one set, MCP through another). Per-owner + safe-error mapping is non-negotiable and already proven on the MCP side; lifting it across is mechanical.

### 0.3 Plan-and-Execute loop

> **Implementation note:** The Go agent package that previously implemented these requirements was removed and replaced by a Python LangGraph orchestrator (OpenSpec change `agent-langgraph`). RAG stays in Go unchanged; tools dispatch via the existing MCP `tools/call` JSON-RPC endpoint; the chat SSE wire contract stays unchanged. The requirements below describe the **observable contract** (what the chat handler emits and what the frontend renders), not where the code lives.

- [x] **NEW `internal/agent/`** — `agent.Service.Run(ctx, req) (<-chan Event, error)`. Planner (one LLM call, optional `LLM_PLANNER_MODEL`) emits an ordered step plan; executor runs steps sequentially. **Shipped in Go, superseded by `agent-langgraph`** — the Go package was deleted and the chat handler is now a thin SSE proxy (`internal/chat.AgentProxy`) to the Python service at `PYTHON_AGENT_URL/agent/run`.
- [x] **Streaming per-step** — `plan` / `step_start` / `tool_call` / `tool_result` / `step_end` SSE frames so the frontend can render intermediate progress. Existing `delta` / `done` / `note` / `error` / `terminator` events keep their wire shape.
- [x] **Nested ReAct inside a single step** — when the planner emits a step with `kind: "react"`, the executor runs model → tool → model → … capped by `AGENT_STEP_REACT_MAX_ITERATIONS` (default 5). Default flow is plan-driven, not ReAct-driven.
- [x] **Caps** — `AGENT_PLAN_MAX_STEPS` (default 10), `AGENT_STEP_REACT_MAX_ITERATIONS` (default 5), both env-driven. Plans exceeding the cap are rejected with a typed SSE `error {code:"plan_too_large"}`. Caps moved to Python pydantic-settings (`PYTHON_AGENT_PLAN_MAX_STEPS`, `PYTHON_AGENT_STEP_REACT_MAX_ITERATIONS`).
- [x] **`POST /api/chat` becomes a thin shell** — validate → emit RAG context step (RAG is now one of the step kinds, not a pre-LLM injection) → `agent.Service.Run`. Behaviour is gated on `AGENT_ENABLED=true`; when false, today's single-turn path runs unchanged.
- [x] **Frontend `ChatPanel.tsx`** parses new SSE kinds and renders an "agent steps" collapsible region (tool-call name + collapsed args). Old token-stream UI unchanged.
- [x] **End-to-end integration test** — Python-side `tests/test_run.py` exercises plan → tool call → tool result → next step (`PlanToolLLM`), budget exhaustion (`BudgetGuardFires`), and Redis-down (`BudgetUnavailableFailsClosed`). Go-side `internal/chat/agent_proxy_integration_test.go` is the byte-for-byte forwarder regression test (httptest.Server returning canned Python SSE → assert response body equals upstream bytes).

**Why Plan-and-Execute over pure ReAct**:
- The AgenticStack user case ("why did conversion drop" → BI → logs → rules → KB → report) is **structured multi-step planning**, not open-ended try-and-error. Pure ReAct would burn tokens on every intermediate thought and is harder to debug.
- Plan-and-Execute separates the planner (one expensive LLM call) from the executor (cheap per-step). The planner can be a small/cheap model (`LLM_PLANNER_MODEL`); the executor can be a larger capable model. Cost amortizes across steps.
- Per-step streaming is unaffected by the planner choice; the frontend renders whatever step kind arrives.
- Nested ReAct inside a single step is allowed — open-ended sub-tasks get a sub-loop, but the outer flow stays predictable.

**Open question for 0.3**: planner JSON schema. Candidates:
- `{"steps": [{"kind": "rag|tool|llm", "name": "…", "args": …}, …]}`
- `{"steps": [{"id": "step_1", "kind": "tool", "tool": "list_feeds", "args": {"limit": 10}}, …]}`

Recommendation: the second. Each step has a stable `id` so SSE frames can be correlated client-side without position-dependent parsing. Decide in design.md of the OpenSpec change.

### 0.4 Task routing — **deferred to a follow-up change**

- [ ] **Intent → plan template** — classifier picks from a curated set of plan templates (`bi_anomaly`, `feed_search`, `general_qa`, …). Templates are version-controlled JSON.
- [ ] **No model routing for templates** — use a small fast model or rule-based keyword match. Cost must stay negligible relative to the planner.

**Why defer**: routing is the "intent → which plan" decision. It makes sense only after the planner itself works on raw user messages and we have evidence of recurring plan shapes. Building it before the loop runs risks over-abstracting templates that the planner never produces.

---

## P1 — Production engineering: state, observability, fault tolerance

### 1.1 Agent state — **landed in `openspec/changes/agent-run-state/`**

- [x] **`Run` / `Step` structs flowed through `context.Context`** — *(N/A: implementation moved to Python; the Go-side typed values in `internal/reqctx/` are no longer relevant to the agent runtime.)*
- [x] **DB-backed run state for crash recovery** — `agent.runs` (in the `agent.*` schema) + langGraph's `checkpoint_*` tables (in `langgraph.*`); run state survives Python-service restart. Resume from the last checkpoint via `graph.get_state(config)`. See `openspec/changes/agent-run-state/proposal.md` and `docs/agent-deployment.md`.
- [x] **Run TTL** — `RetentionCron` (in `backend/agent_py/agent/retention.py`) hard-deletes `agent.runs` rows where `status IN ('completed','failed')` AND `ended_at < now() - interval '<RETENTION_DAYS> day'`. Default 30 days. langGraph checkpoint tables pruned via langGraph's own retention story (out of this slice's scope).

**Why defer**: in-memory state is fine for slice 1; DB persistence lands when the slice-1 loop is exercised enough to reveal which state shape actually needs persistence (plan, partial step output, tool results, etc.).

### 1.2 Observability

> **Implementation note:** OTel spans + Prometheus counters + the per-user Redis budget guard moved to Python LangGraph (OpenSpec change `agent-langgraph`). Span names, counter names, and the Redis budget key contract stay identical so existing dashboards and alerts continue to work.

- [x] **OTel spans** `agent.run {run_id, user_id, plan_size, total_tokens}` and `agent.step.{plan,tool,react,llm} {step_id, kind, name, iteration}`. Tool-call timings as span attributes.
- [x] **Prometheus counters** — `agent_runs_total{kind,status}`, `agent_steps_total{kind,status}`, `agent_tool_calls_total{tool,ok}`, `agent_tokens_total{role,model}`.
- [x] **Per-user budget guard** — Redis-backed counter `agent:budget:u={user_id}:h={hour}` consulted before each `provider.Chat` call inside the loop. Cap: `AGENT_BUDGET_PER_USER_PER_HOUR` (default 200000). Exhaustion emits `error {code:"budget_exceeded"}` and terminates the run. **Fails closed.**
- [x] **OTel span for the budget check** — `agent.budget` so we can correlate budget exhaustion with run traces.

**Why this is P1.2 not P0**: the loop works without it; observability is the difference between "answer" and "production answer". But the per-user budget guard is **load-bearing for cost** (see Risks below) — lands with slice 1.

### 1.3 Fault tolerance — **deferred to a follow-up change**

- [ ] **Retry on malformed tool args** — when the model emits a tool call that fails registry validation, retry once with the validator's error message in the prompt. If still failing, fall back to a relaxed schema or terminate the step with `step_failed`.
- [ ] **Retry on tool execution failure** — for transient tool errors (timeout, network), retry with exponential backoff. Bound retries by `AGENT_TOOL_RETRY_MAX` (default 2). Non-retryable errors (validation, ownership) fail the step immediately.
- [ ] **Planner self-check** — when the planner emits a plan referencing an unknown tool, return `plan_invalid` to the planner for re-prompting (one retry). Exhaustion terminates with a typed SSE error.

**Why defer**: provider failover (`llm.Router`) already covers "model provider down". The new failure modes — model emits illegal JSON / illegal tool args — need empirical data on frequency before we tune retry budgets. Slice 1 will collect that data.

---

## P2 — Advanced autonomy

### 2.1 Cross-session memory — **deferred to a follow-up change**

- [ ] **`chat_sessions` + `chat_messages` tables** — move from FUTURE.md:84 into this roadmap. Soft-delete + retention cron.
- [ ] **Conversation summary on session end** — when a session goes idle for `AGENT_SESSION_IDLE_MIN` (default 30m), summarize into `chat_summaries` (or a `summary` column on `chat_sessions`) for fast re-hydration.
- [ ] **User preferences memory** — short structured facts ("user is an ad ops analyst at Acme") distilled from prior sessions. Stored as `chat_user_prefs(user_id, key, value, updated_at)`. Injected into planner prompt.
- [ ] **Cross-session recall** — when a new session starts, top-K retrieval over past sessions for the same user. Optional (`AGENT_MEMORY_RECALL_ENABLED` default `false` until retention + privacy review passes).

**Why P2**: cross-session memory introduces GDPR / data-retention / privacy review requirements that don't exist in P0/P1. Needs a separate design pass.

### 2.2 Self-reflection (CRAG / Adaptive) — **explicitly out of scope unless re-prioritized**

- [ ] **Not planned.** Per the project's "portability + minimalism" stance, add only when a concrete user-visible need appears.

---

## Risks (drives verification gates)

### Latency — **HIGH**

Per-step latency multiplies. **Must stream per-step, not per-run.** SSE frames `step_start` / `step_end` are the user-visible signal of progress. Frontend must render intermediate steps (tool-call name + collapsed args). Hard verification gate: end-to-end test that asserts `step_start` is emitted within `AGENT_FIRST_STEP_LATENCY_MS` (default 1500ms) of request start, regardless of plan size.

### Cost — **HIGH**

Token consumption multiplies by plan size + nested ReAct iterations. **Per-user budget guard is load-bearing.** Model tiering: cheap planner (`LLM_PLANNER_MODEL`, default to chat model), capable executor. Hard verification gate: `agent_budget_exhausted_total` counter increments under load test; `agent_tokens_total{kind="planner"}` stays <30% of `agent_tokens_total{kind="executor"}`.

### vLLM tool_calls consistency — **HIGHEST**

Two providers, two parsers, two SSE wire shapes. **Both providers need integration tests, not just OpenAI.** Hard verification gate: `backend/internal/llm/vllm/client_test.go` exercises tool-call deltas against canned SSE; CI fails if the parser drops any tool-call delta. Also: `LLM_FALLBACK_TOOLS_ENABLED` defaults to `false` until vLLM tool-call parity is verified end-to-end.

### Tool safety — **MEDIUM**

The MCP layer's per-owner + safe-error pattern is the reference implementation. **Not a new risk** — reuse the pattern, don't reinvent. Hard verification gate: `internal/tool/registry_test.go` includes a concurrency test asserting that `reqctx.UserIDFromContext` is enforced even when a tool panics.

### Context bloat — **LOW**

Long runs inflate the model's context window. **Compression (older-step summarisation) is explicitly out of scope for slice 1.** If a real workload blows the model's window, the gate is `AGENT_PLAN_MAX_STEPS` (already env-driven) plus optional step output truncation at `AGENT_STEP_OUTPUT_MAX_CHARS` (default 4000). Re-evaluate after slice 1 has load data.

---

## Sequencing — which OpenSpec changes, in which order

| Slice | OpenSpec change | Capabilities | Lands |
|---|---|---|---|
| 1a | `openspec/changes/agent-loop/` | `llm-structured-output`, `agent-tool-registry` (Go infra: provider seam + tool registry + MCP shim + chat SSE translator) | shipped |
| 1b | `openspec/changes/agent-langgraph/` | `agent-python-service`, `agent-budget-guard-py`, `agent-rag-bridge`, `chat-agent-proxy` (Python orchestrator + Go chat proxy + Go RAG bridge) | when Python service lands |
| 2 | `agent-run-state` | DB-backed Run/Step persistence (Python-side or Go-side — TBD) | when slice 1 is exercised |
| 3 | `agent-task-routing` | intent → plan template plan | when recurring plan shapes emerge |
| 4 | `agent-fault-tolerance` | retry on malformed tool args / planner self-check | after slice 1 has failure data |
| 5 | `agent-cross-session-memory` | `chat_sessions` / `chat_messages` + retention + summary + prefs | after privacy review |

Tick the `[ ]` items above as each slice lands. Archive-after rule: both `agent-loop` (Go infra) and `agent-langgraph` (Python orchestrator) close together once the Python service is wired in prod.
---

## Lessons from `agent-langgraph` slice 1

Patterns the next agent change should follow, learned the hard way in the `agent-loop` → `agent-langgraph` migration:

### Wire-contract tests before commit

- Self-consistent wrongness is the dominant failure mode when a wire format spans two runtimes. Both sides' unit tests passed while the bytes were wrong (`{kind, data}` wrapper that the frontend parser never read).
- When a component emits a wire format, the test must round-trip through the *consumer's* parser — not just assert the producer's in-process shape. For Go ↔ Python ↔ TypeScript SSE frames, that means parsing the Python output with the actual `chatService.ts` parser, or asserting byte-for-byte equality against a frozen fixture.
- Cross-runtime contract tests (Go proxy + Python service + frontend parser) are the highest-leverage tests in multi-runtime work. They fail loudly when any side drifts; the producer/consumer tests alone don't.

### Spec drift is a bug, not a doc issue

- Writing the spec is half the work. Cross-validating the spec against the wire test is the other half. `agent-langgraph` task 6.1 said "POST + JSON body" while the Go RAG endpoint was GET + query params — an internal contradiction that survived two reviewer passes.
- Write the spec, then write the test that exercises it, then write the code. If the test contradicts the spec, the spec is wrong.

### Multi-runtime design asks about dev-loop early

- Adding a Python service that requires docker while vLLM, backend, and frontend all run locally breaks the iteration loop. The user's first complaint on agent-langgraph was "why does this one have to be docker?"
- For multi-runtime work, the dev-loop story is part of the design phase. `make dev` now boots the Python service alongside the other two; vLLM stays opt-in via `--profile vllm` so local CPU dev doesn't pay GPU spin-up cost.

### Subagent delegation patterns that worked

- Chunk independent work (Go infra across `internal/llm` / `internal/tool` / `internal/mcp` / `internal/chat` / `internal/rag`) for parallel subagent runs. Each chunk had a tight scope (file lists, exact task IDs) so the subagent didn't drift.
- For Python service work, subagent launches hit token-plan rate limits several times. Chunking `batched subagent launches into 5-10 tasks each` and falling back to direct file writes kept progress when the rate limit hit.
- Auto-mode classifier denies destructive Bash (`rm`, `mv` to `/tmp/`, `rmdir`). Don't try workarounds — design the cleanup as a breadcrumb comment + a manual `rm` step the operator runs.

### Self-review is weaker than expected

- The `test-reviewer` agent caught 4 CRITICAL + 4 HIGH issues after I had marked the change "ready". None of those came from me. Run `/review` per chunk, not just at the end.
- For changes that introduce a new wire surface (a new event kind, a new RPC method), update both the change's tests *and* the consumer's tests in the same PR. Otherwise both drift apart silently.
