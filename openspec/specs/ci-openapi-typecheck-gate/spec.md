# ci-openapi-typecheck-gate Specification

## Purpose
Keeps the SPA's generated OpenAPI types (`frontend/src/api/openapi/schema.d.ts`)
honest: the committed artifact either matches `api/openapi.json` or the merge
is blocked, so the typed client can never describe an older API than the
server ships.

## Requirements

### Requirement: Committed frontend OpenAPI types must match the spec

The `frontend-test` job MUST regenerate the frontend types from
`api/openapi.json` and compare them with the committed
`frontend/src/api/openapi/schema.d.ts`. Any difference MUST fail the job.
The comparison MUST use the same generator the developer command uses
(`npm run openapi:gen` / `npm run openapi:check`), so a locally regenerated
and committed file always satisfies the gate.

#### Scenario: Drifted types fail the merge

- **WHEN** a PR or push changes `api/openapi.json` (or a handler that feeds
  it) without regenerating and committing the frontend types — or changes
  the types without the spec
- **THEN** the `frontend-test` job exits non-zero, and the job output shows
  the differing lines so the author can see what the spec generates

#### Scenario: In-sync types pass

- **WHEN** the committed `schema.d.ts` is exactly what `api/openapi.json`
  generates
- **THEN** the check passes and the job proceeds to its remaining steps

#### Scenario: The remedy is discoverable from the failure

- **WHEN** the check fails
- **THEN** the failure output names the stale artifact and the regeneration
  command (`npm run openapi:gen`), so the fix does not require reading the
  workflow file

### Requirement: The gate reuses the existing frontend toolchain

The gate MUST run inside the existing `frontend-test` job on the same events
as the rest of CI (every push to `main`/`mvp` and every PR), using the
dependency set the job already installs. It MUST NOT add an npm dependency,
change the lockfile, or introduce a new action pin.

#### Scenario: No new dependency or pin

- **WHEN** the workflow and `frontend/package.json` are inspected after the
  change
- **THEN** the only workflow delta is the new step, every `uses:` line is
  unchanged, and `frontend/package-lock.json` is untouched

#### Scenario: Runs on push and PR

- **WHEN** a commit is pushed to `main`/`mvp`, or a PR is opened against
  either
- **THEN** the drift check runs as part of `frontend-test` in the resulting
  workflow
