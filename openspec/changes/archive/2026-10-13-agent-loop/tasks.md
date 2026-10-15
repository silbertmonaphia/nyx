# Tasks

> **Scope of this change**: implementation of the Go agent infra (provider seam, tool registry, MCP shim, agent loop, observability) plus the frontend SSE parser for the new event kinds. Implementation is complete. **Production-rollout gates (manual smoke in prod, 1-week CI green, env flips, rollback drill) are deferred to `openspec/changes/agent-production-rollout/`** — that change owns the cross-cutting rollout.

## 1. Provider seam — tool-call support

- [x] 1.1 Add `Tool` / `ToolCall` / `Delta` types to `backend/internal/llm/provider.go` and verify `go build ./...` succeeds and `go test ./internal/llm/...` passes (no consumer changes yet)
- [x] 1.2 Extend `ChatRequest` with `Tools []Tool` and `ToolChoice any` and verify a zero-tools request produces a request struct identical to the pre-change shape (covered by a unit test against the request builder)
- [x] 1.3 Extend `Message` with `ToolCallID`, `Name`, `ToolCalls []ToolCall` and add `RoleTool` to the role enum and verify `go vet ./...` and the existing message round-trip tests pass
- [x] 1.4 Change the streaming callback from `onDelta(string, error)` to `onDelta(Delta, error)` and add a backward-compatible wrapper so existing callers can keep passing `func(text string, err error)` and verify the wrapper test in `internal/llm/provider_test.go` exercises both shapes
- [x] 1.5 OpenAI client surfaces tool-call deltas through the new `Delta` union and verify with a canned-SSE replay test in `internal/llm/openai/client_test.go` (asserts the same `Delta` shape OpenAI live produces)
- [x] 1.6 Rewrite the vLLM client's SSE frame parser to carry tool-call deltas instead of dropping them at `internal/llm/vllm/client.go:440-443` and verify with a canned-SSE replay test in `internal/llm/vllm/client_test.go` (asserts no tool-call delta is dropped, including vLLM wire-shape divergence cases)
- [x] 1.7 Add a parity test asserting both providers emit the same `Delta` shape against canned SSE in a shared `internal/llm/parity_test.go` and verify it passes locally and in CI
- [x] 1.8 Add `LLM_FALLBACK_TOOLS_ENABLED` (default `false`) to the router and verify the router refuses a tool-bearing request when the fallback provider's tool-call test is red by injecting a build tag in the test
- [x] 1.9 Run the existing single-turn path under `internal/chat/service_test.go` and verify byte-for-byte identical SSE output (existing assertions still pass) and document in `backend/LLM.md` that zero-tools requests are no-ops in the new code

## 2. Tool registry

- [x] 2.1 Create `backend/internal/tool/` package skeleton (`registry.go`, `tool.go`, `errors.go`) with `Tool` / `Registry` types and verify `go build ./...` succeeds
- [x] 2.2 Implement per-owner scoping in the dispatcher that reads `reqctx.UserIDFromContext` and refuses calls when the identifier is missing or empty and verify with `internal/tool/registry_test.go::TestDispatch_RequiresOwner`
- [x] 2.3 Implement input validation via the existing struct-tag validator and verify with `TestDispatch_RejectsInvalidArgs` (failing arg fails before the tool body)
- [x] 2.4 Implement safe-error mapping via `api.ClassifyAndLog` so internal error strings never reach the wire and verify with `TestDispatch_SafeErrorMapping` (sentinel → mapped; unknown → static safe detail; panic → recovered + logged)
- [x] 2.5 Migrate the five MCP feed tools (`list_feeds`, `get_feed`, `create_feed`, `update_feed`, `delete_feed`) into the registry as first inhabitants and verify by re-running the existing `internal/mcp/service_test.go` tests against the registry-backed handlers
- [x] 2.6 Refactor `internal/mcp/service.go` and `internal/mcp/handler.go` to consume the registry instead of holding their own tool definitions and verify by grepping `internal/mcp/` for `Tool{Name:` patterns — only the registry import should remain
- [x] 2.7 Add a concurrency test `TestDispatch_OwnerScopingUnderPanic` that fires N concurrent dispatches from different owners while one tool panics and verify each dispatch sees only its owner's row-level data

## 3. Agent loop + observability (behind `AGENT_ENABLED=false`)

> **Implementation note (2026-10)**: the Go agent package (this section) was deleted in `agent-langgraph` and replaced by a Python LangGraph orchestrator. The Go tests for the deleted package are not part of the live test surface; the behaviour is covered by the Python tests in `agent-langgraph` and `agent-run-state`. The Go infra (provider seam, tool registry, MCP, chat SSE wire) survives and is what the Python service consumes.

- [x] 3.1 Create `backend/internal/agent/` package skeleton (`service.go`, `planner.go`, `executor.go`, `budget.go`, `types.go`) with the planner JSON schema per design §6 and verify `go build ./...` succeeds
- [x] 3.2 Implement the planner: one `provider.Chat` call with structured JSON output and verify with `internal/agent/planner_test.go` using a stubbed `llm.Provider` that returns canned plans, including rejection of plans exceeding `AGENT_PLAN_MAX_STEPS`
- [x] 3.3 Implement the executor: sequential step runner, supports step kinds `rag` / `tool` / `llm` / `react` with iteration cap, and verify with `internal/agent/executor_test.go` using a stubbed registry + RAG service
- [x] 3.4 Add new SSE event kinds `plan` / `step_start` / `tool_call` / `tool_result` / `step_end` to `internal/chat/handler.go` and verify the existing `delta` / `done` / `note` / `error` / `terminator` events are byte-for-byte unchanged via a regression test in `internal/chat/handler_test.go`
- [x] 3.5 Wire `POST /api/chat` to `agent.Service.Run` when `AGENT_ENABLED=true` and remove the pre-LLM RAG injection (RAG is a step kind, dispatched by the executor) and verify by running `internal/chat/service_test.go` in both `AGENT_ENABLED=true` and `false` configurations
- [x] 3.6 Add the per-user hourly Redis budget counter (`agent:budget:u={user_id}:h={utc_hour}`) with TTL and verify with `internal/agent/budget_test.go` using miniredis — assert the counter increments per call, refuses after cap, and refuses on Redis unavailability (fails closed)
- [x] 3.7 Add OTel spans `agent.run` / `agent.step.{plan,tool,react,llm}` / `agent.budget` with the attributes from spec `agent-observability` and verify with `internal/agent/observability_test.go` using `tracetest.NewSpanRecorder()`
- [x] 3.8 Add Prometheus counters `agent_runs_total{kind,status}` / `agent_steps_total{kind,status}` / `agent_tool_calls_total{tool,ok}` / `agent_tokens_total{role,model}` and verify with `internal/agent/metrics_test.go` using `prometheus.DefaultGatherer`
- [x] 3.9 Add viper config for `AGENT_ENABLED` (default `false`), `AGENT_PLAN_MAX_STEPS` (default 10), `AGENT_STEP_REACT_MAX_ITERATIONS` (default 5), `AGENT_BUDGET_PER_USER_PER_HOUR` (default 200000), `LLM_PLANNER_MODEL`, `LLM_FALLBACK_TOOLS_ENABLED` (default `false`) in `backend/internal/platform/config/config.go` and verify with `internal/platform/config/config_test.go`
- [x] 3.10 End-to-end integration test: plan → tool call → tool result → next step against pgxmock + httptest (vLLM canned SSE) and verify with `internal/agent/service_integration_test.go` that the run produces the expected SSE frame sequence and that the budget guard fires when set low

## 4. Frontend parses new SSE kinds

- [x] 4.1 Extend the `ChatEvent` union in `frontend/src/features/chat/services/chatService.ts` with `plan` / `step_start` / `tool_call` / `tool_result` / `step_end` variants and verify with `chatService.test.ts` (existing single-turn events still parse)
- [x] 4.2 Implement the SSE parser for the new event kinds in `streamMessage` and verify with a unit test that feeds a canned SSE string and asserts each new event type is reconstructed correctly
- [x] 4.3 Add a `ChatStep` type to `frontend/src/features/chat/store/chatStore.ts` for UI state only (not persisted to localStorage — XSS surface concern stays per `FUTURE_FRONTEND.md` §10) and verify with `chatStore.test.ts`
- [x] 4.4 Render an "agent steps" collapsible region in `frontend/src/features/chat/components/ChatPanel.tsx` (tool-call name + collapsed args) and verify with `ChatPanel.test.tsx` (RTL) that the steps appear when events stream and stay hidden in single-turn mode
- [x] 4.5 Run the full frontend test suite (`npm test`) and verify zero regressions in existing token-streaming tests

## 5. Dev-environment flip

- [x] 5.1 Set `AGENT_ENABLED=true` in `backend/.env.example` (local dev only) and verify `make dev` boots with the agent path
- [x] 5.3 Update `FUTURE_AGENT.md` to tick the items this change shipped (P0.1, P0.2, P0.3, P1.2 partial — per the roadmap doc's checkbox convention) and verify by `grep -E '\[ \]' FUTURE_AGENT.md` that remaining items stay open

## 6. Deferred to `agent-production-rollout`

> The four items below are cross-cutting production-rollout gates shared with `agent-langgraph` and `agent-run-state`. They moved out of this change to a single rollout change so the three implementation changes archive cleanly while the rollout work tracks as one workstream. See `openspec/changes/agent-production-rollout/tasks.md`.

- [ ] 5.2 (moved) Manual smoke test against the local stack with a `list_feeds` plan → see `agent-production-rollout/1.1`.
- [ ] 6.1 (moved) 1-week CI green before flipping `LLM_FALLBACK_TOOLS_ENABLED=true` → see `agent-production-rollout/2.1`.
- [ ] 6.2 (moved) Flip `LLM_FALLBACK_TOOLS_ENABLED=true` in production + canary → see `agent-production-rollout/2.2`.
- [ ] 6.3 (moved) Flip `AGENT_ENABLED=true` in production + Prometheus reachability → see `agent-production-rollout/2.3`.
- [ ] 6.4 (moved) Rollback drill (AGENT_ENABLED=false in staging) → see `agent-production-rollout/2.4`.