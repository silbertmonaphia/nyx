# Spec Delta

## Purpose

Governs which step kinds the planner must choose for a given request shape: which requests require a state-changing registry tool in the plan, when a plan that omits one is rejected, and when a web lookup is expressed as a single search step rather than a nested loop. No existing capability owns this — `agent-planner-structured` governs how a plan is emitted and validated, not what it contains.

Plan-shape compliance is necessary but not sufficient: a plan can name the write tool while the executed call fails validation or a retry substitutes a read-only tool. Execution is graded by `agent-golden-eval`'s execution-effect requirement; this capability governs only the plan.

## ADDED Requirements

### Requirement: State-changing requests plan the matching write tool

When a user's most recent message requests a state change to their feeds — create, publish, update, or delete — the emitted plan SHALL contain a `tool` step invoking the matching registry tool by name, positioned before the answering step. An `llm` step composes text and MUST NOT stand in for the write: a plan that drafts feed-shaped prose as its final step without a corresponding `tool` step does not satisfy the request and MUST fail plan-shape regression grading for such a case. The write-tool mandate MUST NOT weaken the existing constraints that a plan ends with an `llm` step and that redundant steps are avoided.

#### Scenario: Publish request plans the create tool

- **WHEN** the user asks to summarise their recent feeds and publish the result as a new feed
- **THEN** the emitted plan contains a `tool` step invoking `create_feed`, followed by the answering step

#### Scenario: Drafting prose is not a substitute for the write

- **WHEN** a plan for a publish request contains no `tool` step and instead ends with an `llm` step whose prompt asks for feed-shaped prose
- **THEN** the plan is rejected by plan-shape grading for that case, and the report records the observed token sequence so the missing write step is visible

#### Scenario: Mutation siblings keep their existing shape

- **WHEN** the user asks to update or delete feeds rather than create one
- **THEN** the plan still contains the matching `update_feed` / `delete_feed` `tool` step, and the write-tool mandate adds no step these requests did not already require

### Requirement: A plan that omits a required write tool is rejected, not silently answered

When the latest user message expresses an explicit state change and the emitted plan contains no matching write tool, the planner pipeline SHALL reject the plan with a `plan_invalid` outcome so the existing retry loop re-prompts the planner with an actionable reason. It MUST NOT proceed to execute a draft-and-answer plan for that request, and it MUST NOT inject a synthetic write step whose arguments were never authored by the planner. The rejection SHALL be bounded by the existing retry budget; on exhaustion the run terminates with `plan_invalid` rather than silently downgrading to a draft.

#### Scenario: Missing write step triggers a planner retry

- **WHEN** the user asks to publish a feed and the parsed plan contains only `rag` and `llm` steps
- **THEN** the plan is rejected with `plan_invalid`, the retry prompt names the missing `create_feed` step, and the planner gets another attempt within its retry budget

#### Scenario: Exhausted retries fail closed

- **WHEN** the planner repeatedly omits the required write tool across every allowed attempt
- **THEN** the run terminates with `plan_invalid` and does not execute a draft-only answer

#### Scenario: No injection of an unauthored step

- **WHEN** the guard detects a missing write tool
- **THEN** it does not append a synthetic `create_feed` / `update_feed` / `delete_feed` step, because such a step would carry no valid `title` or `id` and would fail execution

#### Scenario: The guard does not fire on non-state-change requests

- **WHEN** the latest user message is a greeting, small talk, a refusal-style prompt, a question about the user's own data, or a web lookup
- **THEN** the absence of a write tool is not rejected and the plan proceeds under the existing rules

### Requirement: The write-tool mandate is scoped to explicit state-change intent

The write-tool mandate SHALL apply only when the user's message expresses an intent to change state. A request to compose, draft, review, suggest, or preview content — with or without an explicit instruction not to publish yet — SHALL NOT cause the planner to emit a state-changing `tool` step. Where a user's phrasing is ambiguous between composing and publishing, the planner MUST NOT resolve the ambiguity by writing; a request that is not clearly a state change is graded as a draft-and-report request. The deterministic rejection rule inherits this scope and SHALL NOT fire when a negation or draft qualifier is present.

#### Scenario: Explicit draft request does not plan a write

- **WHEN** the user asks the agent to draft a feed for review without publishing it
- **THEN** the emitted plan contains no `create_feed` `tool` step, and no feed is written

#### Scenario: Negated publish phrasing does not plan a write

- **WHEN** the user's message contains a publish verb together with a negation or deferral such as 先别发, 不要发布, or 仅供预览
- **THEN** the plan contains no state-changing `tool` step and the deterministic rejection rule does not fire

#### Scenario: Ambiguous compose phrasing is not resolved by writing

- **WHEN** the user's message could be read as either composing content or publishing it, and contains no clear state-change verb
- **THEN** the planner does not emit a state-changing `tool` step on the strength of the ambiguity alone

#### Scenario: Greeting and small talk stay single-step

- **WHEN** the user's message is a greeting or small talk requiring no retrieval or tools
- **THEN** the emitted plan is exactly one `llm` step, unchanged by the write-tool mandate

### Requirement: Single-lookup web requests plan one search step

When a user's message requests a single information lookup from the web, the emitted plan SHALL contain one `tool` step invoking `web_search`, followed by the answering step. A `react` step SHALL be reserved for requests that genuinely need multi-round interaction with the model (iterative refinement, branching follow-ups) and MUST NOT be used to wrap a single lookup. Where web search is unavailable in the active configuration, the plan SHALL contain no `web_search` step and no `react` step standing in for one.

#### Scenario: Explicit search request plans one search step

- **WHEN** the user asks a question and explicitly directs the agent to search the web
- **THEN** the emitted plan contains a `tool` step invoking `web_search` and does not contain a `react` step

#### Scenario: Multi-round requests may still use a react step

- **WHEN** a request needs iterative refinement across several model-tool round trips
- **THEN** a `react` step remains available and is not displaced by the single-lookup preference

#### Scenario: Search disabled removes the step rather than substituting a loop

- **WHEN** web search is disabled in the active agent configuration
- **THEN** the plan contains no `web_search` `tool` step and no `react` step standing in for one
