# Design

## Context

Today `executor.py:296` calls `tools.call(tc_name, args)` exactly once. The model is bound to the tool registry's schema (`executor.py:244`), so the registry's first defense is the model-side schema binding — but the registry itself re-validates args and rejects malformed calls with a typed error. Any rejection (registry validation, network timeout against the MCP / RAG bridge, 5xx) today terminates the step on the first attempt.

Two failure classes are distinct and need distinct retry mechanics:

- **Validation rejection** — the model emitted an arg shape the registry rejects. Recovery: re-prompt the model with the validator's error message so it can self-correct. Same pattern as the planner retry (`agent-planner-structured`), but applied to the tool-dispatch seam.
- **Transient execution failure** — network timeout, 5xx, 408, 429 against the bridge. Recovery: retry the call with backoff; the model doesn't need to be re-invoked.

The classification lives in the executor, not the registry. The registry returns typed errors (`validation` vs `execution`); the executor decides retry semantics. This keeps the registry's contract simple (it already returns the typed error class) and keeps retry policy in one place.

The planner-side retry caps the *plan* emission, not the *tool* dispatch — the two budgets are independent. The counters are independent too (`agent_planner_parse_failures_total` vs the new `agent_tool_retries_total`).

## Goals / Non-Goals

**Goals:**
- Bounded retry on registry-rejected tool calls with validator feedback re-prompt.
- Bounded retry with exponential backoff on transient tool-execution failures.
- Counter `agent_tool_retries_total{tool, reason}` that mirrors `agent_planner_parse_failures_total` in shape and label discipline.
- Per-step, per-tool retry budget — exhaustion in one place doesn't burn the budget elsewhere.
- `step_failed` semantics preserved on exhaustion; the run continues to the next step.

**Non-Goals:**
- Cross-step or cross-run retries.
- Retry of the planner's `Plan` emission (already covered by `agent-planner-structured`).
- Auto-tuning the cap from run history.
- A retry budget for budget checks or RAG warm path. RAG is a tool — falls under the transient retry automatically.

## Decisions

### Two retry paths share the same counter, distinct `reason` label

Both retries increment `agent_tool_retries_total{tool, reason}` with `reason ∈ {tool_arg_invalid, tool_execution_transient}`. The counter shape mirrors `agent_planner_parse_failures_total{retry}` from the planner change so dashboards keep a consistent label discipline across the agent layer.

**Why not separate counters**: the operator's question is "are retries firing, and on what?" — one counter with a `reason` label answers that. Two counters fragment the answer and double the alert surface.

**Why not also a counter for retry *successes***: the existing `agent_tool_calls_total{ok}` already tells you whether a tool call landed. A retry that succeeds is just an `agent_tool_calls_total{ok="true"}` increment on a later attempt; the retry counter tells you how many times we had to try.

### Retry budget is per-step, per-tool — not per-run

The retry loop is scoped to the `step_id` (one step) and the `tool` (one tool within that step). A retry exhausted on `list_feeds` does not consume budget for `get_feed` later in the same step, and exhaustion in step 1 does not consume budget for step 2.

**Why**: a model that emits bad `list_feeds` calls and correct `get_feed` calls in the same plan is a recoverable case, not a runaway. Per-run budget would make the second tool fail just because the first one was flaky.

**Why not per-step only**: tools with different retry profiles (e.g. an HTTP-backed tool vs an in-process tool) shouldn't share a budget. Per-tool scoping is the natural unit.

### Backoff shape: exponential with jitter, hard ceiling

Base 250 ms, factor 2, jittered. Hard ceiling of 5 s between attempts (clamped — the per-step `AGENT_STEP_TIMEOUT` is the outer bound).

**Why exponential**: standard retry pattern; matches the existing observability story (`agent.budget` span attributes already carry timing).

**Why jittered**: prevents the thundering-herd retry against a recovering bridge.

**Why a 5 s ceiling**: a 2-default-cap with no ceiling would let a 2-cap retry wait `250ms * 2^1 + 250ms * 2^2 = 1.5s` — fine, but a future bump to 5+ cap would blow past the per-step timeout. The ceiling is a guard against future cap changes silently breaking the step's overall latency budget.

**Why not shared with the planner retry budget**: the two failures have different recovery characteristics. Planner-side, a re-prompt is essentially free (single model call). Tool-side, a re-prompt round-trips through a model call, *plus* a registry validation, *plus* the actual tool call. They're not the same cost.

### Retryable classification lives in the executor, not the registry

The Go registry's `Dispatch` returns a typed error class. The Python executor inspects the class to decide the retry path. Validation errors → re-prompt path. Network / 5xx / 408 / 429 → transient-retry path. 4xx other than 408/429 → fail-fast.

**Why**: the registry's contract is "return a typed error." Retry policy is a *client* concern — different clients (the agent executor, the MCP server, a future CLI tool) may want different retry semantics. Putting the policy in the registry couples all clients to one policy.

**Why not in the `ToolsClient` abstraction**: `ToolsClient` is the MCP / RAG HTTP wrapper. It could retry the *transport* layer (network timeouts on a single call), but it doesn't know about model re-prompting — that's an executor concern. The split is: `ToolsClient` retries the wire; the executor decides whether to re-prompt the model.

### Wire shape: `tool_result` gains an optional `retry_count` field

The existing `ToolResultEvent` carries `step_id`, `call_id`, `ok`, and optional `result` / `error`. We add optional `retry_count: int` (defaults to 0; present only on the final result frame, ≥ 1 when a retry occurred). The `tool_call` frame stays unchanged.

**Why optional**: backwards compatibility — old clients ignore unknown fields, so the addition is safe. Setting `retry_count: 0` on every success would inflate the payload for the common case; absent = 0.

**Why not a separate event kind**: the retry is a property of the eventual result, not a discrete event the client needs to render. Embedding it in the result frame keeps the stream simple.

### Re-prompt structure follows the planner pattern

The re-prompt is built exactly like the planner's `PREVIOUS_ATTEMPT_FAILED` injection: original messages, the assistant message that emitted the bad call, the rejected `tool_result` frame with the validator's error, a final user message quoting the error and the tool's argument schema.

**Why mirror the planner**: consistency. The two retry paths are conceptually the same — "the model made a mistake, give it the validator's feedback and let it try again." Different mechanics, same contract with the model.

## Risks / Trade-offs

- **Retry storms against a degraded bridge** → The 5 s ceiling + jitter caps the worst case. `agent_tool_retries_total{reason="tool_execution_transient"}` is the leading indicator; alert on its rate. The retry loop MUST NOT extend past `AGENT_STEP_TIMEOUT` — the per-step timeout is the outer wall.
- **Validator feedback leaks internal error text** → Reuse the existing safe-error funnel (`api.ClassifyAndLog` analogue in the Python side; the registry already maps internal errors before they reach the executor). The validator's *message* is not internal — it's a Pydantic-style "field X required, got Y" — but the registry MUST still funnel its own errors before they reach the executor's re-prompt construction.
- **Retry count → "did the user get the right answer?"** → A retry that succeeds is observably identical to a first-try success. The new `agent_tool_retries_total` counter is the only signal; `step_failed` exhaustion is observable via `agent_steps_total{kind, status="failed"}` and the `step_failed` frame. No wire change is needed to communicate "this took three tries."
- **Per-tool budget vs per-step budget confusion** → Tests pin both. The spec scenario "Retry budget is per-step, not per-run" makes the scoping explicit; the test `test_per_step_budget_isolates_retry_state` enforces it.
- **ReAct nested retries** → A `kind="react"` step's nested tool calls share the parent step's retry budget. They do not get their own per-iteration budget — that would let a 5-iteration ReAct consume 5 × 2 = 10 retries. The spec scenario is silent; tests pin the behavior. (Surfaced as an open question below — the implementer should confirm this with the operator before coding.)
- **`tool_result` shape change breaks a downstream consumer** → Additive only (`retry_count` is optional). Wire contract test round-trips through `chatService.ts`'s parser, not just the producer's in-process shape. Cross-runtime wire-contract test is a task in §3 of `tasks.md`.

## Migration Plan

No data migration. No schema change. The env-var default (`PYTHON_AGENT_TOOL_RETRY_MAX=2`) is the shipped default — operators who want the prior behavior set it to 0. Rollout (manual smokes, prod env flip, 1-week CI-green window) is gated on `openspec/changes/agent-tool-dispatch-retry-rollout/`, not this change. The implementation change archives when its own tests + the e2e are green.

Rollback is a single env flip: `PYTHON_AGENT_TOOL_RETRY_MAX=0` disables both retry paths and the executor returns to today's first-attempt behavior. The counter remains registered (Prometheus tolerates stale labels); no rollback script is needed.

## Open Questions

- **ReAct nested retries — resolved 2026-10-13**: per-step, per-tool budget applies **across the entire `kind="react"` step**, not per ReAct iteration. Iteration is the same model with growing context, not a fresh attempt — resetting the cap across iterations lets a runaway ReAct consume `5 × 3 = 15` retries and bust the 5 s backoff ceiling. The validator feedback is already in the model's messages from the prior iteration, so a fresh budget is not needed to apply it. Pinned by the test added in `tasks.md` §10.1.
