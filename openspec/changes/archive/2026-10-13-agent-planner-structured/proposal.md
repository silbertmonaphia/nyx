# Proposal

## Why

The Python agent's planner (`backend/agent_py/agent/planner.py:135-181`) currently asks the LLM for plain text, strips ` ```json ` fences, runs `json.loads` on the result, and re-validates with Pydantic. That triple-step is the single most failure-prone surface in the agent: every chat-tuned model wraps with prose or fences, every model can hit `max_tokens` truncation, and a single parse failure terminates the entire run with `error{code:"plan_invalid"}`. The cost model is explicitly cheap-planner / capable-executor (default `PYTHON_AGENT_LLM_PLANNER_MODEL = gpt-4o-mini`, overridable to local small models for cost/sovereignty), so "use a smarter model" isn't an option. Provider-side structured output (`langchain-openai` `with_structured_output(Plan)` / `bind_tools([PlanTool])`) collapses the three steps into one Pydantic-validated object straight from the provider's decoder, fixes the parse-fragility at every model tier, and lets a single retry-on-validation-failure recover from the residual cases (truncation, schema drift). Lands slice 4 of `FUTURE_AGENT.md` ahead of its deferred slot because the planner is load-bearing on every run.

## What Changes

- **Planner binds `Plan` via provider structured output.** `backend/agent_py/agent/planner.py` replaces the `ChatOpenAI(...)` invocation with `ChatOpenAI(...).with_structured_output(Plan)` for OpenAI-compatible providers (covers OpenAI and vLLM via the existing `PYTHON_AGENT_LLM_BASE_URL` switch). The fence-stripper at `planner.py:137-141` becomes unreachable. Free-text mode is retained as a fallback only when the provider rejects `with_structured_output` (e.g. a model that pre-dates the vLLM `hermes` parser; gated by `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=true` default).
- **Pydantic-only parse.** `Plan` / `PlanStep` already live in `backend/agent_py/agent/types.py`. The plugin enforces schema; Pydantic validates per-field. `_parse_and_validate` simplifies to Pydantic only.
- **Retry on validation failure.** When Pydantic validation fails (or `with_structured_output` raises), the planner re-prompts the model once with the validator's error string appended to the system prompt. Cap via `PYTHON_AGENT_PLANNER_RETRY_MAX` (default 1). Persistent failure emits the existing `error{code:"plan_invalid"}` + `done` and stops.
- **Cross-provider parity test.** Canned SSE replay against both OpenAI-shaped and vLLM-shaped responses (per `backend/internal/llm/{openai,vllm}/client_test.go` precedent, but at the Python layer) verifies the same test plan succeeds on both providers. Aligns with the `llm-structured-output` capability's "vLLM wire-shape divergence" risk note.
- **Counter parity.** Add `agent_planner_parse_failures_total{retry="ok|exhausted"}` Prometheus counter so dashboards expose the failure mode the change exists to reduce.
- **Env-var surface:** one new setting `PYTHON_AGENT_PLANNER_RETRY_MAX`, one new `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED`. No SSE wire changes, no Go-side changes, no frontend changes.

## Capabilities

### New Capabilities

- `agent-planner-structured`: The planner SHALL emit a Pydantic-validated `Plan` object obtained via provider structured output (no `json.loads` on free-text model output) and SHALL recover from validation failures via a bounded retry policy.

### Modified Capabilities

- `agent-python-service`: requirement 7 (env-var list) gains two entries — `PYTHON_AGENT_PLANNER_RETRY_MAX` (default 1) and `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED` (default true). No other requirement changes: the plan JSON shape (`{"steps":[{"id":..., "kind":..., ...}]}`) is fixed by requirement 2 and is unchanged; SSE frame kinds/order/payload shape are unchanged (requirement 5). The Go-side `internal/llm/{openai,vllm}/` provider seam is the subject of `llm-structured-output` and is independent (the Python agent uses `langchain-openai` directly, not the Go provider seam).

## Impact

- **Rollout gates tracked in `openspec/changes/agent-planner-structured-rollout/`** (vLLM `response_format` probe, live curl smokes, browser e2e execution, malformed-planner UI verification) per the rollout-split convention in `openspec/config.yaml`.
- **Code:**
  - `backend/agent_py/agent/planner.py` — replace free-text path with `with_structured_output`; add retry; delete fence-stripper.
  - `backend/agent_py/agent/config.py` — add `planner_retry_max` and `planner_structured_enabled` settings; append to the env-var list.
  - `backend/agent_py/agent/types.py` — possibly tighten `PlanStep` field types (e.g. `args: dict[str, Any] | BaseModel`) so per-step field typing can land later without another parser rewrite.
  - `backend/agent_py/agent/observability.py` — register `agent_planner_parse_failures_total` counter.
- **Tests:**
  - `backend/agent_py/tests/test_planner.py` — new file. Unit tests with canned model outputs:
    - structured path returns parsed `Plan` on first try;
    - validation failure on first try, recovery on second (validator error in second prompt asserted);
    - persistent validation failure emits `plan_invalid`;
    - `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false` falls back to the legacy free-text path.
  - Cross-provider parity: same canned response replay against OpenAI-shaped and vLLM-shaped tool-call wires.
- **Dependencies:** none added. `langchain-openai` and `pydantic` are already in `backend/agent_py/pyproject.toml`.
- **Wire contract:** unchanged. SSE frame kinds, payload shapes, ordering, error codes — all preserved.
- **Operational:** when `AGENT_ENABLED=true` in production, plan-parse-failure rate should drop visibly within one model-tier (cheap-model planner runs become reliable). Counter `agent_planner_parse_failures_total{retry="exhausted"}` becomes the dashboard signal.
