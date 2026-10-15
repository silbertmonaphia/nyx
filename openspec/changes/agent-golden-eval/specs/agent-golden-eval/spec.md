# Spec Delta

## ADDED Requirements

### Requirement: The golden corpus is executable data

The eval SHALL read its cases from `backend/agent_py/eval/golden_cases.yaml`: each case carries a prompt and one or more mode entries, where a mode declares the planner-catalog shape it runs under (e.g. `web_search` on/off), the expected normalized step sequence, and a `min_rate` floor. Expected tokens MUST be validated at load time against the production catalog's step kinds and tool names; an unknown token is a load error, never a silently failing case.

#### Scenario: Unknown token is rejected at load

- **WHEN** a case's expected sequence names a tool that is not in the production catalog
- **THEN** loading fails with an error naming the case and the token, before any model call

#### Scenario: One prompt exercises two catalog shapes

- **WHEN** a case declares a `web_search: false` mode and a `web_search: true` mode
- **THEN** the runner samples the prompt once per mode, with expectations taken from the respective mode entry

### Requirement: The runner exercises the production planning path

The runner SHALL produce each sample by invoking the same plan-production path the graph's planner node uses, with a `Settings` instance constructed for the mode — never a re-implementation of prompt assembly or plan parsing. A test SHALL pin that the runner's callable is the one the planner node invokes.

#### Scenario: Harness cannot drift from production planning

- **WHEN** the planner node's plan-production entry point changes
- **THEN** the pinning test fails until the runner is repointed at the new entry

### Requirement: Verdicts are strict and order-significant

For each sample, the plan's normalized token sequence (step kind for `rag`/`llm`/`react`; tool name for `tool` steps) SHALL equal the expected sequence exactly — no extra steps, no missing steps, order significant. A failing sample SHALL record the raw plan so triage sees what was planned.

#### Scenario: Extra step fails the sample

- **WHEN** the model plans `[rag, web_search, llm]` against an expected `[rag, llm]`
- **THEN** the sample counts as failed and the recorded plan shows the extra step

#### Scenario: Auto-appended llm tail passes

- **WHEN** the model's plan omits the trailing answer step and the planner appends it
- **THEN** the sample's normalized tokens equal an expectation that includes the `llm` tail and the sample passes

### Requirement: The eval reports rates

Each case × mode result SHALL be a pass rate over N samples (serial execution; N configurable, default 3). The run SHALL exit non-zero when any case breaches its `min_rate` floor or the overall rate falls below the configured floor; a `--no-fail` mode SHALL report without the non-zero exit for trend runs. Sampling parameters SHALL be the production planner settings.

#### Scenario: Rate below the floor fails the run

- **WHEN** a case with `min_rate: 1.0` passes 2 of 3 samples
- **THEN** the run reports the 0.67 rate for that case and exits non-zero

#### Scenario: Trend run reports without failing

- **WHEN** the run is invoked with `--no-fail` and a case is under its floor
- **THEN** the report shows the breach and the process exits zero

### Requirement: Reports are durable artifacts

Each run SHALL write `report.json` (per-sample plans and tokens, per-case and overall rates, model id, git revision, start time) and a human-readable summary alongside it, under a UTC-stamped directory. The baseline run SHALL be committed as evidence.

#### Scenario: Report location is deterministic

- **WHEN** a run completes
- **THEN** `eval/reports/<UTC-stamp>/report.json` and `summary.md` exist and the summary names the model and revision

### Requirement: The eval is live-gated and never reddens CI

The runner SHALL require a reachable model stack, preflighting it with a clear unreachable message, and SHALL NOT be part of the default test collection. Modes whose prerequisites are unmet (e.g. web-on without a configured search key) SHALL be reported as skipped with the reason.

#### Scenario: Missing stack fails loudly, not silently

- **WHEN** the model endpoint is unreachable
- **THEN** the runner exits with a message naming the endpoint and writes no report

#### Scenario: Unmet mode prerequisite is skipped visibly

- **WHEN** a web-on mode runs with no search key configured
- **THEN** the report marks that mode skipped with the reason and the case verdict ignores it
