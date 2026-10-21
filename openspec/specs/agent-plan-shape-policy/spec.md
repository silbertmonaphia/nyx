# agent-plan-shape-policy Specification

## Purpose
Governs which step kinds the planner must choose for a given request shape: which requests require a state-changing registry tool in the plan, and when a plan that omits one is rejected. No existing capability owns this — `agent-planner-structured` governs how a plan is emitted and validated, not what it contains. The single-lookup web rule is added to this capability by `agent-web-single-lookup-routing`.

Plan-shape compliance is necessary but not sufficient: a plan can name the write tool while the executed call fails validation or a retry substitutes a read-only tool. Execution is graded by `agent-golden-eval`'s execution-effect requirement; this capability governs only the plan.

## Requirements

### Requirement: State-changing requests plan the matching write tool

When a user's most recent message requests a state change to their feeds — create, publish, update, or delete — the emitted plan SHALL contain a `tool` step invoking the matching registry tool by name, positioned before the answering step. An `llm` step composes text and MUST NOT stand in for the write: a plan that drafts feed-shaped prose as its final step without a corresponding `tool` step does not satisfy the request and MUST fail plan-shape regression grading for such a case. When the write's content must be composed from retrieved data, the plan SHALL include the composing `llm` step before the write, and the write step's arguments SHALL reference that step's output rather than a plan-time placeholder. The composing step's prompt SHALL keep the body within the write tool's declared content limit (for `create_feed`, a description of 1000 bytes, about 300 Chinese characters). The write-tool mandate MUST NOT weaken the existing constraints that a plan ends with an `llm` step and that redundant steps are avoided.

#### Scenario: Publish request plans the create tool

- **WHEN** the user asks to summarise their recent feeds and publish the result as a new feed
- **THEN** the emitted plan contains a `tool` step invoking `create_feed`, followed by the answering step

#### Scenario: Composed content is planned before the write

- **WHEN** the user asks to summarise retrieved feeds and publish the summary as a new feed
- **THEN** the emitted plan contains the composing `llm` step before `create_feed`, and `create_feed`'s content argument references that step's output rather than a plan-time placeholder

#### Scenario: Drafting prose is not a substitute for the write

- **WHEN** a plan for a publish request contains no `tool` step and instead ends with an `llm` step whose prompt asks for feed-shaped prose
- **THEN** the plan is rejected by plan-shape grading for that case, and the report records the observed token sequence so the missing write step is visible

#### Scenario: Mutation siblings keep their existing shape

- **WHEN** the user asks to update or delete feeds rather than create one
- **THEN** the plan still contains the matching mutation `tool` step (`update_feed` / `delete_feed`, or the bulk `update_feeds` / `delete_feeds` once that sibling lands), and the write-tool mandate adds no step these requests did not already require

### Requirement: A plan that omits a required write tool is rejected, not silently answered

When the latest user message expresses an explicit state change, the emitted plan contains no matching write tool, and the plan's final model-authored `llm` step asks to compose feed-shaped text, the planner pipeline SHALL reject the plan with a `plan_invalid` outcome so the existing retry loop re-prompts the planner with an actionable reason. It MUST NOT proceed to execute a draft-and-answer plan for that request, and it MUST NOT inject a synthetic write step whose arguments were never authored by the planner. The rejection SHALL be bounded by the existing retry budget; on exhaustion the run terminates with `plan_invalid` rather than silently downgrading to a draft. The final-step composition signal keeps read/summary plans from being rejected; a state-change plan that omits both the write and the composition signal still fails plan-shape regression for its corpus case. The deterministic trigger uses a fixed lexical cue set; this change ships Chinese cues only, so English state-change phrasing is not guard-triggered and relies on the planner prompt until the cue set is extended.

#### Scenario: Missing write step triggers a planner retry

- **WHEN** the user asks to publish a feed, the parsed plan contains only `rag` and `llm` steps, and the final `llm` prompt asks to compose feed-shaped text
- **THEN** the plan is rejected with `plan_invalid`, the retry prompt names the missing `create_feed` step, and the planner gets another attempt within its retry budget

#### Scenario: Exhausted retries fail closed

- **WHEN** the planner repeatedly omits the required write tool for a composition-shaped state-change plan across every allowed attempt
- **THEN** the run terminates with `plan_invalid` and does not execute a draft-only answer

#### Scenario: No injection of an unauthored step

- **WHEN** the guard detects a missing write tool
- **THEN** it does not append a synthetic `create_feed` / `update_feed` / `delete_feed` step, because such a step would carry no valid `title` or `id` and would fail execution

#### Scenario: Write without the composing step is rejected

- **WHEN** a create/publish plan contains `create_feed` but no preceding composing `llm` step, or the write args contain no reference to that step
- **THEN** the plan is rejected with `plan_invalid` and the retry prompt names the missing composing step or reference, so a placeholder feed cannot be published through the runtime path

#### Scenario: The guard does not fire on non-state-change requests

- **WHEN** the latest user message is a greeting, small talk, a refusal-style prompt, a question about the user's own data, or a web lookup
- **THEN** the absence of a write tool is not rejected and the plan proceeds under the existing rules

#### Scenario: Read or summary request does not trigger the guard

- **WHEN** the user asks to look up or summarise their recent feeds without asking to publish, update, or delete one (for example 帮我查最近我发了什么 or 总结我最近发的feed)
- **THEN** a plan that omits a write tool is not rejected, and the read/summary plan is graded under the existing rules

#### Scenario: State change without a composition signal is not guard-rejected

- **WHEN** the user asks to publish a feed, the plan has no `create_feed`, and the final `llm` prompt is a generic answer rather than a feed composition
- **THEN** the guard does not reject the plan; the case fails plan-shape regression because the expected write step is absent

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
