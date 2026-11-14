# Proposal

## Why

CI currently lets regressions merge green in three places. The Python agent's
856-test suite — including every DB-backed test — never runs in CI: the
`agent-lint` job executes only `tests/test_service_auth.py`,
`tests/test_config.py`, and the eval-unit gate. Go tests run without the race
detector, despite SSE streams, caches, and proxied agent traffic. Agent type
checking is a baseline diff (`mypy`) that accepts 52 known errors instead of
blocking them, and the baseline comments have already drifted (they claim 43).
Meanwhile the frontend and agent have no coverage floor at all, while the
backend enforces 70% per package — so a PR can delete tests in two of the three
runtimes without any job noticing. Unit-suite-green + CI-green therefore still
ships real regressions.

## What Changes

- **Agent suite gate.** A new path-filtered `agent-test` CI job boots the
  pgvector Postgres image (database `nyx_test`), applies the agent's alembic
  chain, installs dev extras, and runs the complete `pytest tests/` suite with
  `TEST_ASYNC_PG_DSN` set. An anti-vacuity guard fails the job if DB-backed
  tests self-skip, so a misconfigured DSN cannot silently turn the gate into an
  offline-only pass.
- **Go race detector.** CI runs `go test -race`, and a new backend `make test`
  target defaults to `-race` locally (`RACE=` opts out for constrained
  environments).
- **Agent type checking becomes a hard gate.** The 52 existing mypy errors are
  fixed, `.mypy-known-errors.txt` and `make mypy-baseline` are deleted, and
  `[tool.mypy]` pins a modestly stricter configuration (`check_untyped_defs`,
  `warn_unused_ignores`, `no_implicit_optional`, `strict_equality`). Full
  `--strict` (measured: 251 errors) is explicitly out of scope.
- **Coverage floors for the two runtimes that lack one.** Frontend vitest gains
  configured coverage thresholds (today's measured baseline: lines 91.53%,
  statements 89.07%, branches 83.53%, functions 87.82%); the agent gains
  `pytest-cov` with a floor recorded in its Makefile. Both are enforced in
  their existing/new CI jobs.
- **Docs and comments reconciled** with the new gates, including the stale
  "43 pre-existing mypy errors" comments.

**Non-change:** the backend coverage gate (70% per package) and the
`ci-supply-chain-gates` capability are untouched. No production runtime
behavior changes.

## Capabilities

### New Capabilities

- `ci-test-gates`: the CI contract that the agent's full test suite
  (DB-backed tests included), Go race detection, agent type checking at zero
  errors, and frontend/agent coverage floors all execute and block merges —
  with anti-vacuity guards wherever a skip could fake a pass.

### Modified Capabilities

- None.

## Impact

- **CI/config**: `.github/workflows/ci.yml`, `backend/Makefile`,
  `backend/agent_py/Makefile`, `backend/agent_py/pyproject.toml`,
  `backend/agent_py/uv.lock`, `frontend/package.json`,
  `frontend/package-lock.json`, `frontend/vite.config.ts`.
- **Agent source**: type-only fixes across `backend/agent_py/agent/**`
  (19 `union-attr`, 17 `arg-type`, 8 `misc`, 4 `import-untyped`, 3 `assignment`,
  1 `attr-defined`); no behavior change intended, and any real `None`-handling
  bug the annotations expose is fixed at the source.
- **Docs**: `CLAUDE.md` command list, `Harness.md` / `Harness.zh.md` gate
  inventory, `backend/agent_py/README.md` / `README.zh.md`, stale workflow
  comments.
- **CI cost**: one new Python job (~5–10 min) and a slower backend test step
  (race detector); no new service image beyond the Postgres/pgvector image the
  alembic job already uses.
- **No rollout gates**: every task is completable in the dev/CI environment
  now — no production flip, deployed-stack smoke, or multi-day window — so no
  `-rollout` sibling per the classification test.
