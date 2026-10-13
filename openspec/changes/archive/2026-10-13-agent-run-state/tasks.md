# Tasks

## 0. Spike: verify langGraph's custom-schema support

- [x] 0.1 Read `langgraph/checkpoint/postgres/aio.py` and `base.py`. **Result:** `AsyncPostgresSaver.from_conn_string(conn_string, *, pipeline=False, serde=None)` has NO `schema_name` kwarg. MIGRATIONS list uses bare `CREATE TABLE IF NOT EXISTS checkpoint_*` with no schema qualifier. langGraph does NOT use alembic — it ships a hardcoded `MIGRATIONS` list and runs them via `setup()` against the connection. **Decision:** schema location is controlled by (a) DSN `search_path=langgraph,agent,public` + (b) pre-existing `langgraph` schema. We do NOT invoke langGraph's alembic — we call `setup()` after our migration creates the schema.
- [x] 0.2 Verify schema lands correctly: read the MIGRATIONS SQL + understand that `setup()` executes `cur.execute(self.MIGRATIONS[0])` etc. via the configured connection. With `search_path=langgraph,agent,public` and the `langgraph` schema pre-created, `CREATE TABLE IF NOT EXISTS checkpoint_*` resolves to `langgraph.checkpoint_*`. **Source-code verification only** (no testcontainer available in this environment); final verification happens in task 10.x when Docker is available.
- [x] 0.3 Fallback not needed — langGraph's setup works with our approach. **Adjust tasks 2.x and 5.x** to call `AsyncPostgresSaver.setup()` after our migration creates the schema (not invoke langGraph's alembic).

## 1. Add alembic scaffold

- [x] 1.1 Add `alembic` + `sqlalchemy[asyncio]` + `langgraph-checkpoint-postgres` + `asyncpg` to `backend/agent_py/pyproject.toml` runtime deps. Verify `pip install -e .` succeeds.
- [x] 1.2 Create `backend/agent_py/alembic.ini` with `script_location = migrations`, `prepend_sys_path = .`, and `sqlalchemy.url` left blank (driven by `PYTHON_AGENT_DATABASE_URL` in `env.py`).
- [x] 1.3 Create `backend/agent_py/migrations/script.py.mako` (vendored from alembic init; the standard template).
- [x] 1.4 Create `backend/agent_py/migrations/env.py` — async glue. Reads `PYTHON_AGENT_DATABASE_URL` from env, ensures `options=-c search_path=langgraph,agent,public` is present, instantiates async engine, runs migrations via `run_sync_migrations()`. Sets `version_table_schema='agent'` on the migration context.

## 2. Migration: create `langgraph` schema + langGraph checkpoint tables

- [x] 2.1 `backend/agent_py/migrations/versions/0001_langgraph_checkpoint.py` — `upgrade()` body: `CREATE SCHEMA IF NOT EXISTS langgraph`. langGraph's `checkpoint_*` tables are created by `AsyncPostgresSaver.setup()` at app startup (task 5.2), not by alembic.
- [x] 2.2 `downgrade()`: `DROP SCHEMA IF EXISTS langgraph CASCADE`.
- [x] 2.3 Verified against live Postgres (docker `db` on :5433) — smoke test 10.8 asserts `langgraph.*` tables committed after lifespan shutdown; re-confirmed 2026-10-12 by a cold-DB `make dev-agent` boot (no schemas before → all four `checkpoint_*` tables after).

## 3. Migration: thin metadata table (in `agent.*`)

- [x] 3.1 Created `backend/agent_py/migrations/versions/0002_runs_metadata.py`. The `upgrade()` body: `CREATE SCHEMA IF NOT EXISTS agent` + `agent.runs` table + `runs_user_started_idx` index.
- [x] 3.2 `downgrade()` reverses all of the above.
- [x] 3.3 Verified against live Postgres — smoke test 10.8 asserts `agent.runs` + `agent.alembic_version` committed after lifespan shutdown; re-confirmed by the same cold-DB `make dev-agent` boot.

## 4. Add dev upgrade hook

- [x] 4.1 Create `backend/agent_py/migrations/_dev_upgrade_hook.py` — thin `run_upgrade_head()` async function that calls `alembic.command.upgrade(cfg, "head")` against the configured DSN.
- [x] 4.2 Hook into `agent/main.py` lifespan startup: `await run_upgrade_head()` runs automatically on lifespan startup when `database_url` is set AND `migrations_disabled=false`. Same posture as the Go backend's golang-migrate on boot — one command = migrations + serve. Compose prod sets `PYTHON_AGENT_MIGRATIONS_DISABLED=true` and relies on the dedicated `agent-migrate` one-shot service instead.
- [x] 4.3 Verified 2026-10-12: cold DB (no `agent`/`langgraph` schemas) + `make dev-agent` → inline alembic applied 0001+0002, both schemas + tables committed (checked via information_schema after boot).

## 5. Wire langGraph PostgresSaver

- [x] 5.1 Modify `backend/agent_py/agent/graph.py` — change `builder.compile()` to `builder.compile(checkpointer=AsyncPostgresSaver.from_conn_string(settings.database_url))`. The DSN's `options=-c search_path=langgraph,agent,public` controls schema location; the `langgraph` schema is pre-created by our `0001` alembic revision.
- [x] 5.2 Add a `await saver.setup()` call once on app startup (after alembic migrations) to ensure langGraph's `checkpoint_*` tables exist — runs `MIGRATIONS` list against the configured connection, idempotent.
- [x] 5.3 Test (`backend/agent_py/tests/test_graph_persistence.py`): committed `e09f86b` — covers compiled-graph-carries-saver, per-node checkpoints land in `langgraph.checkpoints`, `graph.get_state(config)` round-trips post-execution state.
  - `TestCompile_HasPostgresSaver` — assert the compiled graph's `checkpointer` is an instance of `AsyncPostgresSaver`.
  - `TestGetState_AfterAstream` — start a graph run with `thread_id="<uuid>"`, complete two nodes, call `graph.get_state(config)`; assert returned state matches the last node's output.
  - `TestResume_FromCheckpoint` — start a run with `thread_id="<uuid>"`, complete all nodes; call `graph.get_state(config)` for the same `thread_id`; assert the full state is returned (no in-memory dict, real round-trip through `langgraph.*` schema in Postgres).

## 6. Add RunMetadata (thin metadata layer)

- [x] 6.1 Create `backend/agent_py/agent/run_metadata.py` — `RunMetadata` class. Constructor takes `asyncpg.Pool`. Connection string is constructed once in `agent/main.py` from `settings.database_url` with `options=-c search_path=langgraph,agent,public` appended if absent.
- [x] 6.2 `create_run(user_id) -> uuid` — generates uuid4 server-side (Python `uuid.uuid4()`), INSERTs into `agent.runs`. Returns the uuid.
- [x] 6.3 `mark_completed(run_id, total_tokens)` — UPDATE `agent.runs` set `status='completed'`, `ended_at=now()`, `total_tokens=<int>`.
- [x] 6.4 `mark_failed(run_id, error_code)` — UPDATE `agent.runs` set `status='failed'`, `ended_at=now()`, `error_code=<code>`.
- [x] 6.5 `get_run(run_id, user_id) -> RunMetadata | None` — SELECT from `agent.runs WHERE id = $1 AND user_id = $2`. Returns `None` if not found or `user_id` mismatch.
- [x] 6.6 `list_runs_for_user(user_id, limit, offset) -> list[RunSummary]` — SELECT id, status, started_at, ended_at, total_tokens, error_code from `agent.runs WHERE user_id = $1 ORDER BY started_at DESC LIMIT $2 OFFSET $3`.
- [x] 6.7 `RunSummary` pydantic model: `id`, `status`, `started_at`, `ended_at`, `total_tokens`, `error_code`.
- [x] 6.8 All async methods. Pool acquire timeout 5s; transient `asyncpg.PostgresConnectionError` triggers 3-retry exponential backoff (100ms, 200ms, 400ms) then raises `RunMetadataUnavailable`.
- [x] 6.9 All reads include `user_id` in SQL `WHERE`. Defence in depth.

## 7. Wire RunMetadata into `POST /agent/run`

- [x] 7.1 `backend/agent_py/agent/main.py` — request body schema gains `run_id: str | None = None`. Validate as uuid4 if present (HTTP 400 otherwise).
- [x] 7.2 Mint path: when `run_id is None`, call `RunMetadata.create_run(user_id)` BEFORE invoking the graph. Emit `event: run_id\ndata: {"run_id": "<uuid>"}\n\n` *before* `event: plan`.
- [x] 7.3 Resume path: when `run_id is not None`, call `RunMetadata.get_run(run_id, user_id)`. If returns a non-terminal row, build `config = {"configurable": {"thread_id": str(run_id)}}`, call `graph.get_state(config)`, and use the returned state as the executor's initial state. Emit `event: note\ndata: {"kind": "resumed", "last_step_id": "<id>"}\n\n` *before* `event: plan` when `last_step_id` is not None. If `get_run` returns `None` (not found / wrong user / terminal), mint a fresh `run_id` via `RunMetadata.create_run(user_id)` and emit `event: note\ndata: {"kind": "fresh_run", "run_id": "<uuid>"}\n\n`.
- [x] 7.4 Pass `config={"configurable": {"thread_id": str(run_id)}}` to `graph.astream(state, config=config)` — every node transition is automatically checkpointed by langGraph into `langgraph.*`.
- [x] 7.5 Terminal persistence: in `agent/main.py` SSE consumer, on `done` emit `RunMetadata.mark_completed(run_id, total_tokens)`. On `error{code != "agent_unavailable"}` emit `RunMetadata.mark_failed(run_id, error_code)`. On `agent_unavailable` (langGraph checkpoint write failed) the run row stays in `running`; future follow-up adds a stale-run sweeper.
- [x] 7.6 Errors from `RunMetadata` calls are logged at WARN with OTel span `run_metadata_error` and translated to SSE `error{code:"run_metadata_unavailable"}` if a metadata call can't persist. Non-fatal: the run still completes its plan via langGraph; only metadata is lost.

## 8. Add retention cron

- [x] 8.1 `backend/agent_py/agent/retention.py` — `RetentionCron` class. Constructor takes `RunMetadata`, `Settings`. `start()` spawns an `asyncio` task. `stop()` cancels and awaits it (called on FastAPI shutdown).
- [x] 8.2 The cron body: `await asyncio.sleep(PYTHON_AGENT_RETENTION_INTERVAL_SECONDS)` then `DELETE FROM agent.runs WHERE ended_at < now() - interval 'PYTHON_AGENT_RETENTION_DAYS day' AND status IN ('completed', 'failed')`. Returns `deleted_count`.
- [x] 8.3 Wrap each iteration in OTel span `agent.run.retention` with attribute `deleted_count`. Increment Prometheus counter `agent_run_retention_deleted_total` by `deleted_count`.
- [x] 8.4 Wire into `agent/main.py` lifespan startup alongside `RunMetadata`. Spawn only when `PYTHON_AGENT_RETENTION_ENABLED=true` (default true).

## 9. Wire compose

- [x] 9.1 Add `agent.environment` to `docker-compose.yml`: `PYTHON_AGENT_DATABASE_URL=postgresql://${POSTGRES_USER}:${POSTGRES_PASSWORD}@db:5432/${POSTGRES_DB}?options=-c search_path=langgraph,agent,public`.
- [x] 9.2 Add `agent.depends_on: db (condition: service_healthy)` to compose.
- [x] 9.3 Add `agent-migrate` one-shot service to `docker-compose.yml` (`profiles: ["app"]`):
  - Same build context as `agent`.
  - `command: alembic upgrade head`.
  - `depends_on: db (condition: service_healthy)`.
  - Exits 0 on success, non-zero on failure.
- [x] 9.4 Add `agent.depends_on: agent-migrate (condition: service_completed_successfully)`.
- [x] 9.5 Add `PYTHON_AGENT_DATABASE_URL` to `backend/.env.example` (commented out; documented as required when `AGENT_ENABLED=true`).
- [ ] 9.6 (moved) Verify `make up` brings `db + redis + jaeger + backend + agent-migrate + agent + frontend` up. `make dev` boots backend + frontend; agent runs in compose (production-style) OR locally (dev-style with inline migrations). **Partial progress**: smoke caught two real Dockerfile bugs (missing `alembic.ini`/`migrations/` copy, missing `libpq5` for psycopg runtime) — both fixed in commit `4b75a96`. The full-stack exit-0 assertion is deferred to the rollout change → see `agent-production-rollout/1.3`.

## 10. Tests

**Test infra note:** live-Postgres tests are gated on `TEST_ASYNC_PG_DSN` (self-skip when unset) — same posture as the Go integration tests' `t.Skip()` on missing `dbURL`. Local run: `TEST_ASYNC_PG_DSN="postgresql://postgres:postgres@localhost:5433/nyx?sslmode=disable" pytest`. CI spins a testcontainer and exports the var.

- [x] 10.7 (`test_asyncpg_pool_init.py`) — library-API-contract regression guards:
  - `test_create_pool_requires_await_before_acquire` — pins asyncpg's lazy-init contract.
  - `test_build_pool_returns_initialized_pool` — fails if the `await pool` line inside `build_pool` ever regresses (this is the "pool is not initialized" bug guard).
- [x] 10.8 (`test_smoke_run_state.py`) — full-lifespan smoke tests against live PG:
  - `test_lifespan_creates_schemas_and_tables` — runs the real lifespan; asserts `langgraph.*` + `agent.*` schemas/tables exist AFTER shutdown (catches uncommitted/rolled-back DDL), and asserts no leak of table names into `public`. **Caught the leaked-`public.checkpoint_*` residue on first run.**
  - `test_metadata_roundtrip_after_lifespan` — create → get → mark_completed against the live schema; per-user scoping asserted.
- [x] 10.6 Full Python test suite (`cd backend/agent_py && pytest`) passes — 39 passed. Includes updating `test_run.py::test_agent_run_emits_sse_frame_sequence` to the new frame order (`run_id` → `note{fresh_run}` → `plan`).
- [x] 10.1 `backend/agent_py/tests/test_run_metadata.py` — committed `0e696b7` — retention-delete, pagination, pool-exhaustion cases. The pool-exhaustion test caught a real `_with_retry` bug: `asyncio.TimeoutError` from `pool.acquire(timeout=...)` leaked out instead of becoming `RunMetadataUnavailable`; fixed in the same commit.
- [x] 10.2 `backend/agent_py/tests/test_graph_persistence.py` — (per task 5.3). Three tests cover saver-wiring, per-node checkpoint write, and `get_state` round-trip.
- [x] 10.3 `backend/agent_py/tests/test_run_resume.py` — committed `b029dd7` — two tests: planner doesn't re-run on resume, executor skips completed steps on resume.
- [x] 10.4 Wire `alembic check` into CI — committed `e73339c` — new `agent-alembic-check` job gated on `backend/agent_py/migrations/**` path-filter; uses `alembic upgrade head` against a fresh DB (the project's `alembic check` requires a SQLAlchemy MetaData object which the chain doesn't register).
- [x] 10.5 Full backend test suite (`cd backend && SKIP_CONTAINERS=true go test ./...`) passes with zero regressions — 20 packages, all `ok`, fresh run (no cache).

## 11. Reframe `FUTURE_AGENT.md`

- [x] 11.1 Tick `FUTURE_AGENT.md` §1.1 items as they land:
  - `[x]` **Run / Step structs flowed through `context.Context`** — *(N/A: implementation moved to Python; the Go-side typed values are no longer relevant.)*
  - `[x]` **DB-backed run state for crash recovery** — landed in `openspec/changes/agent-run-state/` via langGraph `PostgresSaver` (in `langgraph.*`) + thin `agent.runs` metadata table (in `agent.*`).
  - `[x]` **Run ttl** — landed in `openspec/changes/agent-run-state/` (cron over `agent.runs`).

## 12. Dev-environment flip

- [x] 12.1 Add `PYTHON_AGENT_DATABASE_URL=postgresql://nyx:nyx@localhost:5433/nyx?options=-c search_path=langgraph,agent,public` (commented) to `backend/.env.example`. Verify `make dev` boots the agent path with inline migrations.
- [x] 12.2 Add `backend/agent_py/.env.example` with `PYTHON_AGENT_DATABASE_URL` and the retention envs.

## 13. Deferred to `agent-production-rollout`

> The rollout gates below are shared with `agent-loop` and `agent-langgraph`. They moved out of this change to a single rollout change so the three implementation changes archive cleanly while the rollout work tracks as one workstream. See `openspec/changes/agent-production-rollout/tasks.md`.

- [x] 13.2 Document the rollout in deploy notes: `agent-migrate` runs as part of the release pipeline before `agent` replicas roll out. Operationally: `alembic current` on the production agent-migrate exit confirms the head revision is applied. — Documented at `docs/agent-deployment.md` (commit `3d26f1b`). Two schemas (`langgraph`, `agent`) reachable via the configured DSN; `public` is owned by Go and the Python service never reads it. **Kept in-place as done**: this was the implementation-side doc and it shipped with this change.
- [ ] 13.1 (moved) 1-week CI green before flipping production traffic to `AGENT_ENABLED=true` → see `agent-production-rollout/2.1`.
- [ ] 13.3 (moved) Rollback drill (drop both schemas in staging, verify `/api/chat` reverts to single-run) → see `agent-production-rollout/2.4`. Runbook written at `docs/runbooks/agent-run-state-rollback.md` (commit `52cd2c0`).
- [ ] 13.4 (moved) Archive `agent-run-state` → superseded: this change archives now (implementation complete); the rollout change archives when 13.1 + 13.3 clear → see `agent-production-rollout/2.5`.
