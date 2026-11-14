# Tasks

## 1. Agent full-suite CI gate

- [ ] 1.1 Add the `agent-test` job to `.github/workflows/ci.yml`: `backend/agent_py/**` path filter, `pgvector/pgvector:pg17` service with `POSTGRES_DB=nyx_test`, Python 3.12 + uv 0.10.9 setup, `uv sync --locked --extra dev`, `alembic upgrade head`, then `pytest tests/ -q`. Set `TEST_ASYNC_PG_DSN` (plain DSN), `PYTHON_AGENT_DATABASE_URL` (asyncpg DSN), `AGENT_SERVICE_TOKEN`, and `PYTHON_AGENT_LLM_BASE_URL`. Verify: the workflow YAML parses, and the job runs green on a branch with DB-backed tests reported as run, not skipped.

- [x] 1.2 Add the anti-vacuity guard to the same job: run pytest with `-rs`, tee output, and fail when the skip summary contains `TEST_ASYNC_PG_DSN not set`. Verify: a local reproduction of the guard's shell snippet with the DSN unset exits non-zero, and the real job with the DSN set exits zero.

- [x] 1.3 Add `pytest-cov` to `[project.optional-dependencies].dev`, refresh `uv.lock` with `uv lock`, and add agent Makefile targets: `test` (suite, no coverage) and `test-cov` (`--cov=agent --cov-fail-under=$(COVERAGE_MIN)`, failing fast when `TEST_ASYNC_PG_DSN` is unset). Record `COVERAGE_MIN` from a DSN-enabled run with ~2 points of headroom. Verify: `make -C backend/agent_py test-cov` against `nyx_test` exits 0 and prints the measured number; raising the floor one point above the measurement makes it exit non-zero.

- [x] 1.4 Document the agent test commands (`backend/agent_py/README.md`, `README.zh.md`, `CLAUDE.md` command list) and verify each documented command runs as written.

## 2. Go race detector

- [x] 2.1 Add `RACE ?= -race` and a `test` target (`go test $(RACE) -count=1 ./...`) to `backend/Makefile`; keep `coverage*` targets race-free. Verify: `make -C backend test` runs the suite with `-race` and exits 0, and `RACE=` produces a race-free invocation.

- [ ] 2.2 Change the CI backend `Test` step to `go test -race -v ./...`. Verify: the CI log shows the race flag, and the backend job stays green with the Postgres service.

- [x] 2.3 Update the `CLAUDE.md` backend test commands and `Harness.md` / `Harness.zh.md` gate inventory to name the race-enabled default. Verify: the documented commands and gate list match the Makefile and workflow.

## 3. Agent mypy hard gate

- [x] 3.1 Add `[tool.mypy]` to `backend/agent_py/pyproject.toml` (`python_version`, `files = ["agent"]`, `check_untyped_defs`, `warn_unused_ignores`, `no_implicit_optional`, `strict_equality`, `warn_redundant_casts`, `warn_unused_configs`) plus the `asyncpg` / `asyncpg.*` `ignore_missing_imports` override. Verify: `mypy agent` reports the expected error set under the new config.

- [x] 3.2 Fix the `union-attr`, `misc`, and `assignment` errors (including the `exc`-outside-except defect), adding or tightening tests where a real `None`-handling bug is exposed. Verify: `mypy agent` count drops and the affected package tests pass.

- [x] 3.3 Fix the `arg-type` and `attr-defined` errors and the two `warn_unused_ignores` findings; use only narrow tool directives (`# type: ignore[code]`) where a suppression is genuinely required, with a test proving it. Verify: `mypy agent` exits 0 with no output.

- [x] 3.4 Delete `.mypy-known-errors.txt`, the `mypy-baseline` target and its `.PHONY`/help entries, and simplify `lint-mypy` to `$(MYPY) agent`; update the stale "43 pre-existing errors" comments in `ci.yml` and the Makefile. Verify: `make -C backend/agent_py lint` is green, `grep -r mypy-known-errors` finds nothing outside archived changes, and the gate fails on a deliberately introduced type error.

- [x] 3.5 Update the agent READMEs and `Harness.md` / `Harness.zh.md` to describe mypy as a hard gate. Verify: no doc still instructs regenerating a baseline.

## 4. Frontend coverage floor

- [x] 4.1 Add `test.coverage` to `frontend/vite.config.ts` (v8, `src/**/*.{ts,tsx}` include, test/test-setup/generated-schema excludes, `text-summary` + `text` reporters, thresholds statements 86 / branches 80 / functions 84 / lines 88) and a `test:coverage` script. Verify: `npm run test:coverage` exits 0 and prints a summary above every threshold.

- [ ] 4.2 Make `npm run test:coverage` the `frontend-test` CI `Test` step, replacing the plain `npm test` step. Verify: the CI log shows the coverage summary and the job is green.

- [x] 4.3 Prove the floor is not vacuous: temporarily add a small untested module under `src/`, confirm coverage drops, then temporarily delete a covered test module and confirm the gate fails; restore both. Verify: both experiments exit non-zero only in the expected direction, and the restored tree is green.

- [x] 4.4 Update `CLAUDE.md` frontend commands and `Harness.md` / `Harness.zh.md` to include the coverage gate. Verify: documented commands match `package.json`.

## 5. Integration verification

- [x] 5.1 Run the local gate sweep: `make -C backend test`, `make -C backend coverage-check`, `make -C backend/agent_py lint`, `make -C backend/agent_py test` (bare and with `TEST_ASYNC_PG_DSN`), `make -C backend/agent_py test-cov`, `npm test`, `npm run test:coverage`, `npm run build`. Verify: every command exits 0 and the coverage numbers meet their floors.

- [ ] 5.2 Verify the CI run on the branch: all jobs green; the agent job's log shows DB-backed tests executing and the skip guard passing; the backend log shows `-race`; both coverage summaries are present. Verify: paste the four log excerpts into the task evidence.

- [x] 5.3 Verify no new explanatory comments were added on changed lines (`python3 scripts/check_no_comments.py --range origin/mvp...HEAD` or the CI `no-new-comments` job). Verify: `python3 scripts/check_no_comments.py --range HEAD` exits 0 on the working tree (post-commit, the CI range check applies).

## Workflow follow-up

- Archive this change after review and merge, per the rollout-split convention (implementation change, no rollout sibling).
