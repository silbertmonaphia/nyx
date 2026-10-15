# Proposal

## Why

The planner is a probabilistic model (Qwen3-4B) whose failures are statistical, not deterministic: prompt regressions (the shipped react-step-without-objective bug), wrong tool selection, over- and under-planning. Unit tests pin plumbing; live e2e specs pin a handful of end-to-end journeys; neither reports *rates* for a prompt corpus, so planner-quality regressions can pass every test layer unseen. `AgentEvalPrompts.md` (operator-authored) already lists the golden prompts with expected plans, and its closing note asks for exactly this deliverable: a live-tagged harness whose input is the prompt list + config matrix, reporting rates, run nightly / pre-release. This change formalizes that file into an executable golden-set eval.

## What Changes

- New `backend/agent_py/eval/golden_cases.yaml` — the executable corpus migrated from `AgentEvalPrompts.md`. Schema (design D6): each case is a `prompt` plus a `modes` matrix; each mode declares the planner-catalog shape it runs under (`web_search: on` / `off`), the expected normalized step sequence, and a `min_rate` floor. The weather cases get both modes with mode-specific expectations, per the doc's note. Expected tool names are reconciled against the live catalog during task 1 (the doc's `add_feed` vs `create_feed` discrepancy is resolved there, not guessed here).
- New `backend/agent_py/eval/` harness (importable; `run_golden.py` also a CLI):
  - `cases.py` — YAML load + Pydantic validation; unknown step kinds / tool names fail at load against the production catalog names.
  - `match.py` — plan → normalized token sequence + strict ordered equality (design D1).
  - `report.py` — per-run artifacts: `eval/reports/<UTC-stamp>/report.json` + `summary.md` (per-sample plans, per-case and overall rates, model id, git revision).
  - `run_golden.py` — the live runner: preflight the model endpoint, then per case × mode × N samples (default 3, serial) call the SAME plan-production path the graph's planner node uses, with a `Settings` instance constructed for the mode; aggregate rates; exit code from thresholds (`--min-rate` overall floor, default 0.85; per-case floors from the YAML; `--no-fail` for trend runs).
- Root `Makefile`: `make eval-golden` runs the corpus against the local stack via the agent venv.
- `AgentEvalPrompts.md` is reduced to the human narrative plus a pointer to the YAML, which becomes the source of truth — the operator confirms this edit (operator-owned file).
- Deterministic unit tests cover the harness itself: YAML validation, token normalization, the matcher's pass + every failure shape (extra step, wrong order, wrong tool, missing llm tail), rate aggregation, report schema — canned plans, no LLM.
- **Wire**: none. No SSE frames, HTTP endpoints, or new runtime env vars; the runner reads the existing `PYTHON_AGENT_*` / `LLM_*` settings.
- New dependency: `pyyaml`, declared explicitly in `pyproject.toml` (the harness imports it; don't rely on a transitive).

## Capabilities

### New Capabilities

- `agent-golden-eval`: the corpus schema (prompt × mode matrix, expectations pinned to production catalog names), the live runner contract (production planning path, N-sample rates, thresholds as exit codes), the matching semantics, and the report artifacts.

### Modified Capabilities

- None.

## Impact

- **Code**: `backend/agent_py/eval/` (new), `backend/agent_py/tests/test_golden_eval_harness.py` (new, deterministic), root `Makefile` (one target), `backend/agent_py/pyproject.toml` (pyyaml).
- **Wire**: none. **Dependencies**: pyyaml only.
- **Docs**: `AgentEvalPrompts.md` (pointer, operator-confirmed), `backend/agent_py/README.md` (how to run, prereqs, report location, nightly suggestion).
- **Tests**: the deterministic harness tests above; the corpus run itself needs the live model and is operator-run — task 5 performs and commits the baseline report as evidence.
- **Rollout gates**: none — dev-only tooling with no production flip. The nightly schedule is operator-owned and documented in the README, which is why this change has no `-rollout` sibling (per the classification test in `openspec/config.yaml`).

**Non-goals**: execution-effect assertions (that `create_feed` actually wrote a row) — v1 is plan-level only; grading answer-text quality; automatic prompt tuning.
