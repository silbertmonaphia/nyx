# Spec Delta

## ADDED Requirements

### Requirement: Tracing state and export failures are operator-visible under default logging

With tracing enabled, the agent process SHALL, under its default logging configuration (no `LANGFUSE_DEBUG`, default environment), emit: a startup line at INFO naming the configured host when the tracing client is constructed; and WARN-or-above diagnostics for failed export batches, naming the host when the transport reports it (the startup line always carries it) and stating the retry/drop outcome. When exports succeed, the process SHALL NOT emit per-flush diagnostic noise beyond the startup line. When tracing is disabled, the process SHALL emit no Langfuse-related lines. These diagnostics MUST NOT alter the SSE stream or run outcomes — the best-effort delivery contract is unchanged.

#### Scenario: Startup announces the enabled state

- **WHEN** the agent boots with `PYTHON_AGENT_LANGFUSE_ENABLED=true` and a configured host, under the default log configuration
- **THEN** the process log contains a timestamped INFO line naming the host, before any run executes

#### Scenario: Unreachable host is visible within one flush cycle

- **WHEN** the configured host refuses connections and a run produces spans
- **THEN** the process log shows WARN-or-above diagnostics for the export failure without `LANGFUSE_DEBUG` (they name the host when the transport reports it; the startup line always carries it), and the run's SSE frames are identical to a tracing-disabled run

#### Scenario: Healthy tracing stays quiet

- **WHEN** exports succeed
- **THEN** no per-flush diagnostic lines appear (the startup line is the only tracing output)

#### Scenario: Disabled tracing logs nothing

- **WHEN** `PYTHON_AGENT_LANGFUSE_ENABLED` is false or unset
- **THEN** no Langfuse-related lines appear in the process log
