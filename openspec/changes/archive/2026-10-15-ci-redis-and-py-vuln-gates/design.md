# Design

## Context

See `proposal.md` (Why) for motivation. Two CI gates are missing parity
with the existing supply-chain gates (`govulncheck` for Go, `npm audit`
for the frontend):

1. Real-Redis coverage for `backend/internal/platform/cache/`. The
   layer is reachable via `REDIS_ENABLED=true`; today the only test
   surface is `cache_test.go` against `miniredis`, which cannot exercise
   TCP reconnect, `SCAN+UNLINK` under load, `noeviction` memory
   pressure, real TTL timing, or `PING`-failure fallback. The skip-gate
   pattern (`SKIP_CONTAINERS=true` or empty `dbURL`) is already
   established by `feed/repository_integration_test.go` and
   `user/repository_integration_test.go`, which run against
   `testcontainers-go/modules/postgres` in the `backend-test` CI job
   today.

2. `pip-audit` (or equivalent) for `backend/agent_py/` transitive
   deps. There is no current gate; `agent-alembic-check` only smoke-
   tests alembic migrations, gated on `backend/agent_py/migrations/**`
   (so it skips when only deps change).

`SECURITY.md L11` SHA-pinning policy and the pinning table at
`.github/workflows/ci.yml:9-16` constrain the new workflow lines.
`CLAUDE.md` "Wire-contract tests" + "Spec/code drift" rules apply
(allowlist mechanism must be uniform with `govulncheck`'s so both
gates reason identically).

## Goals / Non-Goals

**Goals:**
- One new integration test file for Redis, plug-compatible with the
  existing testcontainer idiom.
- One new CI job (`py-audit`), plug-compatible with the existing
  `govulncheck` shape (allowlist, exit codes, paths-filter).
- Both gates complete in CI shape in under 5 minutes combined.
- Allowlist auditable: every entry has a rationale comment and a date.
- Both gates fail-closed by default; allowlist is opt-in per advisory.

**Non-Goals:**
- Changing `REDIS_ENABLED` default or runtime cache behavior.
- Replacing the existing `miniredis`-backed unit tests — they keep
  covering the in-process contract.
- Adding Python dep vuln scan to runtime (e.g., dep-scanning on every
  backend boot) — CI-only.
- Auditing transitive deps of the testcontainers-helper chain itself
  (`docker/docker`, `docker/docker-transport`). The existing govulncheck
  allowlist already names these; the new py-audit job does not need to.
- Replacing `agent-alembic-check`. The new `py-audit` job is a
  sibling, not a replacement.

## Decisions

### 1. Use `testcontainers-go/modules/redis` (not the generic container API)

The repo already uses `testcontainers-go v0.41.0` and
`modules/postgres` (`go.sum:204-207`). Adding
`github.com/testcontainers/testcontainers-go/modules/redis` follows the
exact same idiom as `backend/test/testcontainers.go` and the
Postgres-using `*_integration_test.go` files.

**Alternatives considered:**
- Generic `testcontainers.ContainerRequest` with `redis:7-alpine`
  image: works but reinvents the module's helpers (wait strategy,
  log consumer, default port wiring).
- Hand-rolled Docker exec to a sidecar: rejected; this is what
  testcontainers exists to abstract.

### 2. Real-Redis integration suite lives alongside unit tests

`backend/internal/platform/cache/cache_integration_test.go` lives in the
same package as `cache.go` / `cache_test.go`. No `//go:build integration`
tag (the existing Postgres integration tests do not use one; skip is
runtime via `SKIP_CONTAINERS`).

**Alternatives considered:**
- Build-tag gating (`//go:build integration`): would require every
  developer to remember `-tags=integration`; the runtime skip is more
  forgiving and matches the project's existing convention.
- Separate `integration/cache` package: gratuitous indirection for one
  file; the existing Postgres integration tests are package-local.

### 3. Use `pip-audit` (PyPA-official) over `osv-scanner` / `safety`

`pip-audit` is published by PyPA under the Python Software Foundation,
consumes the PyPI advisory database directly, and runs against an
installed venv (`pip-audit -r requirements.txt` or no args for "the
whole venv"). No lockfile requirement (the agent uses `pyproject.toml`,
not `uv.lock` or `pip-tools`). Single static binary, no network calls
beyond advisory DB refresh.

**Alternatives considered:**
- `osv-scanner` (Google): would also work; supports multiple
  ecosystems but expects a lockfile. Not better for this surface.
- `safety` (pyup.io): third-party service dependency; out of step with
  the "vendor-neutral" posture of the existing govulncheck / npm-audit
  gates.

### 4. Every pip-audit advisory gates; the allowlist is the only deferral lever

Revised after probing the tool: `pip-audit`'s JSON output carries no
severity field and it has no `--audit-level`-style flag, so the
`--audit-level=high` posture planned in the first draft is
unimplementable. `govulncheck` filters on call-graph reachability and
`npm audit` on severity; `pip-audit` has neither, so every advisory
it reports fails the job unless a dated, rationale-carrying allowlist
entry defers it (see decision 5). That is the same role
`--audit-level=high` plays at `ci.yml:380` — the noise valve is
explicit and reviewed rather than a numeric threshold.

`pip` itself is part of the audited venv, so the job must
`pip install --upgrade pip` before installing the project: the dev
venv's pinned pip 23.2.1 surfaced 14 advisories against pip alone,
while a fresh venv on the same pyproject audited clean (0 advisories,
2026-10-15).

pip-audit also emits dependencies it could not audit at all as
`skip_reason` entries (no `version`, no `vulns`), and it exits 1 when it
could not collect a dependency — `--strict` makes that fatal, but it
also fires for the project's own editable install (`nyx-agent` is not on
PyPI), which would pin the gate permanently red. So the wrapper owns
that decision instead: every `skip_reason` prints, and any skipped
distribution other than the project itself fails the gate. Same
fail-closed reasoning for the exit code: anything outside pip-audit's
documented 0 / 1 contract is exit 2, never a pass derived from
still-parseable stdout.

### 5. Allowlist is a single TOML file with inline rationale comments

`backend/agent_py/.pip-audit-allowlist.toml`:

```toml
# Each [[advisories]] entry carries a date and a `rationale` string
# the job prints on match. Entries older than 90 days are treated as
# expired by the wrapper script.
[[advisories]]
id = "PYSEC-2024-XXXXX"
date = "2026-10-15"
rationale = "Transitive dep of testcontainers; no fix upstream; revisit when v0.30 ships."
```

A small wrapper script (`scripts/ci/pip_audit_with_allowlist.py`) runs
`pip-audit --format=json`, applies the allowlist, checks the 90-day
freshness, and exits 0/1/2 with a clear log message for each case.
Shape parallels the existing govulncheck wrapper
(`ci.yml:343-358`).

**Alternatives considered:**
- Embedding the allowlist in the job YAML: bloats the workflow file,
  bad review surface for code owners.
- Inline `# noqa`-style comments in `pyproject.toml`: `pip-audit`
  doesn't read them.

### 6. `py-audit` is a new job, not folded into `agent-alembic-check`

`agent-alembic-check` is gated by `backend/agent_py/migrations/**`
(only). The new `py-audit` is gated by `backend/agent_py/**` (broader
— also `pyproject.toml`, `requirements*.txt`, the `.venv` lockfile, and
any Python source that adds deps). The two have different paths-filter
inputs and different venv lifecycles; folding them would force every
dep-only PR to pay the alembic setup cost.

### 7. Test container lifecycle: shared per package, restart for failure tests

A package-level `TestMain` starts one Redis testcontainer and shares it
across the suite; tests that need to simulate connection failure
(`PING` failure, reconnect) stop and start the container through the
module's `Stop` / `Start`, which reuse the same host-port binding so the
client's address survives the outage.

The skip decision is deliberately narrow: `SKIP_CONTAINERS=true` skips,
and so does an unreachable daemon — probed with the testcontainers
provider's own `Health`, i.e. the same code path `redis.Run` uses, so
"the runtime cannot reach a daemon" cannot be confused with "the daemon
is fine but the container would not start". Everything else exits 1. A
first draft skipped on *any* `redis.Run` error, which would have turned
the gate green whenever a Docker Hub pull was throttled — the exact
silent-no-op failure the gate exists to prevent.

### 8. CI caching strategy

- `backend/go.sum` is already cache-keyed (`setup-go` + `cache-dependency-path`)
  on the `backend-test` job; the new `testcontainers-go/modules/redis`
  module transitively uses the same sum so no new cache key needed.
- `py-audit` uses `actions/setup-python` + `cache: 'pip'` keyed on
  `backend/agent_py/pyproject.toml` (no `requirements.txt` in this
  repo). First install pays the wheel-build cost; subsequent runs reuse
  the GH Actions cache.
- No `vllm_cache`-equivalent volume needed; the venv is ~50 MB.

### 9. e2e parity uses two jobs (not a matrix)

The repo has zero `strategy.matrix` usage today
(`.github/workflows/*.yml` grep clean). Introducing a matrix would be
the first such precedent and adds YAML shape the maintainers haven't
internalized. Two sibling jobs (`e2e-test` kept as-is, new
`e2e-test-redis` as a copy with `REDIS_ENABLED=true` and a `redis:7-alpine`
service container) is more aligned with the "one job per gate"
convention and isolates failures clearly: a green `e2e-test` + red
`e2e-test-redis` immediately points at the cache-mode path.

**Alternatives considered:**
- Matrix `strategy.fail-fast: false` with `[noop, redis]`: would force
  the `redis` service container to be present on the `noop` variant
  too (matrix services can't be conditional per variant without
  per-variant `include:` overrides). Wasteful and confusing.
- Keep `e2e-test` only with `REDIS_ENABLED=true`, drop the noop lane:
  loses the regression baseline. A future "REDIS_ENABLED default
  flipped" incident would have no signal that we already broke the
  fallback.

## Risks / Trade-offs

- [Docker daemon in CI runners] → `ubuntu-latest` Actions runners have
  a working Docker daemon (the `services:` postgres block in
  `backend-test` already proves it). No new dependency. If a future
  migration moves CI to a Docker-less runner, the new test will
  `t.Skip` with the same message as the Postgres integration tests.
- [pip-audit advisory DB freshness] → like `npm audit`, the advisory
  DB is fetched fresh each run; CI records what it saw. Documented
  inline at `ci.yml:376-379` for npm; the same caveat applies to
  pip-audit and is noted in the new job's comments.
- [Test runtime] → Real-Redis testcontainer adds ~10-30 s total to
  `backend-test` (one shared container start; reconnect tests add
  ~5 s each). CI job timeout is not raised; the existing 5-minute
  budget has ~3 min headroom.
- [Allowlist hygiene] → A forgotten entry past 90 days fails the job
  to force re-evaluation. The wrapper script also prints a one-liner
  per matched advisory citing the date so reviewers can spot stale
  entries in the log.
- [py-audit on first run discovers long-standing vulns] → A first
  green run may require an initial allowlist sweep to grandfather
  pre-existing advisories (mirroring the govulncheck GO-2026-4883/4887
  precedent at `ci.yml:351-358`). Tasks list includes the baseline
  sweep explicitly so it does not silently swallow real bugs. Observed
  outcome (2026-10-15): the sweep came back empty — a fresh venv built
  the CI way (`pip install --upgrade pip` + `pip install -e '.[ci]'`)
  reports 0 advisories, so the allowlist ships with its header comment
  only. The findings seen against the dev venv were entirely pip's own
  advisories under the pod's stale pip 23.2.1.
- [Two e2e jobs doubles CI minute cost on every push] → ~1 minute per
  job, ~2 minutes total instead of ~1. Free tier is 2000 min/month on
  `ubuntu-latest` for public repos; the added minute is well within
  budget and well below the value of catching a cache-mode regression
  before merge. The two jobs share the same Postgres service container
  spec, so there's no new infra cost beyond the `redis:7-alpine`
  sidecar.

## Migration Plan

No runtime migration. Both gates land in a single PR:

1. Land the new test file + workflow change in one commit.
2. Run CI; expect first py-audit run to surface pre-existing advisories.
3. Either fix them (preferred) or grandfather with documented rationale.
4. Land is complete when CI is green.

Rollback = `git revert <merge-sha>`. Both gates are additive and
independent; reverting one leaves the other intact.

## Open Questions

- None. All decisions above are sufficient to start tasks; the
  baseline-allowlist sweep is a tasks-level concern, not a design
  ambiguity.
