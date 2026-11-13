# Design

## Context

See `proposal.md` — Why. The current CI topology that this design extends:
`backend-test` (Go lint/drift/unit with a Postgres service and `DB_URL` set),
`agent-alembic-check` (pgvector Postgres + `alembic upgrade head`, migration
paths only), `agent-lint` (ruff + mypy baseline diff + a two-file pytest
subset + eval unit gate), `frontend-test` (typecheck, lint, arch, OpenAPI
drift, vitest, build), and two e2e lanes. The agent's `tests/conftest.py`
requires `TEST_ASYNC_PG_DSN` to name exactly `nyx_test` because its fixtures
DROP and recreate `agent.*` / `langgraph.*`, and 20 test modules self-skip
without it. The backend Makefile has no race flags; the agent Makefile encodes
the mypy baseline mechanism; the frontend has `@vitest/coverage-v8` installed
but no configuration or threshold.

Measured facts this design relies on (2026-11-13, this worktree):

- `pytest tests/ --collect-only` → 856 tests; bare run self-skips every
  DB-backed module.
- Frontend coverage, full `src/**` include: statements 88.62%, branches
  82.94%, functions 87.04%, lines 91.04% (statements 2369/2673).
- `mypy agent`: 52 errors / 10 files. `--check-untyped-defs`, `--strict-equality`,
  `--warn-redundant-casts`, `--no-implicit-optional` add none; `--warn-unused-ignores`
  adds 2 (54); `--warn-return-any` adds 56 (108); `--strict` yields 251 / 22 files.

## Goals / Non-Goals

**Goals:**

- Every agent test — including DB-backed modules — runs in CI on agent
  changes, and the job cannot pass vacuously when the DSN is missing.
- Go concurrency regressions are detectable in CI and by default locally.
- Agent type checking fails on any error with no accept-the-baseline escape
  hatch, at a modestly stronger configuration than today.
- Frontend and agent coverage regressions fail their jobs; untested new
  frontend modules count against the floor.

**Non-Goals:**

- Full mypy `--strict` (251 errors) or `--warn-return-any` (108 errors) —
  deferred; this change does not attempt a whole-codebase annotation
  campaign.
- Backend coverage policy changes — the existing 70%-per-package gate stays.
- Running the `@live` Playwright set or the promptfoo golden/security evals
  in CI (GPU- and model-dependent; still operator-run).
- Frontend mobile-viewport, a11y, or visual-regression coverage.

## Decisions

### D1 — A dedicated `agent-test` job, not an extension of `agent-lint`

Add a new path-filtered job (`backend/agent_py/**`, same dorny/paths-filter
pattern and pinned SHAs already in the workflow) that owns the expensive
Python execution:

- **Service**: `pgvector/pgvector:pg17`, `POSTGRES_DB=nyx_test`, the same
  health-check options as `agent-alembic-check`.
- **Env**: `TEST_ASYNC_PG_DSN=postgresql://postgres:postgres@localhost:5432/nyx_test`
  (plain driver for asyncpg and the conftest guard),
  `PYTHON_AGENT_DATABASE_URL=postgresql+asyncpg://postgres:postgres@localhost:5432/nyx_test`
  (SQLAlchemy driver for `migrations/env.py`), plus
  `AGENT_SERVICE_TOKEN` and `PYTHON_AGENT_LLM_BASE_URL` for import-time
  settings parity with `agent-lint`.
- **Steps**: setup Python 3.12 + uv 0.10.9 (existing pinned actions),
  `uv sync --locked --extra dev`, `.venv/bin/alembic upgrade head`, then the
  suite with coverage.
- **Timeout**: 20 minutes; the suite is expected to take single-digit
  minutes, the ceiling absorbs the race-free Python suite plus migration.

Why separate from `agent-lint`: lint is cheap and should stay quick feedback;
the DB suite needs a service container and dev extras, and a red test should
not be conflated with a lint finding. Why not inside `backend-test`: that job
is the Go toolchain's; adding Python setup there couples unrelated failures
and slows every backend change even when the agent is untouched.

### D2 — Anti-vacuity: a DSN-skip guard in the same job

If `TEST_ASYNC_PG_DSN` is renamed, misspelled, or dropped from the job env,
20 modules still pass by self-skipping — the exact false-green this change
exists to remove. The suite therefore runs with `-rs`, tees its output, and
fails the job when the skip summary contains `TEST_ASYNC_PG_DSN not set`.
Expected skips (`PYTHON_AGENT_LIVE_*` live-network modules) do not match that
string and remain allowed. The conftest database-name guard remains the second
line of defense: a DSN pointing anywhere but `nyx_test` raises `pytest.UsageError`
before collection.

Alternative considered: rewriting all 20 module-level `skipif` declarations
into a fixture that asserts — more invasive and easier to regress than one
output check. The output check also validates the job's env wiring, which a
fixture cannot.

### D3 — Race detection: full suite in CI, opt-out locally

- CI's existing `Test` step becomes `go test -race -v ./...`; `DB_URL` is
  already set against the service Postgres, so integration tests run and the
  Docker/testcontainers path is not exercised in CI.
- `backend/Makefile` gains `RACE ?= -race` and a `test` target:
  `go test $(RACE) -count=1 ./...`. `RACE=` is the documented opt-out
  (e.g. a box without a C toolchain, or a deliberately fast iteration loop).
- `coverage` / `coverage-check` / `coverage-html` stay race-free: coverage
  instrumentation plus the race detector multiplies the slowest job for no
  additional signal, and the `test` step already runs the same suite under
  `-race`.

Alternative considered: race only on `internal/chat`, `internal/feed`,
`internal/platform/cache` — narrower runtime but risks missing races in the
wiring and new packages; rejected in favor of the full suite. If runtime
becomes a problem, a follow-up can split race into its own parallel job without
changing coverage.

### D4 — Mypy hard gate at a deliberately modest configuration

Add `[tool.mypy]` to `backend/agent_py/pyproject.toml`:

- `python_version = "3.12"`, `files = ["agent"]`
- `check_untyped_defs = true` (measured: zero new errors today, but future
  unannotated functions are checked)
- `warn_unused_ignores = true` (2 errors to fix; prevents stale suppressions)
- `no_implicit_optional = true`, `strict_equality = true`,
  `warn_redundant_casts = true`, `warn_unused_configs = true` (all zero new
  errors today)

Then fix all 52 errors: 19 `union-attr`, 17 `arg-type`, 8 `misc` (including one
`Assignment to variable "exc" outside except` — a real defect), 4
`import-untyped`, 3 `assignment`, 1 `attr-defined`. The four `import-untyped`
findings are `asyncpg` (no bundled `py.typed`): add a targeted
`[[tool.mypy.overrides]]` with `ignore_missing_imports = true` for
`asyncpg` / `asyncpg.*` rather than adding a stub package dependency. Any
remaining suppression must be a narrow tool directive (`# type: ignore[code]`)
on a line that a test proves necessary — the repo's no-comments rule permits
tool directives, not explanations.

Delete `.mypy-known-errors.txt`, the `mypy-baseline` target, its `.PHONY`
entry, and its help lines; `make lint-mypy` becomes `$(MYPY) agent` (config
picks the file set). Update the stale "43 pre-existing errors" comments in
`ci.yml` and the agent Makefile. Full `--strict` is deferred; `warn-return-any`
and `disallow-untyped-defs` are the next candidates, explicitly outside this
change.

### D5 — Frontend coverage: static floors with headroom

Configure `test.coverage` in `frontend/vite.config.ts`:

- `provider: "v8"`, `include: ["src/**/*.{ts,tsx}"]`, exclude test files,
  `src/test/**`, and the generated `src/api/openapi/schema.d.ts`.
- `reporter: ["text-summary", "text"]` (CI logs carry the number);
  the default `coverage/` output is already gitignored.
- Thresholds with ~2–3 points of headroom under today's full-include
  measurement: `statements: 86`, `branches: 80`, `functions: 84`,
  `lines: 88`.

Add `test:coverage` (`vitest run --coverage`) and make it the CI `Test` step;
`npm test` stays the fast local run without coverage. Why static floors over a
ratchet: floors are simple, reviewable, and do not false-fail legitimate
refactors that delete covered code; the backend's `COVERAGE_MIN` pattern is the
precedent. The include set guarantees an untested new module enters the
denominator (verified by the measurement above: full include vs. default
instrumented set differed by ~0.3 points, so the floor is not an artifact of
only counting imported files).

### D6 — Agent coverage: one global floor, measured with the DB suite

Add `pytest-cov` to `[project.optional-dependencies].dev` and refresh
`uv.lock`. The agent Makefile gains:

- `test` — the full suite, no coverage (works with or without a DSN).
- `test-cov` — the suite with `--cov=agent --cov-fail-under=$(COVERAGE_MIN)`;
  it fails fast when `TEST_ASYNC_PG_DSN` is unset, because the floor is only
  meaningful when DB-backed tests execute.

`COVERAGE_MIN` is recorded next to the target, set from the first DSN-enabled
run with ~2 points of headroom, and adjusted by PR like the backend's value.
The new CI job runs `make test-cov`, so the gated suite and the measured suite
are the same. Why one global floor rather than per-module: 34 source files do
not warrant per-package bookkeeping; the global number plus the DB suite and
wire-contract tests is proportionate, and the backend's per-package gate is
grandfathered policy for a larger Go tree.

## Testing

- **New job smoke**: run the workflow on a branch (or `act`-equivalent where
  available) and confirm the agent job reports DB-backed tests executing, not
  skipping; intentionally unset `TEST_ASYNC_PG_DSN` once and confirm the skip
  guard fails the job.
- **Race**: run `make -C backend test` locally; verify the flag reaches
  `go test` (e.g. a deliberate `-race`-detectable test is not needed — confirm
  the invocation and a zero-exit run). CI's full suite must stay green.
- **Mypy**: `make -C backend/agent_py lint` is green with the baseline files
  deleted; introduce a temporary error/unused ignore and confirm the gate
  fails; remove it.
- **Frontend coverage**: `npm run test:coverage` passes at the committed
  thresholds; delete a test module temporarily and confirm the floor trips;
  add a small untested module under `src/` and confirm the denominator grows.
- **Agent coverage**: run `make -C backend/agent_py test-cov` against a local
  `nyx_test` (or the CI job) and record the measured number; confirm a floor
  one point above it fails.

## Risks / Trade-offs

- **CI runtime grows** (new Python job plus race-instrumented Go suite) →
  path filters keep unrelated PRs cheap; the new job is `needs: [backend-test]`
  so a Go red does not spend Python minutes; if the total becomes painful,
  split race into a parallel job.
- **First-time DB suite in CI may expose latent flakes or missing schema
  assumptions** → the 5-iteration fix loop applies; fix the tests/fixtures
  rather than loosening the gate.
- **Coverage floors can block legitimate deletions of covered code** → floors
  sit 2–3 points under the measured baseline and can be adjusted by PR, the
  same rule as `COVERAGE_MIN`.
- **Mypy fixes may reveal real behavior bugs** (e.g. `Optional.finalize`
  calls) → fix at the source with tests; do not silence. This is a benefit,
  but it may expand the diff.
- **Race detector may surface real races** → fix them; suppressing with
  build tags is out of scope and should be rejected in review.
- **`warn_unused_ignores` can fight legitimate platform-specific suppressions**
  → only two exist today; remove or narrow them; future cases use targeted
  codes and tests.
- **The skip guard is string-based** → it keys on the canonical reason text
  shared by the 20 modules; a future reason rewording must keep the substring
  or update the guard in the same change.

## Migration Plan

Single PR: land the new job, Makefile/pyproject/config changes, mypy fixes,
and doc updates together. Rollback is a revert of the specific job/step or
config block; no runtime, schema, or deployment migration is involved.

## Open Questions

None.
