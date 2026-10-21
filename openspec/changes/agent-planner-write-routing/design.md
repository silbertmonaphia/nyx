# Design

## Context

See `proposal.md` — Why for motivation. The technical facts that shape the approach:

- The plan is produced by **one LLM call** and nothing downstream corrects it. `PLANNER_SYSTEM_PROMPT` (`backend/agent_py/agent/planner.py:66-85`) is the entire policy surface. `_enforce_plan_shape` (`:641-700`) does three things — empty-plan fallback, `plan_max_steps` rejection, duplicate-id rejection — plus appending a trailing `llm` when the plan ends on a data step. It has no rule that can add a missing tool. There is no router node in the graph; `graph.py` is a straight `plan_node → executor_node`.
- Ruled out as causes for the three reds: step cap (`plan_max_steps=10`, observed 2/2/3), mode switches (none in the planner), schema restrictions (`PlanStep.tool` is a free-form `str`), parser drops (the raw plan frame already lacks `create_feed`; no retry fired, no `plan_invalid`), and env toggles (`EVAL_WEB_SEARCH=on` at baseline; `budget_stack` does not affect plan shape because the `plan` frame is emitted before execution).
- The tool catalog is already advertised correctly. `registry_planner_block` (`backend/agent_py/agent/tool_dispatch.py:216-239`) renders `- {name}: {desc}` per tool; `create_feed` is registered at `backend/internal/mcp/service.go:141`. The model is never denied the tool — it is never *required* to use it.
- Baseline evidence is stale. Last full run 2026-10-16 at rev `e6d92db`; 81 commits to HEAD, 11 touching `backend/agent_py/`, none touching plan shape. The newest artifact on disk (`20261016T143912Z`) reads 1.00 off a 3-sample partial that exercised only the budget case.
- The grader is not implicated. `evals/graders/plan_shape.py` compares tool-name tokens under strict ordered equality with one trailing `llm` normalised on both sides (`:62-66`, `:132`); there is deliberately no `react`↔`tool` aliasing, and broadening it would defeat the force-search case's stated purpose.

## Goals / Non-Goals

**Goals:**

- Move the three reds with the smallest change that addresses the actual mechanism (prompt policy), and bound that change so it cannot fire where it should not.
- Make every claim in the proposal falsifiable by a run the operator can execute, and record what a non-improving run means.
- Leave the suite able to catch this class of defect again without a human remembering to run it.

**Non-Goals:**

- A deterministic plan-repair or plan-intent layer in `_enforce_plan_shape`. Discussed and rejected below.
- Relaxing the corpus expectations (the reviewer's decision point, recorded in the proposal — this design assumes the planner is fixed).
- A scheduled/nightly live eval run (see Decision 4).
- Corpus growth, execution-effect assertions (did `create_feed` actually write a row), LLM-as-judge.

## Decisions

### 1. Fix this in the prompt, not in a deterministic repair layer

**Chosen:** add the rules to `PLANNER_SYSTEM_PROMPT` and `web_tools._PLANNER_RULE`.

**Alternative considered — deterministic repair in `_enforce_plan_shape`:** detect "state-change verb + no write tool", then either inject the tool step or reject with `plan_invalid` so the existing retry/`PREVIOUS_ATTEMPT_FAILED` loop re-prompts. This is strictly more reliable than a prompt edit on a 4B model, and the retry machinery already exists.

Rejected for this change because the trigger is intent detection — "形成一篇新feed发出去" versus "帮我写个feed草稿" is exactly the distinction the 4B gets wrong, and a verb-matching heuristic would encode a second, silent copy of the same judgement with its own failure modes (over-firing on drafts, missing phrasings). A wrong heuristic here is worse than the current bug because it fails *closed* on legitimate requests. If the prompt fix does not land, the fallback is a narrower version: repair only when the user's message contains an unambiguous publish verb **and** the plan already ends on an `llm` step whose prompt asks for feed-shaped output — that is a much narrower trigger than general intent detection. Keep this in reserve; do not build it speculatively.

### 2. Bound the write-tool mandate with an explicit draft carve-out

The obvious failure mode of a "must plan the write tool" rule is over-firing: a draft request, a greeting, or an ambiguous compose request acquiring a `create_feed` step. The spec requirement "The write-tool mandate is scoped to explicit state-change intent" exists to make this testable, and `main-refusal-direct-answer` / `main-smalltalk-greeting` are the canaries. The new rule must be worded narrowly enough to not collide with the existing `when unsure, prefer a single llm step` rule (`planner.py:84`) — the carve-out and that rule have to be reconciled in the same edit, not sequentially.

### 3. Re-measure before and after, and treat a non-improving run as a result

Ordering is load-bearing: a clean baseline at current HEAD must land before the first prompt edit, because the 0.73 figure is five days old and the most recent artifact is a false green. Optimising against an unconfirmed number repeats the mistake this change exists to fix.

A confirming run that does **not** improve the rate is a valid outcome and gets recorded as one, with the observed plans attached. The failure mode to avoid is iterating prompt wording until the number moves and landing on a change that scores well for reasons nobody can explain. Two runs (before/after), not a search.

### 4. Wire the deterministic unit gate into CI; spike — do not build — the scheduled live run

The unit gate (`pytest evals/tests`) needs no model and needs no GPU. It is explicitly named as new scope by the existing `agent-golden-eval` "Live-gated execution posture" requirement, so it is unambiguous scope.

A scheduled live run is a different proposition: it needs a GPU runner (GitHub-hosted runners have none), and a nightly that fails on runner provisioning or vLLM warm-up trains people to ignore the signal. The spec's live-gated posture exists for exactly this reason. Tasks therefore carry a **feasibility spike** whose output is a written recommendation plus a documented manual runbook fallback — not a workflow file. If the spike concludes a scheduled run is viable, it becomes its own change with its own spec delta, because "the live suite runs unattended" is a behavioural commitment the current posture explicitly disclaims.

### 5. Track open reds in the corpus, not in a standalone bug log

`bugs.md` was deleted on 2026-10-19 with three open reds inside it. Reinstating a separate tracker invites the same drift. The corpus case notes are already the durable home — they are versioned with the expectation, they travel with the case, and they cannot fall out of sync with the floor. Restore the diagnostic text into `evals/cases/main.yaml` notes and leave no standalone tracker.

**Alternative considered:** reinstate `bugs.md`. Rejected — a second source of truth for red cases is what let the deletion lose information in the first place.

## Risks / Trade-offs

- **Over-firing the write-tool mandate** → the draft carve-out (Decision 2) plus the existing greeting/refusal cases as canaries; a re-run that shows new reds in those cases is the tripwire.
- **Perturbing the currently-green mutation siblings** (`main-rag-update-feed`, `main-rag-delete-feed`, both 3/3) → the same re-run; these are the first thing to check in the after-report, before celebrating the three gains.
- **The prompt fix simply does not flip the 4B** → recorded as an outcome, not tuned around; Decision 1's fallback becomes live.
- **Answer assertions over-constrain a stochastic model** → forbidden-keyword assertions must target the specific denial phrasing (无法访问互联网 / cannot access), not broad vocabulary. Over-broad answer assertions would add a new flake class to a suite whose whole philosophy is rates-over-verdicts.
- **The unit-gate CI job is agent-env dependent** → it runs under `backend/agent_py/.venv` like the existing agent lint job; if the venv is not provisioned on the runner, that job fails loudly at setup rather than silently skipping the gate.
- **Answer-content coverage gap persists elsewhere** → out of scope to close corpus-wide; the report now marks answer coverage as ungraded per case, so the remaining gaps are enumerated rather than invisible.

## Migration Plan

No schema, no wire-format, no stored-data change. Rollback is a revert of two prompt strings and one corpus edit; the eval suite is additive and off by default.

## Open Questions

- Whether the flat `[web_search, llm]` shape is reachable at all for the force-search case's literal prompt. The weather sibling proves flat is reachable for a *different* string; with one prompt per case there is no counterexample. Resolvable only by running it — it does not change the specs or the task breakdown, only what a non-improving run means.
- Whether `main-budget-exceeded`'s observed 2/3-then-3/3 flakiness is reproducible. Two runs five minutes apart disagree; the trip point depends on real token spend. Tasks carry a re-derivation decision with both outcomes specified, so this does not need answering before implementation.