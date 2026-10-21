# Design

## Context

See `proposal.md` — Why. The facts that shape the approach:

- `_PLANNER_RULE` in `backend/agent_py/agent/web_tools.py:520-524` advertises `web_search` as a `tool` and says only "use these for requests about current events". It never steers away from `react`. `react` is a first-class kind (`planner.py:71`), so both shapes are reachable and the model picks.
- `_render_prior_data` (`executor.py:575-628`) documents and implements forwarding for `rag` and `tool` only — there is no `react` branch. A `react` step's text is stored in `step_outputs` and read by nothing.
- The recorded force-search sample shows the react loop calling `web_search`, receiving `ok:true` with real weather rows, and the follow-up `llm_1` answering 「我目前无法直接访问互联网或实时搜索信息」 — the model contradicted data it was never given.
- The answer grader is a case-sensitive substring check. The observed failures contain 「无法获取」 and 「无法直接访问」; the paraphrase 「无法访问互联网」 occurs in neither and would be a vacuous assertion.
- `agent-web-tools` requires web tools to remain dispatchable from **both** `tool` and `react` steps. Anything this change does must narrow *when* `react` is chosen, not remove it.

## Goals / Non-Goals

**Goals:**

- Make the flat single-lookup shape reachable for the literal prompt that currently misses it.
- Ensure an answer is never written from a context that omitted the step that produced the data.
- Grade the answer, not just the plan shape.

**Non-Goals:**

- Removing or deprecating `react`.
- Changing what `web_search` returns, its bounds, or its error classification.
- Semantic routing — there is no intent classifier here; this is a prompt rule plus a data-flow fix.
- Touching the `agent-plan-shape-policy` requirement, which is carried by `agent-planner-write-routing`.

## Decisions

### 1. Fix the shape with a prompt rule, fix the grounding with the executor

Two independent defects, two fixes. The prompt rule makes `[web_search, llm]` reachable; the data-flow fix makes the answer honest whichever shape fires.

**Why both, not just the prompt rule:** if only the rule lands, the flat shape is chosen and the answer is grounded — the case goes green. But the react path remains reachable for any prompt the rule does not catch, and on that path the answer is still written from an empty context. The prompt rule reduces exposure; the data-flow fix removes it.

**Alternative considered — only the data-flow fix.** Rejected: the case grades plan shape, so a grounded `[react, llm]` answer would still fail the case while the plan is objectively worse (more model iterations for a single lookup).

### 2. Forward react output as its own context section

**Chosen:** add a `react` branch to `_render_prior_data`, rendering the step's text as a labelled section, subject to the existing `PRIOR_DATA_PER_SECTION_MAX_CHARS` and total cap.

The existing code already strips untrusted markers on the `tool` path and applies the caps uniformly, so the new branch reuses both rather than inventing a second bounding policy.

**Alternative considered — forward react tool results rather than its text.** Rejected: the react loop's tool results are internal to the loop and the model has already summarised them into its final text. Forwarding the raw results would push context the loop itself chose not to keep, and would risk re-inflating the prompt past the small-model window — the exact 2026-10-14 regression the caps exist to prevent.

### 3. The answer assertion targets the recorded strings

**Chosen:** forbid 「无法获取」 and 「无法直接访问」 on the force-search case.

**Verification obligation:** the assertion must be shown to fail on the recorded baseline sample and to pass on a search-grounded answer. An assertion whose keyword does not occur in the recorded failure is not coverage — this is the vacuous-assertion failure mode the suite's own rules exist to prevent.

## Risks / Trade-offs

- **The prompt rule over-fires and pushes genuinely multi-round requests into flat lookups** → `agent-web-tools` keeps `react` available and the rule reserves it for multi-round work; the corpus should carry a case that legitimately needs a loop so the preference is not tested in one direction only.
- **Forwarding react text re-inflates the answering step's prompt** → the existing caps apply to the new section the same way; watch the provider-400 regression the caps were introduced for.
- **The answer assertion becomes a new flake source** → it can only lower the rate, never raise it, and it is the only assertion that catches this defect. Accepted deliberately.
- **The flat shape may still be unreachable for this literal prompt** → open question below; if it is, the case expectation is the thing to revisit, not the model.

## Migration Plan

No wire change, no stored-data change. Rollback is reverting the rule string and the executor branch.

## Open Questions

- Whether `[web_search, llm]` is reachable at all for this literal prompt. The weather sibling proves the flat shape is reachable for a *different* string, and with one prompt per case there is no counterexample. Only a live run answers it, and it changes neither the specs nor the task breakdown — only what a non-improving run means.