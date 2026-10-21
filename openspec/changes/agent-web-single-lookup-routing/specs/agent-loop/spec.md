# Spec Delta

## ADDED Requirements

### Requirement: A completed react step's output reaches later steps

A completed `react` step's user-visible text SHALL be carried into the context of later steps in the same plan, alongside the `rag` and `tool` outputs already forwarded, subject to the same per-section and total character bounds. A plan that contains a `react` step followed by an answering step therefore lets that step's answer be written from what the react loop actually found, rather than from a context that silently omits it. Forwarding react output MUST NOT change the frames the client receives, and MUST NOT displace an earlier data step under the caps in a way that silently drops the react text.

#### Scenario: The answer step sees what the react loop found

- **WHEN** a plan is `[react, llm]` and the react loop completed with text reporting retrieved weather
- **THEN** the `llm` step's context includes that text, and its answer can be grounded in it

#### Scenario: A react loop that retrieved nothing is still forwarded

- **WHEN** a `react` step completes with text saying it could not retrieve the requested information
- **THEN** that text is forwarded, so the answering step is grounded in the failure rather than free to assert a different outcome

#### Scenario: Caps still bound the forwarded context

- **WHEN** several data steps precede the answering step and their combined output exceeds the total bound
- **THEN** the oldest sections are dropped in the documented order, and the result is explicitly truncated rather than silently incomplete

#### Scenario: Client frames are unchanged

- **WHEN** react output is forwarded into a later step's context
- **THEN** the `delta` frames the client received from the react step, and their order, are exactly what they were before the change