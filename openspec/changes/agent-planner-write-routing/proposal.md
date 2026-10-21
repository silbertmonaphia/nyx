# Proposal

## Why

The agent planner silently does not do what the user asked. On 「总结最近3个月我发的feed，**形成一篇新feed发出去**」 it plans `[rag, llm]` — drafts the feed text and never calls `create_feed`. The feed is never published. The user asked for a post; the agent produced a draft. The same defect costs `main-all-tools-combined` (drops the trailing `create_feed` after `[rag, web_search]`). Separately, on 「今天天气如何？**上网搜索看看**」 it plans `[react, llm]` instead of `[web_search, llm]`; the react loop *did* call `web_search` and *did* receive real weather rows, and the 4B then discarded them and told the user 「很抱歉，我目前无法获取…」.

All three are visible in the committed golden-set baseline as 0/3 case-level breaches (`evals/artifacts/20261016T095807Z/`, overall 0.73). None of them is caught by any other gate: the deterministic test layers all pass, and the eval suite that does catch them **runs nowhere** — it is not in CI, it last ran 2026-10-16, and `bugs.md`, which tracked all three as open, was deleted on 2026-10-19 while they were still red.

So this change fixes the planner routing defect *and* repairs the loop that was supposed to catch it. The score is the symptom; the reason it survived five days is the actual problem.

## What Changes

- **Planner prompt: mandate the write tool for state-changing requests.** Add a rule to `PLANNER_SYSTEM_PROMPT` (`backend/agent_py/agent/planner.py:76-84`) requiring a matching registry `tool` step whenever the user asks to publish/create/update/delete a feed, and stating that an `llm` step composes text and never performs the write. Expected to clear `main-rag-create-feed` and `main-all-tools-combined` together — they are one defect at two chain lengths.
- **Web tools prompt: prefer a flat `web_search` step for single lookups.** Extend `_PLANNER_RULE` (`backend/agent_py/agent/web_tools.py:520-524`) to prefer one `web_search` `tool` step and reserve `react` for requests needing multi-round iteration. Expected to clear `main-web-force-search`.
- **Answer-content assertions on the plan-shape cases.** The three red cases assert plan shape only, which is why the discarded-search-results defect shipped silently. Add deterministic answer assertions so a search that runs and returns rows cannot pass while the answer says the agent cannot search.
- **Re-measure before optimising, and re-measure after.** The suite's last run is five days stale and the newest artifact on disk (`20261016T143912Z`) reads `1.00` off a 3-sample partial that exercised only the budget case — a false green. Tasks require a clean baseline at current HEAD before any edit, and a confirming run after.
- **Wire the eval suite's deterministic unit gate into CI.** `evals/tests` (corpus validation, graders, transcript parsing) needs no model and is currently unwired; `agent-golden-eval` already names this as new scope.
- **Restore open-red tracking and correct a false corpus note.** `bugs.md` was deleted with three open bugs in it; `evals/cases/main.yaml` claims the update/delete siblings pass "on the same fixtures" when they use a different fixture set.

### Explicitly unproven

Every planner change above is a hypothesis about 4B behaviour, not a verified fix. Temperature 0 guarantees the *current* prompt→plan mapping is deterministic; it says nothing about a rewritten system prompt. The projected rates — 30/33 = 0.9091 after the first prompt edit, 33/33 = 1.00 after both — are **arithmetic over `passed/graded`, not predictions**. If the edits do not flip the model, they ship unproven; tasks must treat a non-improving re-run as a real outcome, not a reason to keep tuning until the number moves.

### Decision point for the reviewer

`docs_agent/AgentEvalPromptsRefine.md:39` originally listed `[rag, llm, tool:create_feed, llm]` as an *acceptable variant* of the create-feed shape. On 2026-10-16 an operator overrode that in `evals/cases/main.yaml:37` to the strict form, which is what makes the case red. This proposal takes the side that **the planner is wrong**: a user who says 发出去 expects a published feed, and drafting-instead-of-publishing is a defect regardless of how the corpus grades it. **A reviewer who disagrees should reject this and take the alternative** — relax the corpus expectation to the compose-then-write variant and record the interaction choice as intended. That is a product call, not a technical one, and it is cheaper to settle here than after the prompt edit lands.

## Capabilities

### New Capabilities

- `agent-plan-shape-policy`: which step kinds the planner must choose for a given request shape — specifically the write-tool mandate for state-changing requests and the single-lookup preference for web search. No existing capability specifies plan *content* policy; `agent-planner-structured` governs plan *emission mechanics* (structured binding, validation, retry), and `agent-web-tools` / `agent-tool-registry` treat both `tool` and `react` as equally valid hosts for `web_search`. This capability owns the choice between them.

### Modified Capabilities

- `agent-golden-eval`: (1) a case may carry plan-shape *and* answer-content assertions together, and a plan-shape-only case leaves answer quality ungraded by construction — the blind spot that let the discarded-results defect ship; (2) the deterministic unit gate runs in CI; (3) the overall floor records its provenance, so a report cannot silently disagree with the `EVAL_MIN_RATE` the runner actually passed.

## Impact

- **Code**: `backend/agent_py/agent/planner.py` (system prompt rules), `backend/agent_py/agent/web_tools.py` (`_PLANNER_RULE`), `evals/cases/main.yaml` (answer assertions, corrected note, floor re-derivation for the flaky budget case), `evals/graders/` (answer assertions on existing cases), `.github/workflows/` (unit-gate job), root `Makefile` (floor provenance comment).
- **Behaviour**: user-visible. A request to publish a feed will now actually publish it, and a request that names web search will return search-derived answers instead of an apology. Existing 3/3 cases (`main-rag-update-feed`, `main-rag-delete-feed`) must be re-verified — a broader write-tool mandate can perturb their plans.
- **Out of scope**: no Go backend changes, no frontend changes, no `.env*` edits, no changes to `agent-tool-registry` (the tools already dispatch correctly — the planner just never calls them), no corpus relaxation if the reviewer sides with the planner fix.
- **Rollout gates**: none. No production env flip, no deployed-stack smoke, no multi-day green window. If the reviewer additionally wants a "green across N consecutive nightly runs" sign-off before trusting a scheduled live eval, that window is rollout-shaped and belongs in a `-rollout` sibling — but no such gate is claimed here, and the scheduled-run task is scoped to a feasibility spike with a documented manual fallback instead.