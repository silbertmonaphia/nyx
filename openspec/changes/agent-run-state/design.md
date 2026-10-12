# Design

## Context

The Plan-and-Execute agent (`openspec/changes/agent-langgraph/`) is in production. `AgentState` flows through the langGraph state graph for the lifetime of one HTTP request and is then garbage-collected. The `StateGraph` is compiled without a `checkpointer=` — there is no built-in persistence. A backend or Python-service restart mid-run drops everything, including the tokens already spent on completed steps and the user's intermediate progress. `FUTURE_AGENT.md` §1.1 is the source of intent; this design covers slice 2 of the agent roadmap.

This change adds **Postgres-backed run persistence** to the Python agent service so runs survive process restarts and resume from the last completed step. State persistence is delegated to **langGraph's `AsyncPostgresSaver`** (built-in `thread_id` / `checkpoint_id` semantics, mid-iteration `react` resume for free, alembic migrations included). A **thin `agent.runs` metadata table** owned by us covers the user-facing queries langGraph's checkpoint blob doesn't expose (`list_runs_for_user`, retention cron over `ended_at`, `total_tokens` / `error_code` aggregates). Both layers live in a dedicated `agent` Postgres schema separate from Go's `public`. The Go chat proxy, RAG bridge, MCP, tool registry, and provider seam are untouched.

## Goals / Non-Goals

**Goals:**
- Add crash-recovery for agent runs so a process restart doesn't lose already-spent tokens or in-flight progress.
- Resume semantics: client echoes `run_id` on retry; server rehydrates `AgentState` via `graph.get_state(config)` using langGraph's last checkpoint.
- Persist per-step state automatically via langGraph's `PostgresSaver` (every node transition becomes a checkpoint).
- Run TTL: hard-delete completed/failed metadata rows after `PYTHON_AGENT_RETENTION_DAYS` (default 30). langGraph checkpoint tables pruned via langGraph's own retention story.
- Schema isolation: Python owns `agent.*` (langGraph checkpoint tables + `agent.runs` + `agent.alembic_version`); Go continues to own `public.*`. No cross-schema joins from Go.
- Schema versioning via alembic (owned by Python). `golang-migrate` is not extended.
- Per-user scoping on every metadata read (`user_id` filter in SQL) — defence in depth even though the JWT is verified upstream.
- Observability continuous: OTel spans + Prometheus counters land in the same Jaeger / Prometheus the rest of the stack uses.

**Non-Goals:**
- UX rendering of run history (lands with slice 5 cross-session memory).
- DB-backed conversation history (`FUTURE_AGENT.md` §2.1).
- Task routing (`FUTURE_AGENT.md` §0.4) — unchanged, deferred.
- Fault tolerance beyond what `PostgresSaver` gives for free (`FUTURE_AGENT.md` §1.3) — retry-on-malformed-tool-args and planner-self-check are slice-4 work, but mid-iteration resume is already free here.
- Replacing the langGraph checkpointing story with a custom one. This slice adopts `PostgresSaver` as-is; future slices can layer (e.g., custom metadata extraction) on top.
- Cross-schema joins from Go. Go never reads `agent.*`; SQL queries in `internal/chat/` continue to assume `public.*`.

## Decisions

### 1. State persistence via langGraph's `AsyncPostgresSaver`; thin metadata table for user-facing queries

`AgentState` itself is checkpointed by langGraph's `PostgresSaver` automatically on every node transition. We do **not** reimplement step persistence. The compiled graph carries the saver:

```python
from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver
builder.compile(checkpointer=AsyncPostgresSaver.from_conn_string(settings.database_url))
```

`thread_id` is our `run_id` (uuid4). Resume is `graph.astream(state, config={"configurable":{"thread_id": str(run_id)}})`. State inspection for the resume path is `graph.get_state(config)` returning the latest `AgentState`.

We **do** own a thin metadata table `agent.runs` (id, user_id, status, total_tokens, error_code, started_at, ended_at, archived_at) because langGraph's checkpoint blob doesn't expose those fields in a queryable shape. This table backs the user-facing UX (`list_runs_for_user`) and the retention cron. ~50 LOC of `RunMetadata`.

**Alternatives considered:**
- *Custom schema for the state blob too (earlier rejected proposal)*: rejected after re-evaluation. Would have duplicated ~250 LOC of checkpoint lifecycle code (`append_step` / `complete_step`, `load_run`, transaction wrapping, retry logic) that langChain maintains for us. Lost mid-iteration resume (a slice-4 prerequisite) for a custom implementation.
- *PostgresSaver only, no metadata table*: rejected. The "list my last N runs for user X" UX needs indexed access to `user_id`, `started_at`, `status`, `total_tokens` — none of which langGraph exposes as first-class columns.
- *Redis for metadata, Postgres for state*: rejected. Two stores means the cross-schema join with slice-5 chat_sessions becomes harder; metadata is durable user work and belongs in Postgres.

### 2. Three Postgres schemas with disjoint ownership: `public` / `langgraph` / `agent`

Three Postgres schemas, one migration chain per runtime:

- `public` (Go-owned, `golang-migrate`) — unchanged. Existing `feed`, `user`, `chat_*` tables, no overlap.
- `langgraph` (langGraph framework-owned, langGraph's bundled alembic) — holds langGraph's `checkpoint_*` tables. Python only invokes langGraph's migration from our `0001` revision; we don't author DDL for this schema.
- `agent` (Python agent-owned, our alembic) — holds our metadata (`runs`, future slice-5 tables).

DSN carries `?options=-c search_path=langgraph,agent,public`. Postgres resolves unqualified names left-to-right; one pool, three schemas, no overlap. Go's `pgxpool` is not configured with `search_path` and stays on `public`.

**Alternatives considered:**
- *Two schemas (`public` + `agent` with langGraph stuffed in)*: rejected. Forced us to fight langGraph's natural schema via `search_path` tricks; risk that langGraph hard-codes a schema name in their DDL breaks the whole approach.
- *One shared `public` schema*: rejected. Migrations become a coordination problem between two runtimes.
- *Separate databases*: rejected. Same Postgres instance, two `CREATE DATABASE` lifecycles, two connection pools. Costs more than it buys.
- *Production-only separate Postgres role*: deferred. In compose, the same `nyx` user owns all three schemas. In production the role separation can be added later.

### 3. Alembic + SQLAlchemy Core (no ORM models)

Alembic is the community-dominant choice for FastAPI + Postgres production migrations; langGraph's own `langgraph-checkpoint-postgres` uses alembic internally for its checkpoint tables — we drive langGraph's migration from our chain (`0001_langgraph_checkpoint.py` programmatically invokes langGraph's built-in alembic upgrade). SQLAlchemy **Core only** — `create_async_engine` for the connection abstraction, no `declarative_base`, no model classes, no `Mapped[...]`. Our metadata table is created via `op.create_table(..., schema="agent")`; the data access stays raw `asyncpg`. Alembic only owns schema versioning.

`version_table_schema='agent'` keeps `alembic_version` in our schema.

**Alternatives considered:**
- *Two alembic configs (one for langGraph, one for ours)*: rejected. Splits the migration story; ordering bugs become possible (e.g., our `agent.runs` referenced before langGraph's tables exist).
- *yoyo-migrations*: would be the principled pick if "no SQLAlchemy whatsoever" were a hard requirement. Maintenance gap (~20 months between Dec 2022 and Aug 2024) and solo maintainer tip the balance to alembic for community alignment.

### 4. Python talks to Postgres directly, no Go bridge

The Python service holds an `asyncpg.Pool` (for `RunMetadata`) and a langGraph-managed `AsyncPostgresSaver` connection. Both write into our `agent` schema directly. The Go chat proxy (`internal/chat/agent_proxy.go`) still mediates `/api/chat` → `/agent/run` HTTP traffic — that's an HTTP proxy, not a DB bridge. For run state, there is no Go bridge.

**Alternatives considered:**
- *Python → Go → Postgres*: rejected. Adds a hop on the resume hot path; couples migration ownership to Go; the Go side becomes a thin DB proxy that owns nothing.
- *Python → MCP tools/call for state ops*: rejected. MCP exposes tools; state persistence is not a tool, it's a CRUD concern with its own connection pool, transaction model, and schema versioning.

### 5. Resume via `run_id` echo + `graph.get_state(config)`

`POST /agent/run` accepts an optional `run_id`. When present:
1. `RunMetadata.get_run(run_id, user_id)` checks the metadata row exists, is owned by this `user_id`, and has `status NOT IN ('completed', 'failed')`.
2. If yes: build `config = {"configurable": {"thread_id": str(run_id)}}`, call `graph.get_state(config)` to retrieve the latest `AgentState` langGraph checkpointed. Seed the executor's initial state with it. Emit `note{kind:"resumed", "last_step_id": ...}` before `plan`.
3. If `None` (not found / wrong user / terminal): mint a fresh `run_id` via `RunMetadata.create_run(user_id)`. Emit `note{kind:"fresh_run", "run_id": "<uuid>"}`. Original `run_id` (if any) is left untouched.

When `run_id` is absent: mint fresh, emit `event: run_id` first.

The graph is compiled with `PostgresSaver` once at app startup. Each run uses its own `config` (different `thread_id`); per-step persistence is automatic.

**Alternatives considered:**
- *Server-side session cookie / conversation id*: rejected. The frontend already manages JWT and conversation state; adding a third id type is gratuitous.
- *Manually rehydrate from `agent.runs` + a custom step table*: rejected. That's the original proposal we explicitly moved away from. langGraph already maintains this lifecycle.

### 6. Retention: separate cron for metadata, langGraph's own retention for checkpoints

`backend/agent_py/agent/retention.py` runs as an `asyncio` task spawned on app startup. Every `PYTHON_AGENT_RETENTION_INTERVAL_SECONDS` (default 3600s), it issues `DELETE FROM agent.runs WHERE ended_at < now() - interval 'PYTHON_AGENT_RETENTION_DAYS day' AND status IN ('completed', 'failed')`. Hard delete; `archived_at` reserved for future soft-delete.

langGraph's checkpoint tables are pruned via the framework's own retention config (`AsyncPostgresSaver.setup()` plus a periodic call to its `delete_thread` for completed threads), kept simple — we accept that orphaned checkpoints for non-recently-resumed runs may exist briefly until langGraph's cleanup runs.

**Alternatives considered:**
- *Postgres `pg_cron`*: rejected. Adds a Postgres extension to manage; agent service shouldn't depend on extension availability.
- *One cron for both metadata and checkpoints*: rejected. LangGraph's checkpoint lifecycle (including `delete_thread`) is a framework concern; coupling our retention to it would mean re-running langGraph's logic in our cron.

### 7. Compose: dedicated `agent-migrate` one-shot in prod, lifespan hook in dev

`docker-compose.yml` adds `agent-migrate` (`profiles: ["app"]`) which runs `alembic upgrade head` and exits 0 on success. This includes our `0001_langgraph_checkpoint.py` (which programmatically runs langGraph's built-in migration). The `agent` service has `depends_on: agent-migrate (condition: service_completed_successfully)`. In dev (`make dev`), the FastAPI lifespan hook `_dev_upgrade_hook.py` runs alembic inline so `make dev` brings the stack up in one command.

**Alternatives considered:**
- *Run migrations on app startup in prod too*: rejected. With multiple replicas, two pods racing on alembic is the classic "fail to upgrade" pattern.
- *Let langGraph's checkpointer auto-create tables on first use*: rejected. alembic + migration files is the audit-friendly story; auto-create is fine for the framework's own tests but not production.

### 8. `user_id` from header, re-checked in SQL

`internal/chat/agent_proxy.go` reads `user_id` from the verified JWT and forwards as `X-Nyx-User-Id` (the same pattern the RAG bridge uses, per commit `d9d10b4`). Python does not verify JWTs — the network boundary is the trust boundary at this hop. Every `RunMetadata.get_run(run_id, user_id)` and `list_runs_for_user(user_id, ...)` includes `user_id` in the SQL `WHERE` clause. Defence in depth.

**Alternatives considered:**
- *Python verifies JWT*: rejected. Two verifiers invite drift; one source of truth at the network edge.
- *Trust the header alone with no SQL filter*: rejected. A misrouted request that lands in the Python container with a forged header would bypass per-user scoping.

## Risks / Trade-offs

- **Tighter coupling to langGraph's checkpoint format** → if langChain renames a column in a future release, our resume breaks until we pin the version. Mitigated by pinning `langgraph-checkpoint-postgres` to a known-good version in `pyproject.toml`; the migration is owned by us, so a breaking change shows up in our CI alembic diff.
- **Checkpoint blob is opaque** → real-time observability queries like "how many runs called `list_feeds` last week" require JSONB path access into langGraph's checkpoint blob, which is awkward. The `agent.runs` metadata table covers the common UX queries; JSONB queries are acceptable for analytics but not for product features.
- **Direct DB connection adds a python failure mode** → if postgres is unreachable, every `RunMetadata` call fails. Mitigated: `asyncpg` pool with retry on transient errors (3 retry, 100ms exponential backoff); per-call failures are logged + OTel'd and translated to typed SSE `error{code:"run_metadata_unavailable"}` events (the request still completes if state persistence fails non-fatally). langGraph checkpoint failures translate to existing `error{code:"agent_unavailable"}` (the whole run terminates — that's the right posture because mid-iteration state can't be trusted).
- **Alembic with async** → alembic's sync engine is wrapped via `run_sync_migrations()`. Tested pattern; the only quirk is that `env.py` must use `connectable.connect()` not `engine.begin()` for the migration context.
- **Schema name in DSN** → typo in `search_path=langgraph,agent,public` silently puts tables in `public`. Mitigated: the first migration creates both `langgraph` and `agent` schemas and the runner asserts via `SHOW search_path` after connect that the search_path is exactly right. CI test asserts all three table groups (langGraph's, ours, Go's) resolve qualified.
- **Asyncpg pool exhaustion** → capped at `max_size=10` in dev, configurable via `PYTHON_AGENT_DATABASE_POOL_MAX_SIZE` in prod. Pool acquire timeout 5s; exceeding it returns `run_metadata_unavailable`.
- **Retention cron + in-flight runs** → retention only deletes runs where `status IN ('completed','failed')` and `ended_at IS NOT NULL`. A run that's still `running` and has no `ended_at` is safe by construction.
- **Clock skew between app + Postgres** → retention uses Postgres `now()`, not Python `datetime.now()`. The retention query is authoritative regardless of where the app container's clock is.
- **Two postgres clients in the stack** (`pgxpool` Go, `asyncpg` Python) → both standard, both vetted. Documented in `FUTURE_AGENT.md` §1.1 — accepted cost.
- **Wire contract drift on `run_id` echo** → the resume path adds new SSE frames (`run_id`, `note{kind:"resumed"}`, `note{kind:"fresh_run"}`). Old clients ignore unknown event kinds. The `run_id` field on the request body is optional; old clients that don't send it get a fresh run as before.
- **Two alembic version tables** → our `agent.alembic_version` (in `agent.*`) and langGraph's `checkpoint_migrations` (in `langgraph.*`). They serve different chains in different schemas; each is owned by one runtime.

## Migration Plan

1. **Add alembic scaffold** — `backend/agent_py/alembic.ini`, `migrations/env.py` (async), `migrations/script.py.mako`, `migrations/_dev_upgrade_hook.py`.
2. **Migration `0001_langgraph_checkpoint.py`** — invokes langGraph's bundled alembic pointed at the `langgraph` schema (e.g., `lg_cfg.set_main_option("sqlalchemy.url", settings.database_url)` + schema override). Creates langGraph's `checkpoint_*` tables in `langgraph.*`. langGraph's alembic config accepts a schema name (spike verifies the exact env var / kwarg).
3. **Migration `0002_runs_metadata.py`** — `CREATE SCHEMA agent` (idempotent), `agent.runs` metadata table + index, `downgrade()` reverses all.
4. **Wire langGraph saver** — modify `agent/graph.py`: pass `checkpointer=AsyncPostgresSaver.from_conn_string(settings.database_url, schema_name="langgraph")` (or equivalent) to `builder.compile(...)`. DSN carries `search_path=langgraph,agent,public`.
5. **Add RunMetadata** — `backend/agent_py/agent/run_metadata.py`: `create_run`, `mark_completed`, `mark_failed`, `get_run`, `list_runs_for_user`. All async, scoped by `user_id` in SQL.
6. **Wire into `POST /agent/run`** — `agent/main.py` mints/resumes `run_id`, calls `graph.get_state(config)` on resume, calls `RunMetadata.mark_*` on terminal events. Additive SSE frames: `run_id` first on fresh runs, `note{kind:"resumed"}` on resume, `note{kind:"fresh_run"}` on echoed-but-not-found.
7. **Add retention cron** — `backend/agent_py/agent/retention.py`: hourly `asyncio` task over `agent.runs` only.
8. **Compose** — `docker-compose.yml` `agent.environment` gains `PYTHON_AGENT_DATABASE_URL`; `agent.depends_on` gains `db (service_healthy)` and `agent-migrate (condition: service_completed_successfully)`. Add `agent-migrate` one-shot service.
9. **Tests** — `test_run_metadata.py` (testcontainers-postgres happy paths + per-user scoping), `test_run_resume.py` (FastAPI + httpx against testcontainer; start run → kill mid-execution → restart with same `run_id` → assert resumed state matches the last `PostgresSaver` checkpoint), `test_graph_persistence.py` (assert compiled graph carries `PostgresSaver`; assert `graph.get_state(config)` returns the expected checkpoint). Wire `alembic check` into CI.
10. **Update FUTURE_AGENT.md** — tick §1.1 items as they land.
11. **Cutover:**
    - Dev: `make dev` brings `db`, `redis`, `jaeger`, `backend`, `agent` (with inline alembic), `frontend` up.
    - Compose: `make up` brings infra + `agent-migrate` + `agent` + `frontend`.
    - Production flip: deploy with `PYTHON_AGENT_DATABASE_URL` set; `agent-migrate` runs as part of the release pipeline before `agent` replicas roll out.
12. **Rollback:**
    - Drop the `agent` and `langgraph` schemas entirely (`DROP SCHEMA agent CASCADE; DROP SCHEMA langgraph CASCADE`); alembic downgrade to 0000 (no-op). No Go data affected.
    - Set `PYTHON_AGENT_RETENTION_DAYS=0` to stop retention mid-rollout if needed.
    - Set `AGENT_ENABLED=false` to revert to single-turn chat; `graph.py` reverts to no checkpointer.
    - No Go rollback required — `internal/chat/agent_proxy.go` is unchanged.

## Open Questions

- *Should `agent.runs` carry `plan_json` for quick inspection, or always read it from the checkpoint?* — current answer: only when needed by the UX. The metadata table carries only user-facing fields (`user_id`, `status`, `total_tokens`, `error_code`, timestamps). `plan_json` is in the checkpoint blob; we expose it via `graph.get_state(config)` when the UX needs it.
- *Resume semantics for the in-flight React subgraph* — current answer: `PostgresSaver` checkpoints at every node transition (including inside the react subgraph since it's a sub-graph). Mid-iteration resume is now free. Slice 4 fault tolerance builds on this for retry-on-malformed-tool-args.
- *Should retention be soft-delete + GDPR erase, or hard-delete?* — current answer: hard-delete. GDPR review hasn't been done; the `archived_at` column is in place for a future soft-delete split when the review lands.
- *Does the langGraph `delete_thread` retention get called from our cron or from its own scheduler?* — current answer: rely on langGraph's default. If orphans become a problem, add a periodic `delete_thread` call to the cron in a follow-up.
- *Pin `langgraph-checkpoint-postgres` to which version?* — current answer: pin to the version langChain ships at the same release as our `langgraph` dep. Bump together.
- *What kwarg / env var does langGraph's `AsyncPostgresSaver` accept for custom schema?* — current answer: spike will determine (`schema_name=` constructor arg, `LANGGRAPH_CHECKPOINT_SCHEMA` env var, or alembic-config override). Whichever it is, our `0001` revision and `graph.py` use it to land in `langgraph.*`.
