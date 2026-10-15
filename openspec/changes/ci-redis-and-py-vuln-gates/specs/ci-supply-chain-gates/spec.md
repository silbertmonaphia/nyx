# Spec Delta

## Purpose

Tightens the merge gate for two supply-chain surfaces that are currently
under-covered: the Go cache layer's real-Redis path (today only exercised
against `miniredis`), and the Python agent's dependency graph (today
uncovered). Both gates run in CI on every push to `main` / `mvp` and on
every PR, so a regression fails the merge rather than reaches production.

## ADDED Requirements

### Requirement: Real-Redis cache integration test gate

The `backend-test` job MUST run a real-Redis integration suite against a
testcontainers-managed Redis 7 instance whenever `SKIP_CONTAINERS` is not
`true` and the `DB_URL` testcontainer is available. The suite MUST assert
the cache layer's behavior on surfaces that `miniredis` cannot faithfully
emulate (TCP reconnect, real TTL timing, `SCAN+UNLINK` under load,
`noeviction` memory pressure, `PING` failure fallback).

#### Scenario: TTL expiry timing
- **WHEN** a cache entry is written with a 200ms TTL and 250ms elapse
- **THEN** a subsequent `Get` returns the cache-miss sentinel, not the
  stored value

#### Scenario: DeletePrefix clears the full key namespace
- **WHEN** keys `feeds:q=a:p=1:s=20`, `feeds:q=a:p=2:s=20`, and
  `other:1` are present and `DeletePrefix("feeds:")` is called
- **THEN** the two `feeds:*` keys are removed and `other:1` is preserved

#### Scenario: PING failure falls back without crashing
- **WHEN** the Redis container is stopped after a successful cache write
- **THEN** subsequent cache calls return the cache-miss sentinel and do
  not panic, log no error above `Warn`, and the calling handler returns
  its DB-resolved response

#### Scenario: Container restart reconnects transparently
- **WHEN** the Redis container is stopped, then restarted on the same
  address
- **THEN** the next cache write succeeds and the cache-hit path returns
  the stored value on a subsequent `Get`

#### Scenario: Skip when Docker is unavailable
- **WHEN** `SKIP_CONTAINERS=true` is set OR the testcontainer runtime
  cannot reach a Docker daemon
- **THEN** the suite calls `t.Skip(...)` with a message naming the gate,
  matching the existing `feed/repository_integration_test.go` and
  `user/repository_integration_test.go` skip pattern

### Requirement: Python agent dependency vulnerability scan

A `py-audit` job MUST run `pip-audit` against the installed
`backend/agent_py/.venv` after `pip install -e .`, on every PR whose diff
touches `backend/agent_py/**` (gated by `dorny/paths-filter`). The job
MUST fail when `pip-audit` reports any advisory at `High` or `Critical`
severity that is not present in the documented allowlist.

#### Scenario: High-severity transitive advisory fails the job
- **WHEN** a PR to `backend/agent_py/pyproject.toml` (or any file under
  `backend/agent_py/`) introduces a transitive dep with a published
  `High` or `Critical` PyPA advisory
- **THEN** the `py-audit` job exits non-zero and the workflow run is
  failed before `e2e-test` is scheduled

#### Scenario: Allowlisted advisory with rationale does not fail the job
- **WHEN** an advisory is listed in the allowlist file with an inline
  `## Rationale:` comment that cites the upstream advisory URL and the
  decision to defer (e.g. no fixed upstream release; fix-in-progress
  PR; transitive-only-from-test-helper)
- **THEN** `pip-audit` still exits non-zero but the job wrapper script
  classifies the finding as allowlisted and exits zero, mirroring the
  `govulncheck` allowlist mechanism (`.github/workflows/ci.yml:351-358`)

#### Scenario: PR that does not touch Python deps the job
- **WHEN** the diff does not match any path under `backend/agent_py/**`
- **THEN** the `py-audit` job is skipped (the `paths-filter` outputs
  `agent-deps == 'false'`) and no venv install is performed

#### Scenario: Gate precedes e2e-test
- **WHEN** `py-audit` fails
- **THEN** `e2e-test` is not scheduled (it is in the `needs:` list of
  `py-audit`), so a known-bad Python dep does not spend the e2e lane's
  ~minute budget

### Requirement: Allowlist mechanism is auditable

Each entry in the `pip-audit` allowlist MUST be discoverable in CI logs,
MUST cite the upstream advisory identifier, and MUST expire or be
re-evaluated within 90 days of the entry date. The mechanism parallels
`govulncheck`'s allowlist block so a future maintainer can reason about
both gates uniformly.

#### Scenario: Allowlist entry is visible in job output
- **WHEN** an allowlisted advisory is encountered
- **THEN** the job log prints the advisory ID, the rationale comment
  text, and the entry date, so a reviewer can audit the deferral without
  checking the source tree

#### Scenario: Stale allowlist entry fails the job
- **WHEN** an allowlist entry's date is more than 90 days in the past
- **THEN** the wrapper script treats the entry as expired, prints a
  notice, and re-classifies the advisory as failing

### Requirement: Both gates run as part of the standard CI shape

Both new gates MUST run on every push to `main` / `mvp` and on every PR,
with the same runner image (`ubuntu-latest`) and the same SHA-pinning
policy (`actions/checkout@v7.0.1`, etc.) as the rest of the workflow.

#### Scenario: Gate runs on push event
- **WHEN** a commit is pushed to `main` or `mvp`
- **THEN** the `py-audit` and `backend-test` (with the real-Redis
  integration suite) jobs both run in the resulting CI workflow

#### Scenario: Gate runs on pull_request event
- **WHEN** a PR is opened against `main` or `mvp`
- **THEN** both gates run identically to the push-event path

#### Scenario: Gate action pins are SHA-not-tag
- **WHEN** the workflow is inspected
- **THEN** every `uses:` line is pinned to an immutable commit SHA with a
  version comment, matching `SECURITY.md L11` and the pinning table in
  `ci.yml:9-16`

### Requirement: e2e parity across cache implementations

The merge gate MUST exercise the Playwright e2e suite under BOTH cache
implementations: the noop fallback (`REDIS_ENABLED=false`, the current
`e2e-test` shape) and the real-Redis path (`REDIS_ENABLED=true`,
backed by a `redis:7-alpine` service container on the runner). A
failure on either lane MUST fail the build. The two lanes are
implemented as two independent jobs (not a matrix) to preserve the
project's existing one-job-per-gate convention and so each lane's
failures are attributable to the cache shape under test.

#### Scenario: noop cache lane passes when noop is correct
- **WHEN** `e2e-test` runs with `REDIS_ENABLED=false` and the noop
  fallback
- **THEN** all existing Playwright specs pass identically to the
  pre-change baseline (suite count and pass count unchanged)

#### Scenario: real-Redis cache lane passes when real Redis is wired
- **WHEN** `e2e-test-redis` runs with `REDIS_ENABLED=true` and a
  `redis:7-alpine` service container at `localhost:6379`
- **THEN** all existing Playwright specs pass against the real-Redis
  cache path, with `cache.Cache.Ping` reachable from the backend health
  probe and `GET /api/feeds` exercising the cache hit/miss path

#### Scenario: backend health reports cache mode
- **WHEN** either e2e job's backend is queried at `/api/health`
- **THEN** the response indicates the active cache implementation
  (noop vs redis) so a cache-mode regression surfaces in the readiness
  check before browser assertions run (per `ci.yml:285-299`'s
  "HTTP 200 AND services.database == 'up'" pattern)

#### Scenario: Both lanes are needs-edges for release
- **WHEN** `release` is scheduled (push to `main` / `mvp`)
- **THEN** both `e2e-test` and `e2e-test-redis` are in its `needs:`
  list, so a green release requires BOTH cache shapes green
