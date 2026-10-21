# Design

## Context

See `proposal.md` — Why for motivation. The technical facts that shape the approach:

- The plan is produced by **one LLM call** and nothing downstream corrects its content. `PLANNER_SYSTEM_PROMPT` (`backend/agent_py/agent/planner.py:66-85`) is the policy surface. `_enforce_plan_shape` (`:641-700`) does three things — empty-plan fallback, `plan_max_steps` rejection, duplicate-id rejection — plus appending a trailing `llm` when the plan ends on a data step. It has no rule that can add a missing tool, and it does not receive the user's message. There is no router node in the graph; `graph.py` is a straight `plan_node → executor_node`.
- The structured path calls `_enforce_plan_shape` inside the retry loop (`planner.py:443`); returning `_PlanError("plan_invalid", ...)` appends the reason to the planner messages (`PREVIOUS_ATTEMPT_FAILED_HEADER`) and re-invokes up to `planner_retry_max`. The **legacy free-text path does not call `_enforce_plan_shape`** — `_parse_and_validate` duplicates the enrichment inline — and has no retry (`:555-557`). The comment at `:20`/`:523` saying both paths call the shared enforcer is stale.
- Ruled out as causes for the three reds: step cap (`plan_max_steps=10`, observed 2/2/3), mode switches (none in the planner), schema restrictions (`PlanStep.tool` is a free-form `str`), parser drops (the raw plan frame already lacks `create_feed`; no retry fired, no `plan_invalid`), and env toggles (`EVAL_WEB_SEARCH=on` at baseline; `budget_stack` does not affect plan shape because the `plan` frame is emitted before execution).
- The tool catalog is already advertised correctly. `registry_planner_block` (`backend/agent_py/agent/tool_dispatch.py:216-239`) renders `- {name}: {desc}` per tool; `create_feed` is registered at `backend/internal/mcp/service.go:141`. The model is never denied the tool — it is never *required* to use it.
- **Plan-shape green does not imply execution.** `_run_tool` executes the plan's static `step.args` (`executor.py:277`); there is no templating from earlier step outputs. `_render_prior_data` (`:575-628`) feeds earlier `rag`/`tool` outputs into `llm` prompts only. In the committed baseline, all three `main-rag-update-feed` and all three `main-rag-delete-feed` samples call the write tool with invalid args (`update_feed` id is the string `"feed_id_from_list"`; `delete_feed` id is missing), the retry substitutes `list_feeds` (read-only, ignored by the security grader), and no write occurs — yet both cases score 3/3 because `evals/graders/plan_shape.py` reads only the `plan` frame. `create_feed` accepts a planner-authored `title`/optional `description`, so a shape-only fix would publish a placeholder feed and still score green.
- **React output is not carried forward.** A `react` step stores its final text in `step_outputs`; `_render_prior_data` only forwards `rag` and `tool` outputs, not `react`. The force-search case's final `llm` step therefore sees none of the `web_search` results that ran inside the react loop. This is the mechanism behind 「我无法直接访问互联网」 in sample 16.
- **The answer grader is a case-sensitive substring check** (`evals/graders/answer.py`: `text in answer`). The observed failure text is 「我目前**无法获取**…」 and 「我**无法直接访问**互联网…」; the paraphrase `无法访问互联网` does not match either.
- Baseline evidence is stale. Last full run 2026-10-16 at rev `e6d92db`; 92 commits to HEAD, 14 touching `backend/agent_py/`, none touching plan shape. The newest artifact on disk (`20261016T143912Z`) reads 1.00 off a 3-sample partial that exercised only the budget case (floor 0.7, all other cases NOT RUN).
- The grader is not the cause of the three reds, but plan-shape-only grading *is* the cause of the hollow update/delete greens. `evals/graders/plan_shape.py` compares tool-name tokens under strict ordered equality with one trailing `llm` normalised on both sides (`:62-66`, `:132`); there is deliberately no `react`↔`tool` aliasing, and broadening it would defeat the force-search case's stated purpose.

## Goals / Non-Goals

**Goals:**

- Move the three reds with the smallest change that addresses the actual mechanism (prompt policy plus a narrow deterministic guard), and bound that change so it cannot fire where it should not.
- Make the suite able to prove the write happened — plan shape alone is demonstrably insufficient.
- Deliver the write-content mechanism (arg templating, Decision 4) so a write carries composed content and real identifiers, rather than the plan-time placeholder the planner cannot do without.
- Leave the suite able to catch this class of defect again without a human remembering to run it.

**Non-Goals:**

- A general intent classifier in `_enforce_plan_shape`. A narrow, explicitly-triggered write-tool guard is in scope; broad verb/intent detection is not.
- Relaxing the corpus expectations by default (reviewer decision point; see proposal).
- A scheduled/nightly live eval run (see Decision 6).
- Corpus growth beyond the write/execution cases and the draft/negation canary; LLM-as-judge.
- Rewriting the planner into a router or a multi-call architecture.

## Decisions

### 1. Fix plan shape in the prompt, with a narrow deterministic retry guard as defence in depth

**Chosen:** add the rules to `PLANNER_SYSTEM_PROMPT` and add a narrow guard to the shared post-parse path. The prompt is the primary policy; the guard is the fallback for 4B non-compliance.

The guard fires only when all of the following hold:

- the latest user message contains an explicit state-change verb (发出去 / 发布 / 创建 / 更新 / 删除 / 形成…发出去), and
- the message contains no negation or draft qualifier (别发 / 不要发 / 先不发 / 草稿 / 仅供预览), and
- the plan contains no matching write tool (`create_feed` / `update_feed` / `delete_feed`), and
- the plan ends with an `llm` step that answers the request, per the existing shape rule.

On a match it returns `_PlanError("plan_invalid", ...)` with an actionable reason; the existing retry loop re-prompts. It does **not** inject a synthetic step: an injected `create_feed` has no valid `title` and an injected `update_feed`/`delete_feed` has no valid `id`, so injection would guarantee a schema failure and then a retry that can substitute a different tool.

**Why this revises the earlier rejection:** the original design rejected deterministic repair because it would encode a second, silent copy of the intent judgement. That reasoning still applies to a *general* intent classifier; it does not apply to this deliberately narrow trigger, which the earlier revision already kept in reserve as a fallback. Prompt-only is unreliable on a 4B; the guard is the only deterministic way to keep the omission from being silently accepted.

**Implementation constraints:** pass the latest user message into the post-parse path (both the structured call site and, if the legacy path is kept, the legacy parser — or document that the guard is structured-only). The legacy path currently has no retry, so a `plan_invalid` there terminates the run; the structured path is the default. Unit-test the trigger matrix: publish, draft, negation, greeting/refusal, update/delete, ambiguous compose.

**Alternative considered — inject the step:** rejected for the args reason above. **Alternative considered — prompt only:** rejected because it is unverifiable and 4B compliance is not established.

### 2. Bound the write-tool mandate with an explicit draft carve-out

The obvious failure mode of a "must plan the write tool" rule is over-firing: a draft request, a greeting, or an ambiguous compose request acquiring a `create_feed` step. The spec requirement "The write-tool mandate is scoped to explicit state-change intent" exists to make this testable, and the corpus must carry a draft/negation case, not just the greeting/refusal canaries. The new rule must be worded narrowly enough not to collide with the existing `when unsure, prefer a single llm step` rule (`planner.py:84`); the carve-out and that rule have to be reconciled in the same edit, not sequentially.

### 3. Grade execution for write cases; plan shape is not acceptance

**Chosen:** every case that expects a write tool asserts the transcript's `tool_call`/`tool_result` frames in addition to the plan. The named write tool must be called under the expected step, and its `tool_result.ok` must be true. Where the case is about a specific feed, assert the DB effect through the eval user's API; for the create cases, assert the created feed's content reflects the retrieved fixtures rather than a plan-time placeholder.

**Why:** the baseline's update/delete 3/3 shows the exact failure mode a shape-only assertion misses — the plan names `update_feed`, the executed call fails validation, the retry calls `list_feeds`, and the run scores green. A create-feed plan with a planner-authored title will execute successfully and publish the wrong content; only a content/DB assertion distinguishes that from the requested summary.

**Consequence to accept:** adding these assertions will probably turn the currently-green update/delete siblings red. That is the defect becoming visible. The reviewer must choose whether to fix their args/id mechanism in this change or record them red with operator sign-off.

**Non-goal:** an LLM judge. All execution assertions are deterministic frame/API checks.

### 4. Deliver arg templating so a write can carry content and real identifiers (decided: option A)

**Decision: A — arg templating.** Operator decision, 2026-10-22.

The strict `[rag, create_feed, llm]` shape writes before any composition step exists, and the planner cannot see rag results at plan time. The compose-then-write variant `[rag, llm, create_feed, llm]` is the correct shape but is unimplementable while tool args are static (`executor.py:277`).

The executor resolves an earlier step's output into a `tool` step's args at execution time. The corpus moves `main-rag-create-feed` to `[rag, llm, create_feed, llm]` and `main-all-tools-combined` to `[rag, web_search, llm, create_feed, llm]` — both need composed content before the write, and the combined chain keeps its `web_search` step.

**This is not create-feed content polish — it is the only path to working mutation at all.** The verified baseline shows `update_feed` called with `id: "feed_id_from_list"` and `delete_feed` called with `{"feed_id": "list_feeds_3months"}`: a placeholder the planner cannot avoid emitting, because a 4B at plan time has no way to know a real numeric feed id. No prompt wording fixes that. Templating is what lets `update_feed`/`delete_feed` receive a real identifier from an earlier `list_feeds` step, so their planned writes become executable rather than permanently schema-rejected.

**Why not B (react-hosted write):** B requires the 4B to read a tool result and correctly place a real value into a later tool call's args. That is the same failure mode already observed on the force-search case, where the react loop received real `web_search` results and the final `llm` step answered 「我无法直接访问互联网」 — the model's handling of tool-result-to-next-step context is the least reliable part of this pipeline. A resolves the value deterministically at execution time and does not depend on the model getting that right.

**Why not C (plan-shape-only):** C would document that feed mutation is broken and ship. With A rejected, `create_feed`, `update_feed`, and `delete_feed` all stay non-functional while the suite grades them on plan shape alone — the hollow-green pattern this change exists to close.

**Cost accepted:** `executor.py` gains a resolution pass, and referenced values originate from untrusted user content (retrieved feed text, fetched pages). This is the real cost of a real fix — see the injection-surface risk below and the `agent-loop` delta, which requires untrusted-content handling on resolved argument values at the dispatch boundary.

### 4b. Bulk write tools select their targets by query, not by id (decided)

**Decision: query-based selection.** Operator decision, 2026-10-22. `update_feeds(query, title, description?)` and `delete_feeds(query)`.

**Why query and not ids:** the planner can author a query — it has the user's own words ("负面情绪的feed") — and it cannot author an id, because real feed ids only exist after execution. A query-based tool's arguments are therefore expressible at plan time; id-based ones are not. This is the same reason `"feed_id_from_list"` could never be fixed by prompt wording, and it is the deciding factor.

**Why not ids (the alternative considered):** ids force the selection back onto the model — an `llm` step picks the id list and templating carries it in. That is the 4B making a correctness-critical decision, which is precisely the failure mode already demonstrated twice (placeholder ids; react results discarded). The server deciding is the point.

**Inherited semantics that must be designed around, not discovered later:** `list_feeds`'s `query` is `ILIKE '%' || query || '%'` over title and description (`backend/queries/feeds.sql:20-22`). A bulk write tool inherits that: it is a literal substring match, not semantic. `delete_feeds(query="的")` matches most of a user's feeds. Three consequences the implementation MUST carry:

- a count cap — the tool refuses when the match exceeds a configured maximum rather than mutating an unbounded set;
- a preview before approval — the confirm gate shows how many feeds match and a sample of their titles, so the user is not approving an invisible blast radius;
- the exact matched set returned in `tool_result` — this is what lets the answering step report honestly instead of confabulating, which is the defect that produced the baseline's false-success answers.

Feeds are soft-deleted (`feeds.deleted_at`), so a mistaken bulk delete is recoverable; that is a mitigation, not a substitute for the cap and the preview.

**Cross-user safety is already structural:** every query filters on `user_id` from the request context, never from arguments (`backend/queries/feeds.sql:11` and the `agent-tool-registry` per-owner scoping requirement). A query argument selects among the caller's own feeds; it cannot reach another user's rows. The new trust boundary is not user scope, it is *match breadth*.

**Where it lands:** a Go-only change (`internal/tool/feeds.go`, the registry, the MCP shim, `agent-tool-registry` + `agent-mcp-tools` spec deltas, and the destructive-tool allowlists on both sides of the wire). It has no implementation dependency on the planner work in this change, but the eval execution assertions in this change depend on it — see tasks 5.4 and the split note in the proposal.

### 5. Re-measure before and after, and treat a non-improving run as a result

Ordering is load-bearing: a clean baseline at current HEAD must land before the first prompt edit, because the 0.73 figure is five days old and the most recent artifact is a false green. Optimising against an unconfirmed number repeats the mistake this change exists to fix. The baseline needs two runs: the normal stack (11 cases graded, `main-budget-exceeded` skipped) and the small-budget side stack (the budget case). A single normal-stack run cannot re-derive the budget floor.

**Sequence a case's fix before that case's assertion, but do not delay an honest red just because the fix is in another change.** The create fix lands here, so the create-case assertions land here. The update/delete fix is the bulk-tool sibling, so their assertions land here and deliberately record the known-unfixed defect as red; the sibling flips it green. Tightening update/delete first without recording the red would leave the hollow green in place, which is the failure this change exists to close.

A confirming run that does **not** improve the rate is a valid outcome and gets recorded as one, with the observed plans attached. The failure mode to avoid is iterating prompt wording until the number moves and landing on a change that scores well for reasons nobody can explain. Two runs (before/after), not a search.

### 6. Wire the deterministic unit gate into CI; spike — do not build — the scheduled live run

The unit gate (`pytest evals/tests`) needs no model and no GPU. It is explicitly named as new scope by the existing `agent-golden-eval` "Live-gated execution posture" requirement, so it is unambiguous scope. Add it to the `agent-lint` job and extend that job's path filter to `evals/**`; verify the gate actually goes red on a deliberately failing test before trusting it.

A scheduled live run is a different proposition: it needs a GPU runner (GitHub-hosted runners have none), and a nightly that fails on runner provisioning or vLLM warm-up trains people to ignore the signal. Tasks therefore carry a **feasibility spike** whose output is a written recommendation plus a documented manual runbook fallback — not a workflow file.

### 7. Track open reds in the corpus, not in a standalone bug log

`bugs.md` was deleted on 2026-10-19 with three open reds inside it. Reinstating a separate tracker invites the same drift. The corpus case notes are already the durable home — they are versioned with the expectation, they travel with the case, and they cannot fall out of sync with the floor. Restore the diagnostic text into `evals/cases/main.yaml` notes and leave no standalone tracker. The deleted log also held a security finding; that item is owned by `agent-tool-policy-gate` and should be cross-referenced, not duplicated here.

### 8. Add only the missing floor provenance

`check_rate.py` already records `args.min_rate` as the report's overall floor and renders it in `summary.md` (`:368,373`, `:523`); the `20261016T143912Z` false green did record floor 0.7. The missing piece is the **baseline rate the floor was derived from** and a guard against treating a partial run as a baseline. Scope the report change to those two, rather than re-implementing the applied-floor recording that already works.

### 9. Prefer a flat web_search step for single lookups, and assert the answer (deferred)

**Deferred to `agent-web-single-lookup-routing`; recorded here only as the design context that motivated the split.** Extend `_PLANNER_RULE` to prefer one `web_search` `tool` step and reserve `react` for multi-round work. The answer assertion targets the exact observed denial phrases (`无法获取`, `无法直接访问`), because the grader is a case-sensitive substring check. Separately, either fix `_render_prior_data` to carry react-hosted tool results into the next `llm` step, or state in the spec that the "search performed" guarantee applies to `tool`-hosted searches only. The current requirement's claim that "the executor feeds the tool result into the answering step's context" is false for react-hosted searches.

## Risks / Trade-offs

- **Over-firing the write-tool guard** → narrow trigger, negation/draft carve-out, a dedicated draft case, and the existing greeting/refusal canaries; a re-run that shows new reds in those cases is the tripwire.
- **Plan-shape green is mistaken for acceptance again** → execution assertions are a required acceptance gate; a plan-only green is explicitly not a fix.
- **The prompt fix simply does not flip the 4B** → recorded as an outcome, not tuned around; the deterministic guard becomes the live fallback.
- **Resolved arguments carry untrusted user content into a write path** → a referenced `rag` or `tool` output is retrieved feed text or a fetched page, i.e. attacker-influenceable input reaching `create_feed`/`update_feed`/`delete_feed` arguments. Mitigation is normative in the `agent-loop` delta: untrusted-content markers are stripped from resolved values at the dispatch boundary, matching what `_strip_untrusted_markers` already does on the prompt path. The security review of the resolution pass is a task, not an afterthought.
- **Reference syntax collides with literal user content** → if a value a user typed into a feed body contains the reference marker, a later tool step could resolve against it. Mitigation: references resolve only against known step identifiers in the current plan, and an unknown or malformed reference fails the step closed rather than passing the text through.
- **Corpus expectations move** → `main-rag-create-feed` shifts from `[rag, create_feed, llm]` to `[rag, llm, create_feed, llm]`, and `main-all-tools-combined` to `[rag, web_search, llm, create_feed, llm]`, which re-opens floors the suite previously graded as settled. The change is deliberate and operator-decided; the re-opened floors need the same operator sign-off as any other floor edit.
- **Templating does not by itself make update/delete green** → it makes the identifier resolvable, but the case still needs its fixtures seeded (see the fixture-seeding risk) and its answer asserted (the hallucinated-success risk).
- **Execution assertions expose the update/delete siblings** → treat the new red as the defect becoming visible; choose fix-or-record-red with operator sign-off.
- **Answer assertions over- or under-constrain** → target the observed denial phrases, not broad vocabulary; verify the assertion fails on the recorded sample 16 answer and passes on a search-grounded answer.
- **The unit-gate CI job is agent-env dependent** → it runs under `backend/agent_py/.venv` like the existing agent lint job; if the venv is not provisioned on the runner, that job fails loudly at setup rather than silently skipping the gate.
- **Budget case cannot be measured on the normal stack** → run the budget side stack separately and record both baselines; do not mark the budget task complete from a skipped case.
- **The agent reports a failed write as a success** → verified in the committed baseline: with zero writes executed, `update_feed` answered 「你近3个月的负面情绪feed已全部转化为积极情绪feed」 and `delete_feed` answered 「已根据你的要求，删除了近3个月所有关于负面情绪的feed」, both inventing before/after titles. Neither case carries an answer assertion, so this ships. This is the most user-damaging defect in the baseline and it is separate from the three original reds — execution-effect grading (Decision 3) catches the failed write, but only an answer assertion catches the lie.
- **Fixture seeding may be broken** → in the committed baseline the executor's `list_feeds` retry for the mood fixtures returned `total: 0` despite three declared fixtures (`eval-main-mood-stressed` / `-anxious` / `-happy`). Either seeding does not land or the query does not match, and plan-shape grading hides it because the plan is fixed before retrieval runs. Diagnose before relying on any update/delete case's retrieval as real.

## Migration Plan

No schema, no wire-format, no stored-data change. Arg templating adds an internal plan-arg syntax with no client-visible frame change; a `tool_call` frame carries the same resolved arguments it would have carried with the reference absent. Rollback is a revert of the prompt strings, the guard, the executor resolution pass, the corpus edits, the execution grader, and the CI step; the eval suite is additive and off by default.

## Open Questions
- Whether the flat `[web_search, llm]` shape is reachable for the force-search case's literal prompt. The weather sibling proves flat is reachable for a *different* string; with one prompt per case there is no counterexample. Resolvable only by running it.
- Whether `main-budget-exceeded`'s observed 2/3-then-3/3 flakiness is reproducible. Two runs five minutes apart disagree; the trip point depends on real token spend. The budget-stack baseline resolves it.
- Whether the deterministic guard should also cover the legacy free-text path, or whether that path is deprecated enough to leave structured-only.
