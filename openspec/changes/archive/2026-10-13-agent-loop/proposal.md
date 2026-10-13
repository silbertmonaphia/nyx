# Proposal: Agent Loop

> **Status note (added 2026-10-10):** The Go agent package (`internal/agent/`) was removed and superseded by the Python LangGraph orchestrator in OpenSpec change **`agent-langgraph`** (`openspec/changes/agent-langgraph/`). **This change retains the load-bearing infrastructure that the Python service consumes**: provider seam (`internal/llm/`), tool registry (`internal/tool/`), MCP shim (`internal/mcp/`), and the chat SSE wire envelope (`internal/chat/`). All tasks under "Provider seam", "Tool registry", and the SSE-translator parts of "Agent loop + observability" remain delivered. Tasks under "Agent loop" / "Agent observability" that implemented Go-side agent orchestration are now superseded; the live follow-up is `agent-langgraph`.

## Why

## Why

`POST /api/chat` today is a single-turn LLM call with optional RAG context injection — there is no loop, no tool dispatch, no plan-and-execute, no reflection. `llm.Provider.Chat` accepts `Tools` neither in its request nor in its messages; `vllm/client.go` actively drops tool-call deltas. The five feed tools exposed by `internal/mcp/` are reachable only by *external* agents — grep confirms `internal/chat/` and `internal/llm/` never call `internal/mcp/`. So when a user asks "why did yesterday's ad conversion drop?" the backend can't call BI, can't pull logs, can't query a rules engine, can't stitch evidence — it just answers from whatever the RAG retriever pulled into a single prompt.

This change turns `/api/chat` into a real Plan-and-Execute agent loop, builds an internal tool registry that chat and MCP both consume (no internal-vs-external drift), and extends the provider seam so OpenAI and vLLM both carry tool calls. The full P0–P2 roadmap lives in `FUTURE_AGENT.md`; this OpenSpec change covers slice 1 (provider tools + tool registry + Plan-and-Execute loop + OTel + per-user budget guard). Later slices (DB-backed run state, task routing, fault tolerance, cross-session memory) ship as their own OpenSpec changes against the same roadmap.

## What Changes

- **BREAKING (provider seam)**: `internal/llm/provider.go` gains `Tools []Tool`, `ToolChoice any` on `ChatRequest`; `Message` gains `ToolCallID`, `Name string`, `ToolCalls []ToolCall`. A new `RoleTool` is added; `Provider.Chat` is no longer text-only — its streaming contract is extended so a single chat can return a mix of content deltas and tool-call deltas, with the existing `onDelta` callback now carrying a typed `Delta` union. Existing callers (the RAG-only path) keep working — they pass zero tools and behave exactly as before.
- **BREAKING (vLLM client)**: `internal/llm/vllm/client.go` SSE frame parser is rewritten to surface tool-call deltas instead of dropping them. The OpenAI client gains tool-call support; both clients share a new internal `streamFrame` parser test (`backend/internal/llm/openai/client_test.go` + `vllm/client_test.go`).
- **NEW `internal/tool/`**: tool registry shared by `internal/chat/` and `internal/mcp/`. Existing MCP feed tools (`list_feeds`, `get_feed`, `create_feed`, `update_feed`, `delete_feed`) move into the registry as their first inhabitants — per-owner scoping (`reqctx.UserIDFromContext`) and the safe-error mapping (`api.ClassifyAndLog`) move with them. MCP and chat both call the registry, so adding a tool once makes it available to both surfaces.
- **NEW `internal/agent/`**: `agent.Service.Run(ctx, req) (runID, events)` implements a Plan-and-Execute loop. Planner (one LLM call, optional cheap model via `LLM_PLANNER_MODEL`) emits an ordered step list. Executor runs steps sequentially, each step emitting `step_start` / `tool_call` / `step_end` SSE frames so the frontend can render progress. Each step may internally nest a short ReAct loop (model → tool → model → …, capped at `AGENT_STEP_REACT_MAX_ITERATIONS`), but the default flow is plan-driven. `POST /api/chat` becomes a thin shell: validate → emit RAG context step → delegate to `agent.Service.Run`.
- **NEW SSE event kinds** on `/api/chat`: `step_start {step_id, kind}`, `tool_call {step_id, name, args}`, `tool_result {step_id, ok, payload_redacted}`, `step_end {step_id, status}`, `plan {steps: [...]}`. Existing `delta` / `done` / `note` / `error` / `terminator` events keep their wire shape.
- **NEW per-user token budget guard** at the agent layer: `agent_budgets` (or simpler: per-user counter in Redis with `AGENT_BUDGET_PER_USER_PER_HOUR` defaulting to 200k tokens) is consulted before each `provider.Chat` call inside the loop; exhaustion emits a typed SSE `error {code:"budget_exceeded"}` and terminates the run. Fails closed.
- **NEW OTel spans**: `agent.run {run_id, user_id, plan_size}`, `agent.step.{plan,tool,react} {step_id, name, iteration}`, with token counts and tool-call timings as attributes. `agent.budget` span on every check. Existing OTel surface extends; nothing is renamed.
- **NEW frontend event handling**: `chatService.streamMessage` parses the new SSE event kinds; `ChatPanel.tsx` renders intermediate steps (tool-call name + collapsed args, collapsible). Existing token-streaming UI is unchanged for backward-compatible clients (frontend gracefully ignores unknown kinds).

## Capabilities

### New Capabilities

- `llm-structured-output`: Provider seam (`llm.Provider`) supports tool calling and structured output. Adds `Tools` / `ToolChoice` to `ChatRequest`, `ToolCallID` / `Name` / `ToolCalls` to `Message`, `RoleTool` role, and a typed streaming `Delta` (content | tool_call | usage | note). OpenAI and vLLM clients both implement it. JSON-mode / `ResponseFormat` is *out of scope* for this slice — added when first caller needs it.
- `agent-tool-registry`: Internal tool registry shared by chat and MCP. Owns registration, per-owner scoping (`reqctx.UserIDFromContext`), input validation (struct tags), and safe-error mapping (`api.ClassifyAndLog` with a tool-specific `safeDetail`). Existing MCP feed tools migrate into it as the first inhabitants.
- `agent-loop`: Plan-and-Execute loop. Planner produces an ordered step plan; executor runs steps sequentially; each step may nest a short ReAct. Streams `plan` / `step_start` / `tool_call` / `tool_result` / `step_end` SSE frames. Capped by `AGENT_STEP_REACT_MAX_ITERATIONS` and `AGENT_PLAN_MAX_STEPS` (env-driven, defaults 5 / 10).
- `agent-observability`: OTel spans + Prometheus counters for the loop. `agent.run` / `agent.step.*` with token counts, tool-call timings, per-step kind/iteration attributes. Per-user token budget guard (Redis-backed counter) consulted before every `provider.Chat` call.

Cross-session memory (`FUTURE_AGENT.md` §P2.1) and DB-backed run state (`FUTURE_AGENT.md` §P1.1) are deliberately not in scope for this change — Run/Step state flows through `context.Context` with typed values in `internal/reqctx/` (in-memory only) so the layer is *designed* for persistence, but the persistence itself ships in a follow-up change.

### Modified Capabilities

None. The repo currently has no specs in `openspec/specs/`, so there are no existing capabilities to modify.

## Impact

- **Code**:
  - `backend/internal/llm/provider.go` — extend `ChatRequest` / `Message` / `Provider`, add `Delta` union.
  - `backend/internal/llm/openai/client.go` + `vllm/client.go` — implement tool-call streaming; rewrite vLLM SSE parser.
  - **NEW** `backend/internal/tool/` — registry, types, owner-scoped dispatch, safe-error mapping.
  - **NEW** `backend/internal/agent/` — planner, executor, run state, budget guard.
  - `backend/internal/mcp/service.go` + `handler.go` — refactor to consume `internal/tool` (delete duplicate tool definitions).
  - `backend/internal/chat/handler.go` + `service.go` — replace single-turn flow with `agent.Service.Run`; SSE emitter adds new event kinds.
  - `backend/internal/reqctx/` — typed values for `AgentRunID`, `AgentStepID`.
  - `backend/internal/platform/observability/` — extend with agent span helpers.
  - `frontend/src/features/chat/components/ChatPanel.tsx` + `services/chatService.ts` — parse + render new SSE kinds.
- **Dependencies**: none new. `golang.org/x/exp` constraints, native `encoding/json`. pgxmock + miniredis + testify already in `go.mod`.
- **Config (env, additive only)**: `AGENT_ENABLED` (default `false`), `AGENT_PLAN_MAX_STEPS` (default 10), `AGENT_STEP_REACT_MAX_ITERATIONS` (default 5), `AGENT_BUDGET_PER_USER_PER_HOUR` (default 200000), `LLM_PLANNER_MODEL` (optional — falls back to chat model).
- **API**: `/api/chat` request body gains `agent: { max_steps?, react_max_iterations? }` (both optional, capped server-side). Response SSE gains new event kinds; old kinds unchanged. Wire contract is additive — old clients keep working.
- **Frontend**: `ChatPanel.tsx` adds an "agent steps" collapsible region; `chatStore` adds `steps: ChatStep[]` for UI state. Existing token stream path is unchanged.
- **Database**: none. Conversation history tables land in the follow-up change.
- **Observability**: existing OTel + Prometheus unchanged; new spans/counters added.
- **Security**: per-owner scoping already enforced by the moved-from-MCP layer; tool input validation uses the existing struct-tag validator; per-user budget guard fails closed. No new attack surface beyond what's already analyzed for MCP.
- **Roadmap**: see `FUTURE_AGENT.md` for the full P0–P2 sequence (task routing, fault tolerance, DB-backed run state, cross-session memory, self-reflection). This change is **slice 1** of that roadmap.