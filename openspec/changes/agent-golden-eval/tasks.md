# Tasks

## 1. Corpus

- [ ] 1.1 Write `backend/agent_py/eval/golden_cases.yaml` migrating every case from `AgentEvalPrompts.md` (rag-summary, feed-from-summary, update-negative-feeds, delete-negative-feeds, weather ×2 modes, weather-explicit-search, all-tools), using the D6 schema. Reconcile expected tool names against the live planner catalog (`registry_planner_block` / the MCP registry tool list) — the doc's `add_feed`-style names are corrected to what the catalog actually emits. Verify: `python -m pytest tests/test_golden_eval_harness.py -v` loads and validates the shipped YAML (task 2 lands the loader first).
- [ ] 1.2 Add `pyyaml` to `backend/agent_py/pyproject.toml` runtime deps and refresh the venv. Verify: `.venv/bin/python -c "import yaml"` succeeds on a fresh `pip install -e .`.

## 2. Harness core (deterministic)

- [ ] 2.1 `backend/agent_py/eval/cases.py`: Pydantic models for the D6 schema + `load_cases(path)`; reject unknown step kinds/tool tokens at load (token vocabulary from the production catalog module, single source). Verify: `python -m pytest tests/test_golden_eval_harness.py -v` covers valid load, unknown token, missing `modes`, duplicate case id.
- [ ] 2.2 `backend/agent_py/eval/match.py`: `tokens(plan) -> list[str]` (D1 normalization) + `matches(plan, expect) -> (bool, detail)` with failure detail naming the first divergence (extra step, missing step, wrong order, wrong tool). Verify: pytest cases for every failure shape plus the pass path, including the auto-appended-llm-tail plan form.
- [ ] 2.3 `backend/agent_py/eval/report.py`: `write_report(dir, run_result) -> paths` producing `report.json` (D7 schema) + `summary.md`; deterministic given an injected timestamp. Verify: pytest — golden-file comparison of both artifacts for a canned run result; schema keys asserted.
- [ ] 2.4 `backend/agent_py/tests/test_golden_eval_harness.py`: the deterministic suite above (canned plans and YAML fixtures under `tests/fixtures/`; no LLM, no network). Verify: `python -m pytest tests/ -q` green with the new file included.

## 3. Live runner

- [ ] 3.1 `backend/agent_py/eval/run_golden.py`: CLI (`--cases`, `--samples`, `--min-rate`, `--no-fail`, `--report-dir`); preflight the model endpoint with a clear unreachable message; per case × mode build the D5 `Settings`, call the D4 production planning path N times serially, normalize + match, aggregate rates, write the D7 report; exit code per D2. Skip-mode reporting for unmet prerequisites (e.g. web-on without a search key) per D5. Verify: pytest monkeypatches the planner callable — rate aggregation, exit codes (breach vs `--no-fail`), skip handling, and preflight-failure message all covered; plus one live run against the operator stack recording its report path in this task.
- [ ] 3.2 Pin D4 with a test: the callable the runner invokes is the one `plan_node` uses (import-level assertion, not a copy). Verify: `python -m pytest tests/test_golden_eval_harness.py -v` — the assertion fails when pointed at any other planning entry.

## 4. Entrypoints and docs

- [ ] 4.1 Root `Makefile`: `eval-golden` target (`backend/agent_py/.venv/bin/python -m eval.run_golden`, run from `backend/agent_py`) added to `.PHONY` + the help block. Verify: `make eval-golden` exits with the clear unreachable-model message when the stack is down, and writes a report when it is up.
- [ ] 4.2 `backend/agent_py/README.md`: an eval section (what it measures, prereqs, `make eval-golden`, report location, the nightly/ pre-release suggestion, the rate-not-pass/fail philosophy). Verify: section renders and commands match the Makefile target.
- [ ] 4.3 `AgentEvalPrompts.md`: reduce to the narrative + pointer at the YAML (operator-owned file — confirm the edit with the operator before landing). Verify: `openspec validate agent-golden-eval` still passes and the pointer path exists.

## 5. Baseline

- [ ] 5.1 Run the corpus against the current live stack (serial) and commit the baseline report under `backend/agent_py/eval/reports/BASELINE-<stamp>/` with the observed per-case rates recorded in this task. Tune the YAML `min_rate` floors and the web-off weather expectation from the observed rates (design D2/D6). Verify: committed report present; every floor ≤ observed rate; a rerun reproduces rates within noise (record both runs).
