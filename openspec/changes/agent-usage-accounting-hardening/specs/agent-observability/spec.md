# Spec Delta

## ADDED Requirements

### Requirement: Run token accounting is provider-derived and internally consistent

Every usage value the agent reports for a run — the `done` frame's `usage` object, the `agent_tokens_total` counter deltas, and `agent.runs.total_tokens` — SHALL derive from provider-reported usage, never from the flat per-call estimate. For every `done` frame, `total_tokens` SHALL equal `prompt_tokens + completion_tokens`, and the run's persisted `total_tokens` SHALL equal the `done` frame's `total_tokens`.

#### Scenario: Done frame usage is provider-reported

- **WHEN** a run's model calls return provider usage summing to 900 prompt and 300 completion tokens
- **THEN** the `done` frame carries `prompt_tokens=900`, `completion_tokens=300`, `total_tokens=1200`, and the run row's `total_tokens` is 1200

#### Scenario: Resume from a pre-upgrade checkpoint does not mix the estimate

- **WHEN** a run resumes from a checkpoint written before provider-usage accounting existed, and the resumed portion accrues provider-reported usage
- **THEN** the done frame reports only the provider-derived totals for the resumed portion, with `total_tokens = prompt_tokens + completion_tokens`; the pre-upgrade estimate is not included in any reported total

#### Scenario: Resumed runs still report subsequent provider usage

- **WHEN** a resumed run (pre-upgrade checkpoint) makes multiple model calls that each report usage
- **THEN** the done frame's totals equal the sum of all provider-reported usage accrued after the resume, and the usage object remains internally consistent
