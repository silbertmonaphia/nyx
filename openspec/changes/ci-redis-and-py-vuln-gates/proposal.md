# Proposal

## Why

Two missing CI gates leave the supply-chain surface partially uncovered.
The Go side is fully gated (`govulncheck` for reachable vulns + per-package
integration tests against real Postgres via testcontainers), but two
parity gaps remain:

1. **Redis is exercised only against `miniredis`.** Production can flip
   `REDIS_ENABLED=true` any time; miniredis is a faithful-enough in-process
   implementation for interface-contract tests (Get/Set/Delete/DeletePrefix/
   IncrBy/TTL) but cannot reach the real-Redis surface (TCP reconnection,
   `noeviction` memory pressure, `SCAN+UNLINK` under load, real TTL timing,
   `PING` failure fallback timing). `backend/Makefile:19-22` already names
   this as a known gap. The cache layer is `best-effort` (errors are
   swallowed per CLAUDE.md), so silent fallback bugs reach production.

2. **The Python agent has no dep-vulnerability scan.** `govulncheck` covers
   Go, `npm audit` covers the frontend, but `backend/agent_py/` deps
   (fastapi / langgraph / pydantic / openai / httpx / redis-py /
   langfuse-sdk / uvicorn — all reachable from network-exposed handlers)
   are only validated by the `agent-alembic-check` job, which only
   smoke-tests alembic migrations. A new transitive CVE in any of these
   hits production unflagged.

Both gaps are completable in the dev/CI environment with no rollout gates.

## What Changes

- Add `backend/internal/platform/cache/cache_integration_test.go` exercising
  the cache layer against a real Redis testcontainer (testcontainers-go
  `modules/redis`): TTL expiry timing, `DeletePrefix` SCAN+UNLINK behavior,
  `noeviction` under memory pressure, client-side reconnect after
  container stop/start, `PING` failure fallback to noop.
- Skip-gate semantics match the existing Postgres pattern (`SKIP_CONTAINERS
  =true` or `dbURL == ""` ⇒ `t.Skip(...)`). Local default runs the full
  set; CI default runs the full set because `backend-test` does not set
  `SKIP_CONTAINERS`. Coverage counts because `make coverage-check` runs
  `go test ./...` after the change.
- Add a `py-audit` job to `.github/workflows/ci.yml` running `pip-audit`
  (PyPA official) against `backend/agent_py/.venv` after `pip install
  -e .`, with a documented allowlist mechanism parallel to govulncheck's
  (each allowlisted advisory requires an inline `## Rationale:` comment
  citing the upstream issue and the upgrade-or-not decision). Gated on
  `dorny/paths-filter` for `backend/agent_py/**` so PRs that don't touch
  Python deps don't pay the venv install cost.
- `py-audit` becomes a `needs:` dependency of `e2e-test` (parallel to
  `govulncheck` / `npm-audit`), so a high-severity Python dep CVE blocks
  merge before the e2e lane spends its ~minute.
- Add a sibling `e2e-test-redis` job to `.github/workflows/ci.yml` that
  exercises the real-Redis path end-to-end (browser-driven Playwright
  suite against a backend booted with `REDIS_ENABLED=true` and a real
  `redis:7-alpine` service container). The existing `e2e-test` job is
  kept intact (`REDIS_ENABLED=false`, noop fallback), so the merge gate
  covers BOTH cache implementations. A failure on either lane fails the
  build. Both jobs become `needs:` of `release`, parallel to the
  existing dependency list.
- `agent-alembic-check` keeps its existing migration-smoke shape; the new
  `py-audit` is a sibling job, not a replacement.

## Capabilities

### New capabilities

- `ci-supply-chain-gates`: The CI supply-chain gate contract. Covers the
  testcontainer-Redis real-path requirement and the `pip-audit`
  Python-dep requirement.

### Modified capabilities

- None. Existing runtime-behavior specs (cache best-effort fallback,
  agent Python service, etc.) are unchanged — this change only tightens
  the merge gate, not the runtime contract.

## Impact

- `backend/go.mod`, `backend/go.sum` — add
  `github.com/testcontainers/testcontainers-go/modules/redis` (separate
  Go module; main `testcontainers-go` is already present).
- `backend/internal/platform/cache/cache_integration_test.go` — new file
  (~200-400 LOC test + helpers).
- `backend/Makefile` — no change; `make coverage-check` already runs
  `go test ./...` so the new test contributes to the coverage gate.
- `.github/workflows/ci.yml` — new `py-audit` job, `paths-filter` step,
  new `needs:` edge from `e2e-test`; new sibling `e2e-test-redis` job
  with a `redis:7-alpine` service container and `REDIS_ENABLED=true`,
  parallel `needs:` into `release`. Total ~60 added lines across both
  jobs.
- `backend/agent_py/.venv/` — `pip-audit` installed alongside the
  existing `.venv` install.
- No runtime code paths change. No `REDIS_ENABLED` default change, no
  agent runtime change.
- No rollout gates — completable in dev/CI; rollout-split sibling not
  required.
