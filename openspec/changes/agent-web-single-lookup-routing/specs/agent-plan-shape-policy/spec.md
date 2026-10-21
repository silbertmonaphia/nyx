# Spec Delta

## ADDED Requirements

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
