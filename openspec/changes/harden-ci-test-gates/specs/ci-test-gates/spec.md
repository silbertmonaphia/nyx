# Spec Delta

## Purpose

Defines the CI gates that keep the agent's full test suite, Go race detection,
agent type checking, and frontend/agent coverage floors executing on every
relevant change, so a green pipeline cannot hide a deleted test, a skipped
database suite, or a known-broken type annotation.

## ADDED Requirements

### Requirement: The full agent suite runs in CI

CI SHALL run the agent's complete pytest suite for every change under
`backend/agent_py/**` against a disposable pgvector Postgres named `nyx_test`,
after applying the alembic chain. The job SHALL set `TEST_ASYNC_PG_DSN` so
DB-backed tests execute, and MUST fail when a test skips because that variable
is unset, so a misconfigured DSN cannot make the gate vacuous. Live-network
tests SHALL self-skip; no model or external network is required.

#### Scenario: An agent regression blocks the merge

- **WHEN** a change under `backend/agent_py/**` breaks any test in the suite
- **THEN** the agent test job exits non-zero and the pull request is blocked

#### Scenario: DB-backed tests actually execute

- **WHEN** the agent test job runs with the disposable database configured
- **THEN** tests gated on `TEST_ASYNC_PG_DSN` execute rather than skip, and a
  skip attributed to the variable being unset fails the job

#### Scenario: Migrations are applied before the suite

- **WHEN** the test fixtures drop and recreate the `agent` and `langgraph`
  schemas
- **THEN** the alembic chain has already established the schema the suite
  expects, and the suite runs against the disposable database

#### Scenario: Live-network tests do not leak into the gate

- **WHEN** no live model endpoint is configured in the job
- **THEN** live-network tests self-skip and the job still gates on the offline
  suite

### Requirement: Go tests run under the race detector

CI SHALL execute the backend Go test suite with `-race`. The backend Makefile
SHALL expose a test target whose default invocation also enables `-race`, so a
local run matches the CI gate, with a documented opt-out for constrained
environments. A detected data race SHALL fail the gate.

#### Scenario: A data race fails the pipeline

- **WHEN** the Go suite reports a data race
- **THEN** the backend test job exits non-zero and the merge is blocked

#### Scenario: Local default matches CI

- **WHEN** a developer runs the backend Makefile test target with no overrides
- **THEN** the suite runs with `-race`

#### Scenario: The opt-out is explicit

- **WHEN** a developer sets the documented race opt-out variable
- **THEN** the suite runs without the race detector, and this is the only way
  the local target disables it

### Requirement: Agent type checking is a hard gate

The agent lint target SHALL run mypy over the agent package with the committed
configuration and MUST fail on any reported error; there SHALL be no baseline
file or regeneration target that accepts existing errors. The committed
configuration SHALL enable `check_untyped_defs`, `warn_unused_ignores`,
`no_implicit_optional`, and `strict_equality`. Ruff remains a zero-finding
gate alongside it.

#### Scenario: A new type error fails the build

- **WHEN** a change introduces any mypy error in `backend/agent_py/agent`
- **THEN** the agent lint job exits non-zero and the merge is blocked

#### Scenario: A stale suppression is rejected

- **WHEN** a change leaves a `type: ignore` that no longer suppresses an error
- **THEN** the unused-ignore diagnostic fails the gate

#### Scenario: No accept-the-baseline path remains

- **WHEN** the repository and Makefile are inspected after the change
- **THEN** the baseline file and its regeneration target are gone, and the lint
  output names the offending error directly

### Requirement: The frontend enforces a coverage floor

The `frontend-test` job SHALL run the vitest suite with coverage and MUST fail
when any configured threshold (statements, branches, functions, lines) is not
met. The thresholds SHALL be committed in the frontend test configuration, and
the coverage set SHALL include source files that no test imports, so adding an
untested module lowers the measured percentage rather than escaping the gate.

#### Scenario: Deleting tests trips the floor

- **WHEN** a change removes tests that cover source code and coverage falls
  below a configured threshold
- **THEN** the frontend test job exits non-zero and the merge is blocked

#### Scenario: An untested new module counts

- **WHEN** a change adds a source module under the configured coverage set
  without tests
- **THEN** that module is included in the coverage denominator, and a large
  enough addition fails the floor

#### Scenario: The current suite passes the floor

- **WHEN** the committed suite runs unchanged
- **THEN** coverage meets every configured threshold and the job proceeds

### Requirement: The agent enforces a coverage floor

The agent test job SHALL measure coverage over the agent package with
`pytest-cov` and MUST fail below a floor recorded in the repository. Coverage
SHALL be measured with the DB-backed tests executing, so the floor reflects the
suite the job actually gates.

#### Scenario: A coverage regression blocks the merge

- **WHEN** an agent change drops measured coverage below the recorded floor
- **THEN** the agent test job exits non-zero and the merge is blocked

#### Scenario: The floor is measured against the gated suite

- **WHEN** the agent test job computes coverage
- **THEN** DB-backed tests have executed, and their code is included in the
  measured percentage

#### Scenario: The floor is discoverable

- **WHEN** the gate fails
- **THEN** the failure output states the measured percentage, the configured
  floor, and where the floor is recorded, so the remedy does not require
  reading the workflow file
