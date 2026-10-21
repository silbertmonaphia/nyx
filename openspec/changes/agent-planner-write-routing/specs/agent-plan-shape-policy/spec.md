# Spec Delta

## Purpose

Governs which step kinds the planner must choose for a given request shape: which requests require a state-changing registry tool in the plan, and when a web lookup is expressed as a single search step rather than a nested loop. No existing capability owns this — `agent-planner-structured` governs how a plan is emitted and validated, not what it contains.

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

### Requirement: The write-tool mandate is scoped to explicit state-change intent

The write-tool mandate SHALL apply only when the user's message expresses an intent to change state. A request to compose, draft, review, suggest, or preview content — with or without an explicit instruction not to publish yet — SHALL NOT cause the planner to emit a state-changing `tool` step. Where a user's phrasing is ambiguous between composing and publishing, the planner MUST NOT resolve the ambiguity by writing; a request that is not clearly a state change is graded as a draft-and-report request.

#### Scenario: Explicit draft request does not plan a write

- **WHEN** the user asks the agent to draft a feed for review without publishing it
- **THEN** the emitted plan contains no `create_feed` `tool` step, and no feed is written

#### Scenario: Ambiguous compose phrasing is not resolved by writing

- **WHEN** the user's message could be read as either composing content or publishing it, and contains no clear state-change verb
- **THEN** the planner does not emit a state-changing `tool` step on the strength of the ambiguity alone

#### Scenario: Greeting and small talk stay single-step

- **WHEN** the user's message is a greeting or small talk requiring no retrieval or tools
- **THEN** the emitted plan is exactly one `llm` step, unchanged by the write-tool mandate

### Requirement: Single-lookup web requests plan one search step

When a user's message requests a single information lookup from the web, the emitted plan SHALL contain one `tool` step invoking `web_search`, followed by the answering step. A `react` step SHALL be reserved for requests that genuinely need multi-round interaction with the model (iterative refinement, branching follow-ups) and MUST NOT be used to wrap a single lookup. Where the web-search tool is unavailable in the active configuration, the expectation for the affected case is keyed to that mode and the search step is not planned.

#### Scenario: Explicit search request plans one search step

- **WHEN** the user asks a question and explicitly directs the agent to search the web
- **THEN** the emitted plan contains a `tool` step invoking `web_search` and does not contain a `react` step

#### Scenario: Multi-round requests may still use a react step

- **WHEN** a request needs iterative refinement across several model-tool round trips
- **THEN** a `react` step remains available and is not displaced by the single-lookup preference

#### Scenario: Search disabled removes the step rather than substituting a loop

- **WHEN** web search is disabled in the active agent configuration
- **THEN** the plan contains no `web_search` `tool` step and no `react` step standing in for one, and the case's expectation resolves to the mode that has no search step

### Requirement: A performed search is reported as performed

When a run's transcript contains a `web_search` `tool_call` whose `tool_result` returned results, the answer text assembled from the run's `delta` frames SHALL NOT deny that the search happened or claim the agent lacks internet access. The executor feeds the tool result into the answering step's context; the planner's step rules alone do not guarantee the answering step consumes it, so this is a graded property of the run, not an assumed consequence of planning.

#### Scenario: Answer reflects the search that ran

- **WHEN** a run's transcript shows a `web_search` `tool_call` returning results and the answer text states it cannot access the web or cannot retrieve the requested information
- **THEN** the run fails answer-content grading, and the report records both the successful `tool_result` and the contradicting answer text