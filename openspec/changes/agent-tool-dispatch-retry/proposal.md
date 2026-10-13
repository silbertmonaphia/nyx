# Proposal

## Why

The Python agent loop fails closed the moment a model emits a tool call the registry rejects or a tool's remote call flakes. The planner-side structured-output retry in `agent-planner-structured` covers only `Plan` validation; tool-dispatch failures still terminate the step. `FUTURE_AGENT.md §1.3` records both unchecked bullets — *Retry on malformed tool args* and *Retry on transient tool execution failure* — and the slice's sequencing note (slice 4) sets it after the planner work. Today, one bad tool arg or one transient timeout kills a step that the model could plausibly self-correct or that the network could plausibly recover from. This change adds the bounded retry-and-recover layer the planner retry deliberately left out, and ships a Prometheus counter so the failure mode is visible in dashboards.

## What Changes

- Add a `PYTHON_AGENT_TOOL_RETRY_MAX` setting (default 2, must be ≥ 0) consumed by the executor at the tool-dispatch site. The same knob caps both retry paths.
- **Tool-arg retry**: when the registry rejects a tool call (Pydantic validation error, missing required field, wrong type), the executor SHALL re-invoke the model once with the validator's error appended to the re-prompt under a `PREVIOUS_ATTEMPT_FAILED` block, capped at `PYTHON_AGENT_TOOL_RETRY_MAX` attempts total. The retry emits an `agent.step.tool` span attribute `retry_reason="tool_arg_invalid"`. On exhaustion, the step terminates with `step_failed` and a typed `tool_result` carrying the validator error (the user sees *what was wrong*; internal error text is mapped through the safe-error funnel).
- **Transient-failure retry**: when tool execution fails with a transient error (network timeout, 5xx from MCP/RAG bridge, connection reset), the executor SHALL retry with exponential backoff (base 250 ms, factor 2, jittered) bounded by `PYTHON_AGENT_TOOL_RETRY_MAX` and a hard ceiling of 5 s between attempts. Non-retryable errors (validation, ownership, 4xx except 408/429) fail the step immediately. Span attribute `retry_reason="tool_execution_transient"`. The retry budget is per-step and per-tool; it does not span steps.
- **Counter**: `agent_tool_retries_total{tool, reason}` on the same Prometheus exporter as `agent_planner_parse_failures_total`. Incremented on every retry attempt (not on success or first attempt). The `reason` label carries the same value as the span attribute (`tool_arg_invalid` / `tool_execution_transient`). `tool` carries the registry tool name.
- **Retryable-vs-fatal classification lives in the executor**, not in the registry. The Go registry's `Dispatch` returns the typed error class (validation / ownership / internal); the Python executor decides whether to retry, with the network error shape provided by the MCP / RAG HTTP clients.
- **Partial-failure banner**: when a run terminates with a system-recognised failure (tool retry exhausted, plan-level error such as `plan_too_large` / `plan_invalid` / `react_iterations_exceeded` / `budget_exceeded` / `budget_unavailable`), the orchestrator emits a `note{kind: "partial_failure", failed_steps: [...]}` SSE frame **before** the final `delta` frames; the SPA renders a `<PartialFailureBanner>` above the final answer so the user is not silently shown an answer built on a failed plan. Intermediate retry failures (recovered within budget) do NOT trigger the banner; RAG empty results do NOT trigger it (legitimate "no results"); `run_metadata_unavailable` does NOT trigger it (non-fatal per `agent-run-state`).

Not in scope:
- Cross-step or cross-run retries. The cap is per-step, per-tool.
- Retry of the planner's `Plan` emission. That's already covered by `agent-planner-structured` (`agent_planner_parse_failures_total`).
- Retry of budget checks or RAG retrievals on the warm path. RAG is a tool, so it falls under the transient retry automatically.
- Auto-tuning the cap. The env var is the knob; nothing reads run history to adjust it.

## Capabilities

### New Capabilities

- `agent-tool-retry`: how the Python agent executor retries tool-dispatch failures (malformed args, transient execution errors), and how the retry is observable.
- `agent-partial-failure`: the SSE `note{kind: "partial_failure"}` signal emitted when a run diverges from the plan, and the SPA's `<PartialFailureBanner>` component that surfaces it above the final answer. Triggers on system-recognised failures only; RAG-empty and `run_metadata_unavailable` are explicitly excluded.

### Modified Capabilities

- `agent-python-service`: the env-var list (requirement 7) gains `PYTHON_AGENT_TOOL_RETRY_MAX`.
- `agent-observability`: the counter list (requirement 2) gains `agent_tool_retries_total{tool, reason}`.

## Impact

- **Code**: `backend/agent_py/agent/executor.py` (retry loop around the tool-dispatch site at `executor.py:236`; `_collect_failed_steps` helper + `note{kind: "partial_failure"}` emission); `backend/agent_py/agent/observability.py` (counter registration); `backend/agent_py/agent/config.py` (new setting, validator); `backend/agent_py/agent/types.py` (`NoteEvent` extended with `kind: "partial_failure"` and `failed_steps: list[str]`); `frontend/src/features/chat/components/ChatPanel.tsx` + a new `<PartialFailureBanner>` component; `frontend/src/features/chat/types/chat.ts` + `chatService.ts` parse path.
- **Wire**: two additive SSE fields. The existing `tool_result` frame gains optional `retry_count: int` (defaults to 0; present only on the final result frame, ≥ 1 when a retry occurred). A new `note{kind: "partial_failure", failed_steps: [...]}` frame is emitted before the final `delta` frames. Both are backward-compatible — old clients ignore unknown fields and unknown note kinds.
- **Tests**: `backend/agent_py/tests/test_executor.py` (retry-path coverage; transport mocks; `_collect_failed_steps` unit tests covering the priority order); `frontend/tests/e2e/chat-agent-retry.spec.ts` (browser assertion that the banner renders above the final answer when a step fails, and is absent when none fail).
- **Docs**: `FUTURE_AGENT.md` §1.3 ticks both bullets with rationale pointing at this change. `backend/agent_py/README.md` env-var table gains the new setting. A new top-level `AgentFaultTolerance.md` indexes the whole agent's fault surfaces (covered vs not yet) and the partial-failure banner's role in surfacing them to users.
- **Rollout gates tracked in**: `openspec/changes/agent-tool-dispatch-retry-rollout/` (per the rollout-split convention). Gates: dev-stack live smoke against the running agent (tool-arg retry path visible in `tool_result` frames; transient retry visible in `agent_tool_retries_total`; `note{kind:"partial_failure"}` frame present before `delta`); Playwright e2e covering a step that retries once and succeeds (no banner), a step that retries and fails (banner with `failed_steps`), and a RAG-empty case (no banner).
