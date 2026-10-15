# Tasks

> **Rollout split (2026-10-13):** rollout-gated items were moved to `openspec/changes/agent-planner-structured-rollout/` per the rollout-split convention (`openspec/config.yaml`) — the previous "BLOCKED" entries below are pointers now, not checkboxes. Items here are `[x]` only when verified in the dev/CI environment.

## 1. Spike and settings

- [x] 1.1 Read `langchain-openai`'s `with_structured_output` source path to confirm the Pydantic-error-message surfaces to the caller (vs being normalized to `OutputParserException`). Verify by writing a one-off script that throws on a mocked model and inspecting the raised exception's `__cause__` / `args`.
  - **Result:** both ValidationError and OutputParserException surface cleanly to the caller; `_extract_structured_error` normalizes them. langchain-openai 1.7.0 (installed).
- `1.2` **Moved** to `agent-planner-structured-rollout/` §1.1 (vLLM `response_format` probe — needs the `--profile vllm` stack). The CI-side parsing boundary is covered here by `tests/test_planner_parity.py`.
- [x] 1.3 Add `planner_retry_max: int = 1` and `planner_structured_enabled: bool = True` to `backend/agent_py/agent/config.py`. Validator rejects `planner_retry_max < 0` at startup. Verify by booting `make dev-agent` and asserting a `PYTHON_AGENT_PLANNER_RETRY_MAX=-1` startup refuses to start.
  - **Verified** by `tests/test_planner.py::test_negative_planner_retry_max_refuses_settings`.
- [x] 1.4 Append both settings to `agent-python-service` requirement 7's env-var list in `openspec/specs/agent-python-service/spec.md`. Verify by `openspec validate` against `openspec/specs/agent-python-service/spec.md` succeeding.
  - **Verified** by `openspec validate --changes` — `valid: true`, zero issues. Implementation via the change's `specs/agent-python-service/spec.md` MODIFIED delta (added `agent-python-service` to `proposal.md` Modified Capabilities).

## 2. Structured-output binding on the primary path

- [x] 2.1 In `backend/agent_py/agent/planner.py`, build the planner as `ChatOpenAI(...).with_structured_output(Plan, method="json_schema")` when `settings.planner_structured_enabled` is true; route through the legacy `_parse_and_validate` path when false. The feature flag is the only branch point. Verify by `pytest backend/agent_py/tests/test_planner.py::test_structured_returns_parsed_plan_first_try` passing against a mocked `ChatOpenAI` whose `with_structured_output(...).ainvoke(...)` returns a `Plan` directly.
  - **Deviation (review round 1):** implemented as an **explicit `base.bind(response_format={... "strict": False}) | JsonOutputParser() | RunnableLambda(Plan.model_validate)`**, not `with_structured_output`. Verified against langchain-openai 1.7.0: `with_structured_output(..., strict=False)` silently forces `strict: true` for Pydantic-class schemas (the flag is dropped at bind time and `bind()` defaults strict=True when `response_format` is present). Strict mode can't represent `PlanStep.args`' free-form object — args would resolve to `{}` or the request be rejected. The spec's observable behaviour (structured binding, Pydantic validation, no `json.loads` on the primary path) is unchanged; only the binding mechanism differs.
- [x] 2.2 Confirm the executor downstream of `plan_node` receives a `Plan` instance, not a dict, by type-annotating the function signature on `PlanStep` and adding a one-line `assert isinstance(state["plan"], Plan)` in the executor's first line. Verify by `grep -n 'state\["plan"\]' backend/agent_py/agent/executor.py` showing the assertion and `pytest test_executor.py` (or equivalent) passing.
  - **Note**: `executor.py` already typed `plan` as `Plan | None`; the `assert isinstance(plan, Plan)` was added at the executor's first line. `test_executor.py` (4 tests) green.
- [x] 2.3 With `settings.planner_structured_enabled=False`, the legacy `_parse_and_validate` path runs unchanged. Verify by a regression test `test_legacy_parses_fenced_json` asserting the legacy path still parses a ```-fenced response.
  - **Note**: existing `test_parse_strips_code_fence` already covered this; added `test_structured_disabled_routes_through_legacy` to pin the feature-flag branch.

## 3. Bounded retry with validator feedback

- [x] 3.1 Wrap the structured invocation in a retry loop, capped at `settings.planner_retry_max`, that catches `OutputParserException` / `ValidationError` and re-prompts with the validator's error appended under a `PREVIOUS_ATTEMPT_FAILED` block in the system prompt. Verify by `pytest test_planner.py::test_retry_injects_validator_error` asserting the second-call prompt body contains the exception text.
- [x] 3.2 Add `test_retry_succeeds_after_validation_failure` — first `ainvoke` raises ValidationError with a precise message; second returns `Plan`. Assert the run-state `plan` is the second-attempt Plan, not the first-attempt error.
- [x] 3.3 Add `test_retry_exhaustion_emits_plan_invalid` — every `ainvoke` raises. Assert the planner node emits `error{code:"plan_invalid"}` followed by `done`; the run-state never transitions past planning; `agent_planner_parse_failures_total{retry="exhausted"}` increments by 1.
- [x] 3.4 Add `test_retry_cap_zero_is_one_attempt` — `PYTHON_AGENT_PLANNER_RETRY_MAX=0` short-circuits to a single attempt. Verify by mock-raising on the only attempt and asserting `error{code:"plan_invalid"}` (no retry observed in mock call count).
- [x] 3.5 Update `backend/agent_py/agent/types.py` `PlanStep.args` to a typed alias (`dict[str, Any] | BaseModel`) so per-step field-typing can land later. Backward compatible. Verify by `pytest test_planner.py` still passing with the new `args` shape.
  - **Amended (review round 1):** reverted to `dict[str, Any] | None`. The `BaseModel` union branch renders as an always-satisfiable empty-object alternative in the provider JSON schema — guided decoders (OpenAI, vLLM xgrammar) can silently resolve `args` to `{}`, dropping tool arguments. The task's intent (per-step field-typing can land later without a parser rewrite) is preserved: the planner is schema-driven, so per-kind arg schemas land by changing this annotation alone.

## 4. Counter and observability

- [x] 4.1 Register `agent_planner_parse_failures_total{retry}` on the Prometheus exporter in `backend/agent_py/agent/observability.py` alongside the existing `agent_runs_total` family. Verify by `curl :9001/metrics | grep agent_planner_parse_failures_total` showing the registered metric.
  - **Verified** by `tests/test_planner.py::test_retry_increments_ok_counter` + `::test_retry_exhaustion_increments_exhausted_counter` exercising the counter through the planner path.
- [x] 4.2 Emit an OTel span `agent.planner.structured` covering one full structured-output invocation (success or failure, before retry). Add span `agent.planner.retry` per retry attempt. Verify by inspecting the OTel exporter output during a successful test run; assert one `agent.planner.structured` span plus one or zero retry spans.
  - **Verified** by `_NoopTracer` in unit tests; the real OTel exporter requires the live Jaeger stack (related env constraint).
- [x] 4.3 Wire `agent_planner_parse_failures_total{retry}` increments into the planner node — increment `ok` on retry-recovered success, `exhausted` on retry-budget exhaustion. Verify by `pytest test_planner.py::test_retry_increments_ok_counter` and `::test_retry_exhaustion_increments_exhausted_counter`.

## 5. Cross-provider parity

- [x] 5.1 Create `backend/agent_py/tests/test_planner_parity.py`. Define a canned plan fixture (single tool step, args with shape `{name: str, limit: int, filters: list[str]}`).
- [x] 5.2 Implement an OpenAI replay — the mocked `ChatOpenAI.with_structured_output(...).ainvoke(...)` returns the canned plan parsed from a JSON `choices[0].message.content` shape. Verify by `pytest test_planner_parity.py::test_openai_replay` asserting identical `Plan` step order, id, and field values.
  - **As implemented:** drives the REAL `langchain-openai` client over `httpx.MockTransport` (no schema-object mock), asserting the request body (`response_format.type == "json_schema"`, `strict` falsy, `additionalProperties` present for free-form `args`) and full `Plan` equality against the fixture.
- [x] 5.3 Implement a vLLM replay — same canned plan arrives as a `choices[0].message.tool_calls[0].function.arguments` JSON string with the `hermes` parser envelope. The mocking layer normalizes the tool-call wire into the same `Plan` object the structured-binding returns. Verify by `pytest test_planner_parity.py::test_vllm_replay` asserting identical `Plan` instances against the OpenAI replay.
  - **Amended (review round 2):** under the `response_format` json_schema binding, BOTH providers return the plan as content JSON — so the vLLM replay is the same canned plan content through a vLLM-shaped response envelope (vendor-extra fields tolerated), not a `tool_calls` envelope. Content-envelope parity is the implemented contract; the spec scenario was amended to match (see `specs/agent-planner-structured/spec.md`).
- [x] 5.4 Add `test_vllm_wire_divergence_caught` — a canned response with a known vLLM-specific divergence (e.g. argument-string over-escaped Unicode escape) yields a normalized `Plan` via the same code path. Verify by replay and parse-success.
  - **Amended (review round 2):** implemented as `test_tool_calls_wire_divergence_recovers_on_retry` — a `tool_calls`-only response (the historical vLLM divergence) raises `OutputParserException` in the real chain, is retried with `PREVIOUS_ATTEMPT_FAILED` feedback, and recovers on the next attempt; persistent divergence terminates with `plan_invalid` (per the amended spec scenario).

## 6. Smoke test against running agent

- `6.1` **Moved** to `agent-planner-structured-rollout/` §1.2 (structured-path curl smoke — needs a running provider).
- `6.2` **Moved** to `agent-planner-structured-rollout/` §1.3 (legacy-path curl smoke).
- [x] 6.3 Add the smoke test to `backend/agent_py/tests/test_smoke_live.py` (gated on `PYTHON_AGENT_LIVE_DSN` / `PYTHON_AGENT_LIVE_BASE_URL` env vars per the test-skip posture of `agent-run-state` task 10.7). Self-skip when env vars are unset. Verify by `pytest backend/agent_py/tests/test_smoke_live.py --co` showing the new tests are collectable and self-skipping without env.

## 7. E2E browser test

- [x] 7.1 Added `frontend/tests/e2e/chat-agent-planner.spec.ts` (committed with this change). Drives a chat that must produce a `tool`-step plan; asserts the step row + `list_feeds` name render from the `tool_call` frame, every row reaches a terminal status, no error frame, and the final answer is non-empty.
  - **Amended:** the originally-drafted "retry counter visible at 0" assertion was dropped — the counter is Prometheus-side, not UI-observable; it is unit-covered instead. Execution against the live stack **moved** to `agent-planner-structured-rollout/` §2.1.
- [x] 7.2 Unit-level retry-then-succeed coverage exists here (`test_retry_succeeds_after_validation_failure`, the node-level terminal-frames test, and the parity recovery tests). The browser-side malformed-planner verification **moved** to `agent-planner-structured-rollout/` §2.2 (needs a controllable provider stub).
- [x] 7.3 Collection verified: `playwright.config.ts` (`testDir: './tests/e2e'`) picks the spec up — `npx playwright test --list` lists it. Execution **moved** to `agent-planner-structured-rollout/` §2.1.

## 8. Docs

- [x] 8.1 Tick `FUTURE_AGENT.md` §1.3 planner self-check item with rationale pointing at this change. `grep -n 'planner-structured' FUTURE_AGENT.md` ✓.
- [x] 8.2 Update `backend/agent_py/README.md` with the two new env vars (`PYTHON_AGENT_PLANNER_RETRY_MAX`, `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED`) and the counter name. `grep` finds both.
- [x] 8.3 Update `backend/agent_py/.env.example` with the two new vars (commented).

## 9. Integration checks

- [x] 9.1 Run the full Python pytest suite: `cd backend/agent_py && .venv/bin/python -m pytest tests/ -q` → **76 passed, 13 skipped** (skips are the live-DB / live-network gated suites, self-skipping without `TEST_ASYNC_PG_DSN` / `PYTHON_AGENT_LIVE_*`). Use the project venv — the pyenv default python lacks `asyncpg`, which silently changes collection. Review round 1 caught that the first pass ran only a subset; the full suite now includes `test_run.py` (structured stubs) and `test_ratelimit.py`.
- [x] 9.2 Run the Go test suite: `cd backend && SKIP_CONTAINERS=true go test ./...` — green (cached, but consistent across runs). The Go side is untouched; this is a regression check, not a Go change.
- [x] 9.3 Lint the Python side: ruff + mypy added to `pyproject.toml` dev deps; run via `.venv/bin/ruff check …` / `.venv/bin/mypy agent`. Verify: no new findings.
  - **Result:** ruff — **0 findings on every file this change touches** (28 mechanical auto-fixes + 6 manual, incl. two latent bugs: `F821` undefined `asyncio` in `test_run_resume.py`, and mutable class-level dicts flagged `RUF012`). 43 pre-existing findings remain in modules this change does not touch — ruff had never run (no config existed); recorded as baseline, package-wide adoption is a follow-up chore. mypy — **8 pre-existing errors** (`executor.py` ×6, `main.py`, `run_metadata.py`), all outside this change; every file touched here is mypy-clean (`StepKind` enum at synthesized steps, `SecretStr` for `api_key`, documented `type: ignore[call-arg]` on the vLLM-compatible `max_tokens` transport field).
- [x] 9.4 `openspec validate --change agent-planner-structured` reports the change as valid. `openspec status --change agent-planner-structured` shows every artifact `done`.
- [x] 9.5 Drift check: `cd backend && make sqlc-diff` — `sqlc generate` followed by `git diff --exit-code` returned exit 0 with no output. Go queries are untouched, as expected (this change is Python-side only).

## Review round 1 (test-reviewer, verdict: request changes) — fixes applied

- **Red tests repaired:** `test_run.py`, `test_ratelimit.py`, `test_run_resume.py` stubs now implement `bind` (the planner's structured path) alongside `ainvoke` (executor), keeping the pre-change call-1 numbering. The earlier "subset only" pytest run masked these; suite now runs in full via the project venv.
- **Uncaught paths closed:** `_build_structured_client` construction is wrapped (→ typed `planner_failed`); a non-`Plan` return from the chain is treated as a retryable conformance failure (→ `plan_invalid` after retries) instead of escaping the graph node. Regression tests: `test_provider_error_returns_planner_failed_no_retry`, parity `test_tool_calls_wire_divergence_recovers_on_retry`.
- **`strict` made real:** the binding is an explicit `bind(response_format=..., strict: false)` — see task 2.1 note. Parity tests assert `strict` is falsy on the wire and the schema carries `steps`.
- **Parity tests now exercise the wire:** rewritten against `httpx.MockTransport` — OpenAI-shaped and vLLM-shaped envelopes both replay the real `langchain-openai` client; the divergence case (response with `tool_calls`, no content) asserts retry-with-feedback and recovery.
- **Test doubles fixed:** `raise_parser` constructs `OutputParserException` correctly and is now exercised (`test_retry_recovers_from_output_parser_exception`); the `.error`-attribute branch was dead code and was removed from `_extract_structured_error` (langchain-core 1.6.9 stores the message in `args[0]`; the troubleshooting doc URL is stripped before injection).
- **Spec-scenario coverage added:** node-level terminal frames (`test_plan_node_emits_error_and_done_on_retry_exhaustion`), `retry_max=2` bound (`test_retry_max_two_permits_two_retries`), default settings (`test_planner_robustness_defaults`), executor type-contract oracle (`test_executor_rejects_non_plan_state`), plan-frame byte-identity across paths.
- **Docs/deps:** `pyproject.toml` pins `langchain-openai>=0.3` (json_schema method floor); README corrected (legacy fallback has no retry budget; explicit-bind rationale); `agent.step.plan` span regained its `iteration` attribute; legacy-path terminal `plan_invalid` now ticks `exhausted` (spec-literal).
- **Re-verified:** `.venv/bin/python -m pytest tests/ -q` → 76 passed, 13 skipped; `openspec validate --changes` clean.

## Review round 2 (test-reviewer, verdict: request changes — narrow scope) — fixes applied

- **Spec delta reconciled with the implemented contract** (`specs/agent-planner-structured/spec.md`): retry feedback now specified as appended to the re-prompt's final user message (was "system prompt"); parity requirement/scenarios rewritten — both providers return content JSON under `response_format` json_schema, and a `tool_calls`-only response is a retryable divergence with a dedicated scenario; counter requirement now per-run semantics with `plan_too_large` explicitly excluded; "no string-parsing step" scenario softened to "no fence-stripping and no `json.loads` on unconstrained free-text".
- **Stale mechanism descriptions corrected** in `.env.example`, `agent/config.py`, `pyproject.toml`, and the planner construction-fault comment — all four previously cited the abandoned `with_structured_output` mechanism.
- **Missing branch tests added:** `test_construction_fault_returns_planner_failed`, `test_non_plan_return_is_retryable_and_recovers`, `test_non_plan_return_exhaustion_ticks_exhausted`, `test_legacy_terminal_plan_invalid_ticks_exhausted` (+ `plan_too_large` no-tick assertion).
- **Comment honesty:** the `ValueError`-classification rationale now exists at the catch site (`planner.py`), including the subclass note for the redundant-looking tuple.
- **Parity nit fixed:** divergence-test docstring matches the exercised path (parser exception); `test_openai_replay` now asserts the `additionalProperties` consequence of non-strict mode.
- **Pre-existing stub nit fixed:** `test_run.py::_StubLlm` counter is class-level (planner `bind` + executor `ainvoke` in one sequence), so the executor's answer branch is live again.

## Review round 3 (test-reviewer) — **approved**

No CONFIRMED issues remained on the reviewed diff; every round-2 fix verified by execution. One post-approval comment-only tweak: the `pyproject.toml` pin comment no longer asserts an unverified `0.2.x` behaviour claim (only the 1.7.0 line was exercised). Remaining gates are the recorded-blocked live items (1.2, 6.1-6.2, 7.1-7.3) and lint tooling (9.3) — all environment-gated, none code-gated.

**Post-review (2026-10-13):** the live items were split to `openspec/changes/agent-planner-structured-rollout/` per the rollout-split convention; 9.3 landed (ruff clean on this change's files, mypy baseline recorded); the Playwright spec was authored and wired here (execution moved to rollout §2.1). No rollout-gated checkboxes remain in this change.
