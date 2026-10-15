# Proposal: Agent Run State (Postgres persistence via langGraph PostgresSaver)

## Why

The Plan-and-Execute agent (`openspec/changes/agent-langgraph/`) is in production. Today, `AgentState` flows through the langGraph state graph for the lifetime of one HTTP request and is then garbage-collected. The langGraph `StateGraph` is compiled without a `checkpointer=` — there is no built-in persistence. A backend or Python-service restart mid-run drops everything, including the tokens already spent on completed steps and the user's intermediate progress.

This change adds **Postgres-backed run persistence** so runs survive process restarts and resume from the last completed step. `FUTURE_AGENT.md` §1.1 is the source of intent; this proposal is the slice 2 follow-up.

**Decision (resolved during design):** state persistence uses **langGraph's `AsyncPostgresSaver`** as the checkpoint mechanism (built-in `thread_id` / `checkpoint_id` semantics, mid-iteration `react` resume for free, alembic migrations included), plus a **thin `agent.runs` metadata table** we own for the user-facing queries that langGraph's checkpoint blob doesn't expose (`list_runs_for_user`, retention cron, `total_tokens` / `error_code` aggregates). Postgres is the right tier because:

1. langGraph's `AsyncPostgresSaver` is the canonical persistence path for our orchestrator — used in production by every serious langGraph deployment, maintained by langChain, async-first, alembic-native. Reimplementing checkpoint storage would duplicate ~250 LOC of code langGraph already maintains.
2. Mid-iteration `react` resume (a slice-4 prerequisite per `FUTURE_AGENT.md` §1.3) is free with `PostgresSaver`. Building our own state blob would mean building it again.
3. Slice 5 (cross-session memory, `FUTURE_AGENT.md` §2.1) will join runs against `chat_sessions` / `chat_messages`. langGraph's checkpoint tables + our `agent.runs` metadata all live in the dedicated `agent` schema, so the cross-schema join with Go's `public.*` is trivial.
4. Run state is durable user work (tokens already spent). `synchronous_commit=on` over Postgres WAL is the standard answer; langGraph inherits this from `psycopg` / asyncpg defaults.

The Go chat proxy (`internal/chat/agent_proxy.go`), RAG bridge, MCP, tool registry, and provider seam are unchanged.

## What Changes

- **NEW** three Postgres schemas with clean ownership boundaries — separate from each other, separate migration chains:
  - `public` (existing, Go-owned, `golang-migrate`) — unchanged.
  - `langgraph` (NEW, framework-owned by langGraph) — holds langGraph's checkpoint tables (`checkpoint_*`). langGraph does NOT use alembic for these tables; it ships a hardcoded MIGRATIONS list and applies them via `AsyncPostgresSaver.setup()` at app startup. Our `0001` alembic revision just creates the empty schema; langGraph's tables land there when `setup()` runs.
  - `agent` (NEW, Python agent-owned, our alembic) — holds our metadata tables (`runs`) and our `alembic_version` row. Clean blast radius for our future schema changes.
  - DSN carries `?options=-c search_path=langgraph,agent` so unqualified names in Python resolve left-to-right (langGraph first, ours second). DDL lands in the first schema; DML resolves to the first match. One pool, two new schemas — `public` is not in our search_path because we never need to touch Go's tables.
- **NEW** `backend/agent_py/migrations/` — Python-owned **alembic** migration chain, **not** in `backend/migrations/` (golang-migrate never touches it). Uses SQLAlchemy **Core only** — `create_async_engine` for the connection abstraction, no `declarative_base`, no model classes. The chain manages BOTH langGraph's checkpoint tables (via `langgraph-checkpoint-postgres`'s built-in alembic migration) AND our thin `agent.runs` metadata table.
  - `backend/agent_py/alembic.ini` — alembic config, single env-var-driven database URL (`PYTHON_AGENT_DATABASE_URL`).
  - `backend/agent_py/migrations/env.py` — async `run_sync_migrations()` glue. Sets `version_table_schema='agent'` so our `alembic_version` lands in the `agent` schema. DSN carries `options=-c search_path=langgraph,agent`.
  - `backend/agent_py/migrations/script.py.mako` — alembic template (vendored once).
  - `0001_langgraph_checkpoint.py` — creates the `langgraph` schema (idempotent: `CREATE SCHEMA IF NOT EXISTS langgraph`). The `checkpoint_*` tables are created later by `await AsyncPostgresSaver.setup()` at app startup; this revision does NOT touch them. We don't author any DDL for langGraph's tables.
  - `0002_runs_metadata.py` — our thin metadata table:
    - `op.execute("CREATE SCHEMA IF NOT EXISTS agent")` (idempotent)
    - `op.create_table("runs", …, schema="agent")` with `(id uuid PK, user_id bigint NOT NULL, status text NOT NULL, total_tokens bigint NOT NULL DEFAULT 0, error_code text NULL, started_at timestamptz NOT NULL DEFAULT now(), ended_at timestamptz NULL, archived_at timestamptz NULL)`
    - `op.create_index("runs_user_started_idx", "runs", ["user_id", "started_at"], schema="agent", unique=False)`
- **NEW** `backend/agent_py/migrations/_dev_upgrade_hook.py` — thin `alembic upgrade head` wrapper callable from FastAPI's lifespan, used in dev for fast iteration. **Not used in prod** — prod runs alembic as a dedicated one-shot compose step.
- **MODIFIED** `docker-compose.yml` — adds `agent-migrate` one-shot service (`profiles: ["app"]`) running `alembic upgrade head` with `depends_on: db (service_healthy)`. `agent` service depends on `agent-migrate` having completed before booting AND sets `PYTHON_AGENT_MIGRATIONS_DISABLED=true` so the `agent` replicas don't race the migration runner. In dev (`make dev-agent`), the FastAPI lifespan runs alembic inline by default — same posture as the Go backend's golang-migrate on boot: one command, migrations + serve.
- **MODIFIED** `backend/agent_py/agent/graph.py` — compile the `StateGraph` with `checkpointer=AsyncPostgresSaver.from_conn_string(settings.database_url)`. langGraph's `from_conn_string` accepts no `schema_name` kwarg; schema location is controlled by (a) the DSN's `?options=-c search_path=langgraph,agent` and (b) the pre-existing `langgraph` schema created by our `0001` revision. When `await saver.setup()` runs, langGraph's hardcoded MIGRATIONS list executes `CREATE TABLE IF NOT EXISTS checkpoint_*` against the connection — unqualified names resolve to `langgraph.checkpoint_*` per the search_path. Single-line change that gives us thread-scoped persistence, mid-iteration resume, and `graph.get_state(config)` for inspection.
- **NEW** `backend/agent_py/agent/run_metadata.py` — thin metadata layer (~50 LOC). Holds an `asyncpg.Pool` with DSN `options=-c search_path=langgraph,agent`. Methods:
  - `create_run(user_id) -> uuid` — mint uuid4, INSERT into `agent.runs` (`status='running'`, `started_at=now()`). Returns uuid.
  - `mark_completed(run_id, total_tokens)` / `mark_failed(run_id, error_code)` — UPDATE `agent.runs` set terminal state + `ended_at`.
  - `get_run(run_id, user_id) -> RunMetadata | None` — SELECT one row, `user_id` filter (defence in depth).
  - `list_runs_for_user(user_id, limit, offset) -> list[RunSummary]` — for the user-facing history UX.
  - **No** `append_step` / `complete_step` — langGraph checkpoints step boundaries automatically via `PostgresSaver`.
- **MODIFIED** `backend/agent_py/agent/main.py` — `POST /agent/run` now:
  - Accepts optional `run_id` in the request body (clients pass it back for resume). If absent, mints one via `RunMetadata.create_run` and emits a new first event `run_id`.
  - If `run_id` is present, calls `RunMetadata.get_run(run_id, user_id)`. If found and non-terminal, builds `config={"configurable":{"thread_id": str(run_id)}}`, calls `graph.get_state(config)` to load the latest `AgentState`, seeds the request with that state, and emits `note{kind:"resumed", "last_step_id": "..."}` before `plan`. If `None` (not found, wrong user, or terminal), mints a fresh `run_id` and emits `note{kind:"fresh_run"}`.
  - Wraps the langGraph execution: pass `config` to `graph.astream(state, config=config)` — every node transition is automatically checkpointed. On terminal `done`, calls `RunMetadata.mark_completed(run_id, total_tokens)`. On terminal `error{code != "agent_unavailable"}`, calls `RunMetadata.mark_failed(run_id, error_code)`. langGraph state checkpointing handles step boundaries.
- **NEW** `backend/agent_py/agent/retention.py` — runs every hour (Python `asyncio` task spawned on app startup). `DELETE FROM agent.runs WHERE ended_at < now() - interval 'PYTHON_AGENT_RETENTION_DAYS' AND status IN ('completed', 'failed')`. **Does NOT touch langGraph checkpoint tables** — those are pruned separately by langGraph's own retention (configurable via `langgraph_checkpoint_kwargs` if needed). Hard-delete; the `archived_at` column is reserved for a future soft-delete + GDPR-erase split.
- **MODIFIED** `backend/agent_py/pyproject.toml` — add `asyncpg`, `alembic`, `sqlalchemy[asyncio]` (Core only), `langgraph-checkpoint-postgres`. Compose env: `PYTHON_AGENT_DATABASE_URL=postgresql://${POSTGRES_USER}:${POSTGRES_PASSWORD}@db:5432/${POSTGRES_DB}?options=-c search_path=langgraph,agent`.
- **MODIFIED** `docker-compose.yml` — `agent` service gains `PYTHON_AGENT_DATABASE_URL` env (DSN with `options=-c search_path=langgraph,agent`); `depends_on: db (condition: service_healthy)` added. Optional dedicated `agent-migrate` one-shot service (`profiles: ["app"]`) runs `alembic upgrade head` before `agent` starts; in dev, migrations run inline on app startup via `_dev_upgrade_hook.py`. Both paths use the same alembic env.
- **MODIFIED** `FUTURE_AGENT.md` §1.1 — items `[ ]` → `[x]` as they land.
- **BREAKING**: none. The `POST /agent/run` request body is additive (`run_id` optional). The SSE envelope is additive (new `run_id` event kind, `note{kind:"resumed"}`, `note{kind:"fresh_run"}`, `error{code:"run_metadata_unavailable"}` — old clients ignore unknown kinds).

## Capabilities

### New Capabilities

- `agent-run-store-py`: Postgres-backed persistence for agent runs in a dedicated `agent` schema (separate from the Go backend's `public` schema). State checkpointing uses **langGraph's `AsyncPostgresSaver`** (built-in thread_id semantics, alembic migrations included). A thin `agent.runs` metadata layer (`RunMetadata` class, ~50 LOC) owns the user-facing queries: `list_runs_for_user`, retention cron over `ended_at`, terminal `mark_completed` / `mark_failed`. Migration chain uses **alembic + SQLAlchemy Core** (Core only, no ORM models), matching the LangGraph ecosystem's own choice. Python service owns connection lifecycle (pool created on app startup, closed on shutdown). Retention cron lives in the same service.

### Modified Capabilities

- `agent-python-service`: `POST /agent/run` mints/resumes `run_id`, defers state persistence to langGraph's `PostgresSaver`, and emits `run_id` / `note{kind:"resumed"}` / `note{kind:"fresh_run"}` SSE frames. Wire contract is additive.

## Impact

- **Code added**:
  - `backend/agent_py/migrations/env.py` (~60 LOC) — alembic async env, `version_table_schema='agent'`, env-driven DSN.
  - `backend/agent_py/migrations/script.py.mako` (~30 LOC, vendored once) — alembic revision template.
  - `backend/agent_py/migrations/versions/0001_langgraph_checkpoint.py` (~10 LOC) — `CREATE SCHEMA IF NOT EXISTS langgraph`. langGraph's own `setup()` (called once at app startup, idempotent) creates the `checkpoint_*` tables there. This revision does not author DDL for langGraph's tables.
  - `backend/agent_py/migrations/versions/0002_runs_metadata.py` (~40 LOC) — creates `agent.runs` metadata table + index.
  - `backend/agent_py/migrations/_dev_upgrade_hook.py` (~30 LOC) — thin alembic wrapper callable from FastAPI lifespan for `make dev` fast iteration.
  - `backend/agent_py/agent/run_metadata.py` (~50 LOC) — thin `RunMetadata` class with `asyncpg.Pool`, methods above. **No** step-persistence methods.
  - `backend/agent_py/agent/retention.py` (~60 LOC) — hourly cron loop with structured shutdown; deletes only from `agent.runs`, not langGraph checkpoint tables.
  - `backend/agent_py/tests/test_run_metadata.py` (~150 LOC) — `testcontainers-postgres` happy paths + per-user scoping; no per-step tests (langGraph owns that).
  - `backend/agent_py/tests/test_run_resume.py` (~150 LOC) — end-to-end with FastAPI + `httpx.AsyncClient` against a testcontainer Postgres: start run → kill mid-execution → restart with same `run_id` → assert resumed state matches the last `PostgresSaver` checkpoint.
  - `backend/agent_py/tests/test_graph_persistence.py` (~150 LOC) — verify the compiled graph carries `PostgresSaver`; assert `graph.get_state(config)` returns the expected checkpoint after `astream`.
- **Code modified**:
  - `backend/agent_py/agent/graph.py` — 1 LOC change: pass `checkpointer=AsyncPostgresSaver.from_conn_string(settings.database_url)` to `builder.compile(...)`.
  - `backend/agent_py/agent/main.py` — ~80 LOC of glue around the existing `astream` loop: mint `run_id`, build `config`, call `get_state` on resume, emit new SSE frames, call `RunMetadata.mark_*` on terminal events.
  - `backend/agent_py/agent/executor.py` — no change (langGraph checkpoints step boundaries).
  - `backend/agent_py/agent/types.py` — `ChatRequest` gains optional `run_id: str | None`. `Event` discriminated union gains `RunID(run_id: str)` and `Note(kind='resumed', last_step_id: str)` and `Note(kind='fresh_run', run_id: str)`.
  - `backend/agent_py/agent/config.py` — `database_url: str` pydantic-settings field.
  - `backend/agent_py/pyproject.toml` — `asyncpg`, `alembic`, `sqlalchemy[asyncio]`, `langgraph-checkpoint-postgres` added to runtime deps.
  - `backend/agent_py/Dockerfile` — no change (all four are pip-installable).
  - `docker-compose.yml` — `agent.environment` gains `PYTHON_AGENT_DATABASE_URL`; `agent.depends_on` gains `db (service_healthy)`. Optional `agent-migrate` one-shot service for prod rollout.
  - `FUTURE_AGENT.md` §1.1 — tick boxes.
- **Dependencies**:
  - Python: `asyncpg` (new runtime dep), `alembic` + `sqlalchemy[asyncio]` (Core only, no ORM), `langgraph-checkpoint-postgres` (the framework's own persistence adapter; pin to a known-good version).
  - Go: none new.
- **Config (env, additive only)**:
  - `PYTHON_AGENT_DATABASE_URL` (required when `AGENT_ENABLED=true`) — Postgres DSN with `options=-c search_path=langgraph,agent`.
  - `PYTHON_AGENT_RETENTION_DAYS` (default 30) — applies only to `agent.runs` (the metadata table).
  - `PYTHON_AGENT_RETENTION_INTERVAL_SECONDS` (default 3600) — how often the cron fires.
- **API**: `POST /api/chat` request body unchanged. The Python `/agent/run` request gains an optional `run_id`. SSE envelope gains `run_id` (first event of every run), `note{kind:"resumed"}` (when resuming from a live run), `note{kind:"fresh_run"}` (when the echoed `run_id` was not found / wrong user / terminal), and `error{code:"run_metadata_unavailable"}` (metadata layer failures — langGraph checkpoint failures translate to existing `error{code:"agent_unavailable"}` and terminate the live run). All existing kinds unchanged.
- **Frontend**: unchanged for slice 2. The `run_id` echo round-trip is wired but not yet rendered. UX (history view) lands with slice 5.
- **Database**: three Postgres schemas with disjoint ownership. `public` (Go, golang-migrate) unchanged. `langgraph.*` holds langGraph's checkpoint tables (created by langGraph's own alembic, invoked from our `0001` revision). `agent.*` holds our metadata (`runs`, future tables). Same `pgvector/pgvector:pg17` compose service, no new infrastructure. No cross-schema joins from Go.
- **Observability**:
  - New OTel spans: `agent.run_metadata.create`, `agent.run_metadata.update`, `agent.run_metadata.list`, `agent.run.retention`. Attributes: `run_id`, `user_id`, `tokens_used`. langGraph's own checkpoint spans (`langgraph.checkpoint.write`, `langgraph.checkpoint.read`) flow through unchanged.
  - New Prometheus counters: `agent_run_metadata_total{op,status}` (ops: `create`/`complete`/`fail`/`load`/`list`), `agent_run_resume_total{status}` (`fresh`/`resumed`/`expired`/`forbidden`), `agent_run_retention_deleted_total`.
- **Security**:
  - Per-owner scoping on every metadata read: `RunMetadata.get_run(run_id, user_id)` and `list_runs_for_user(user_id, ...)` filter on `user_id` in SQL. Defence in depth — the JWT is verified by Go before the proxy forwards, but the Python service re-checks.
  - `user_id` is read from the verified JWT in `internal/chat/agent_proxy.go` and forwarded as `X-Nyx-User-Id` (the same pattern the RAG bridge uses; see commit `d9d10b4`). Python does not verify JWT itself.
  - langGraph's `PostgresSaver` is keyed on `thread_id = str(run_id)`; the user_id filter on `agent.runs` is what enforces per-owner isolation at the metadata layer. langGraph's checkpoint blob itself is opaque — its contents are not exposed to other users because the `thread_id` is a server-minted uuid.
  - Connection string lives in env, not logs. `asyncpg` exceptions are wrapped; raw connection errors never reach the SSE wire.
- **Roadmap**:
  - Tick `FUTURE_AGENT.md` §1.1 items as they land (`Run`/`Step` typed values in `internal/reqctx/` marked N/A since implementation moved to Python; DB-backed run state; run TTL).
  - Slice 4 (`agent-tool-dispatch-retry`) builds directly on top of `PostgresSaver` — mid-iteration react resume is free; only retry-on-malformed-tool-args, transient tool execution, and the partial-failure banner need building.
  - Slice 5 (`agent-cross-session-memory`) joins `agent.runs` against slice 5's session tables (cross-schema join; ownerships stay split — Python owns `agent.*`, Go owns `public.*`).
