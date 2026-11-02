# Spec Delta

## Purpose

Replaces single-turn chat with a Plan-and-Execute agent loop that streams per-step progress to the client while preserving the existing chat SSE wire contract. **The implementation moved to Python LangGraph** (see OpenSpec change `agent-langgraph`); this spec stays as the wire contract the chat handler and frontend observe.

## MODIFIED Requirements

### Requirement: Step kinds cover the agent surface

The agent loop SHALL support at least these step kinds: `rag` (vector retrieval over feeds), `kb` (vector retrieval over the caller's knowledge corpus), `tool` (registry dispatch), `llm` (single model call), and `react` (nested model-tool-model loop with iteration cap). The planner MAY emit any combination of these kinds.

#### Scenario: Planner emits a mixed plan

- **WHEN** the planner produces steps of kinds `[rag, tool, llm, tool]`
- **THEN** the executor runs them in that order, each emits its own `step_start`/`step_end` pair, and tools dispatched match the registry

#### Scenario: Nested ReAct caps iterations

- **WHEN** a step of kind `react` runs and the model emits tool calls
- **THEN** the loop iterates model → tool → model until either the model emits a final answer OR the configured iteration cap is hit; exceeding the cap emits `step_failed` with reason `react_max_iterations`

#### Scenario: A knowledge step runs against the knowledge corpus

- **WHEN** the planner produces a step of kind `kb`
- **THEN** the executor performs a knowledge-corpus retrieval for that step, emits the step's own `step_start`/`step_end` pair, and the retrieved context is carried forward to the answering step

## ADDED Requirements

### Requirement: A knowledge step carries its query and its optional scope

A step of kind `kb` SHALL carry a query string, and MAY carry a source identifier and a relative-path prefix that narrow the search. The executor SHALL pass the query and any scope to the backend retrieval. A knowledge step whose retrieval fails SHALL be handled as the retrieval step already is: the step records an unavailable outcome, the run continues, and the failure never surfaces an internal error string to the model.

#### Scenario: Scoped knowledge step

- **WHEN** a `kb` step carries a query, a source identifier, and a path prefix
- **THEN** the executor requests the knowledge corpus restricted to that source and that path prefix

#### Scenario: Unscoped knowledge step

- **WHEN** a `kb` step carries only a query
- **THEN** the executor requests the whole visible knowledge corpus for the caller

#### Scenario: Unavailable retrieval does not fail the run

- **WHEN** the knowledge retrieval is unavailable
- **THEN** the step records an empty result, the run continues to the answering step, and no internal error detail reaches the model
