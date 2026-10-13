# Design

## Context

`backend/agent_py/agent/planner.py:50-100` is the planner node in the LangGraph `StateGraph`. Today it calls `ChatOpenAI(model=settings.planner_model, ...)` (which honours `PYTHON_AGENT_LLM_BASE_URL`, so it covers vLLM-as-OpenAI), passes a system prompt demanding a JSON `Plan`, and returns to `_parse_and_validate` (lines 135-181). `_parse_and_validate` does:

1. Strip ` ``` ` / ` ```json ` fences via the regex-equivalent at lines 137-141.
2. `json.loads(text)` on the remaining string (line 144).
3. Walk the parsed dict, validate each step through `PlanStep.model_validate(s)` (line 177).

Failure modes (per `tmp_learn_agent.md` "What's not done yet" and the proposal): fence wrapping, leading/trailing prose, trailing commas, `max_tokens` truncation, greedy-decoded duplicate `id`s, schema drift across provider versions, type-coercion drift (`"10"` vs `10`). Any single failure terminates the run.

The cost model is explicitly cheap-planner / capable-executor (config default `PYTHON_AGENT_LLM_PLANNER_MODEL` falls back to `PYTHON_AGENT_LLM_MODEL`, itself defaulting to `gpt-4o-mini`). "Use a smarter model" is not an option. Constraints on the design:

- SSE wire contract unchanged (10 enumerated frame kinds, payload shapes, ordering). The proposal must NOT touch `agent-python-service` requirement 5.
- Plan JSON shape (`{"steps":[{"id":..., ...}]}`) unchanged (`agent-python-service` requirement 2).
- Go-side provider seam (`internal/llm/{openai,vllm}/client.go`) is independent; the Python service uses `langchain-openai` directly with a custom `base_url`.
- vLLM must work via the same `ChatOpenAI` client. vLLM tool-calls wire (with the `hermes` parser, gate at `docker-compose.yml` override for Qwen3-Instruct) MUST be parity-verified with OpenAI-shaped responses at the planner boundary.

## Goals / Non-Goals

**Goals:**
- Eliminate the `json.loads`-on-free-text path from the primary planner flow.
- Reduce plan-parse-failure terminated runs to near zero at every model tier, including `gpt-4o-mini` and Qwen3-Instruct-via-vLLM.
- Recover from the residual schema-drift and `max_tokens` cases via bounded retry that uses the validator's error message as feedback.
- Preserve backward compatibility for vLLM users with the legacy parsing path through a configuration flag (default on).
- Expose parse-failure rate as a Prometheus label so dashboards and alerts can detect regression.

**Non-Goals:**
- Replacing the executor's `_run_tool` schema validation (that's `agent-fault-tolerance` slice 4 follow-up).
- Adding JSON-mode `ResponseFormat` to the Go-side `internal/llm/` provider seam (that's `llm-structured-output` / `internal/llm/{openai,vllm}/client.go` §0.1).
- Storing the parser-error history per `(user_id, run_id)` for analytics. Out of scope; counter-only.
- Migrating the planner to a non-`ChatOpenAI` client (Anthropic, Cohere, etc.). Future work.

## Decisions

### Decision: bind `Plan` via `with_structured_output(Plan)`

**Choice.** The planner uses `ChatOpenAI(...).with_structured_output(Plan, method="json_schema")` for OpenAI and vLLM providers that accept `response_format: {type: "json_schema"}`. `langchain-openai` translates the Pydantic schema into the OpenAI `json_schema` shape and parses the response back into a Pydantic `Plan` before the planner sees it. The Pydantic-validated object reaches `_post_plan` without any string-parsing step on the planner side.

**Alternatives considered.**
- `bind_tools([PlanTool], tool_choice="required")`. Works on OpenAI and vLLM-with-hermes-parser, but the vLLM tool_calls wire historically diverged from OpenAI (the `llm-structured-output` spec's risk note). Picking this approach forces a vLLM parity-mapping layer at the planner boundary that `with_structured_output(json_schema)` already gets for free because both providers accept the same `response_format`.
- `output_parsers.PydanticOutputParser` with manual prompt instructions. Same fragility profile as today; rejected.
- Hand-rolled `json_repair` library. Adds a dependency; covers trailing commas but not truncation or schema drift; rejected.

### Decision: per-pydantic-failure retry, validator error as feedback

**Choice.** When `with_structured_output(...).ainvoke(...)` raises (Pydantic validation failure on the provider's emitted object, or schema-refusal at decoding), the planner:
1. Formats the exception message (one short string) and appends it to the system prompt under a `PREVIOUS_ATTEMPT_FAILED` block.
2. Re-invokes the bound `Plan` chain.
3. Repeats up to `PYTHON_AGENT_PLANNER_RETRY_MAX` times (default 1).
4. On exhaustion, emits `error{code:"plan_invalid"}` + `done` and short-circuits.

The counter `agent_planner_parse_failures_total{retry="ok|exhausted"}` increments on every validation failure (label `ok` for recovered, `exhausted` for terminal).

**Alternatives considered.**
- Self-reflection (a separate critique pass). Rejected for slice purposes — the validator's structured error already names the field; a critique pass adds latency without proportional value.
- Concurrent multiple-sampling (`n=4`, pick first parseable). Rejected — doubles cost and adds non-determinism; bounded retry is enough at the small-model tier.
- Persisting the retry history. Rejected — adds DB schema for negligible telemetry value; counter is enough.

### Decision: keep free-text parsing behind a feature flag

**Choice.** `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED` (default `true`). When `false`, the planner routes through the legacy free-text `_parse_and_validate` path so operators can compare run-rate of `plan_invalid` between the two paths during rollout. The fence-stripper code at lines 137-141 stays; it just becomes unreachable on the primary path. The feature flag is removed in a later openspec change once dashboards confirm the structured path dominates.

**Alternatives considered.**
- Delete the free-text path entirely at landing. Rejected — vLLM with non-hermes parsers and Anthropic-style providers may need a fallback; cutover is staggered.
- Branch on provider name to decide which path to take. Rejected — adds branching at runtime; the configuration flag is the cleaner cutover mechanism.

### Decision: cross-provider parity test as gate

**Choice.** A new test file `backend/agent_py/tests/test_planner_parity.py` replays one canned schema-conformant plan as (a) an OpenAI `chat.completions` JSON response and (b) a vLLM `tool_calls`-shaped response. Both replays are parsed through the planner's actual `with_structured_output` chain (using a mock `ChatOpenAI` whose response is set per provider shape) and must yield identical `Plan` instances. The test runs in CI and is the gate that vLLM tool-call wire divergences get caught before production.

**Alternatives considered.**
- Relying on the existing `llm-structured-output` Go-side test. Rejected — different runtime, different wire-parser stack.
- A live-provider integration test (hits real OpenAI / real vLLM). Rejected — flaky in CI; the canned-replay approach exercises the same parsing logic deterministically.

### Decision: counter labels limited to `retry`

**Choice.** `agent_planner_parse_failures_total{retry="ok|exhausted"}`. Two labels only. Adding `provider` or `model` would inflate cardinality on the dashboards for the same actionable information.

**Alternatives considered.**
- Per-failure-type labels (`{error="schema_drift|truncation|fence"}`). Rejected — too noisy; the same error counter covers all of them, and OpenTelemetry spans preserve the per-failure detail.
- Histogram of recovery latency. Rejected — out of scope; OpenTelemetry spans already capture retry timing.

## Risks / Trade-offs

- **Risk:** `with_structured_output(method="json_schema")` requires the provider to honour `response_format`. vLLM with the `hermes` parser does; vLLM with older parsers (`auto`, `guided_json` only) may not. → **Mitigation:** the parity test catches this in CI; the feature flag backs out to the legacy path if it doesn't.
- **Risk:** Bumping retry count past 1 increases planner latency under degradation. → **Mitigation:** default cap is 1; ops can tune via `PYTHON_AGENT_PLANNER_RETRY_MAX`; counter `agent_planner_parse_failures_total{retry="exhausted"}` flags the degradation mode for dashboards.
- **Risk:** Counter label cardinality. → **Mitigation:** cardinality is bounded (two values); verified against the existing `agent_*_total` family in `agent/observability.py`.
- **Risk:** Removing the fence-stripper code in the same change breaks the legacy fallback path if the feature flag is toggled off after deletion. → **Mitigation:** leave the fence-stripper code in place behind the flag (decision above); removal is a separate openspec change after dashboards confirm.
- **Risk:** `langchain-openai` upgrades change `with_structured_output` semantics. → **Mitigation:** pin `langchain-openai` minimum version in `pyproject.toml`; the parity test catches any regression at the parsing boundary.

## Migration Plan

1. **Implement behind the flag.** `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=true` is the default; the structured path is reachable from day one. Legacy path stays reachable.
2. **Test in CI.** `pytest backend/agent_py/tests/test_planner.py` and `test_planner_parity.py` run on every PR; both must pass.
3. **Roll out:**
   - Stage 1 (merge): feature flag `true` for dev + CI; legacy path tested for parity.
   - Stage 2 (after green for ≥1 week per the `agent-run-state` 13.1 gate precedent): run historical replays of past `plan_invalid` failures through the new parser to confirm they recover.
   - Stage 3: monitor `agent_planner_parse_failures_total{retry}`; expect `ok` rate rising, `exhausted` rate near zero. Compare against the pre-change counters.
4. **Cutover (separate openspec change):** delete the fence-stripper + `_parse_and_validate` legacy path. Out of scope here.
5. **Rollback:** set `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false` on the running pod to revert the planner to the legacy path. Counter changes are additive — no Prometheus rules depend on the new label values existing.
6. **Production flip timing:** depends on `agent-run-state` archiving first; this change ships behind the flag, so it's safe to flip per-environment without waiting for archive.

## Open Questions

- Should the retry inject the validator error into the **system** prompt (cross-turn persistent prompt) or a **user**-tail (one-shot correction)? Current design picks system because the model self-corrects better when the corrective signal sits with the schema instructions. Cheap-model behaviour may differ; if `ok`-rate stays low at the cheap tier, this becomes a follow-up experiment.
- Does `langchain-openai` `with_structured_output` carry the raw provider error to the caller, or does it normalize to `OutputParserException`? If normalized, the injected feedback message will be the parser exception text, not the Pydantic field-level error. The latter is more useful. Implementation-time spike needed; falls out of the test scope.
