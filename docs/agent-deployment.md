# Agent deployment notes

Companion to `openspec/changes/agent-run-state/`. Covers the production deploy path: env var setup, migration ordering, rollout order, and the verification gate at each step.

For the recovery / drop-schemas path, see `docs/runbooks/agent-run-state-rollback.md` (the rollback drill — task 13.3).

## Topology (production, compose)

The production stack runs under `docker compose --profile app`. The agent slice adds two services to the existing `db + redis + jaeger + backend + frontend` set:

- **`agent-migrate`** — one-shot. Runs `alembic upgrade head` and exits. `restart: "no"`.
- **`agent`** — long-lived. The Python FastAPI service. Reads `PYTHON_AGENT_DATABASE_URL`; runs `await saver.setup()` on startup.

Both depend on `db` being healthy. The `agent` service additionally depends on `agent-migrate` having completed successfully before it starts, so the migration runner can never race a live replica on the same DB.

The existing `agent` dev path (`make dev-agent`) runs alembic **inline** on lifespan startup when `PYTHON_AGENT_MIGRATIONS_DISABLED` is unset (default behavior). Production sets `PYTHON_AGENT_MIGRATIONS_DISABLED=true` and uses the one-shot service instead, so replica rollout doesn't N× re-run alembic.

## Required env vars (production)

Set on the `agent` service in `docker-compose.yml`. None of these are required when `AGENT_ENABLED=false`; the existing chat path doesn't touch them.

| Var | Required? | Default | Notes |
| --- | --- | --- | --- |
| `AGENT_ENABLED` | yes | `false` | Flip to `true` to route `/api/chat` through the Python service. |
| `PYTHON_AGENT_DATABASE_URL` | yes (when `AGENT_ENABLED=true`) | unset | Must include `options=-c search_path=langgraph,agent`. The service refuses to start otherwise. |
| `PYTHON_AGENT_MIGRATIONS_DISABLED` | no | `false` | Set `true` in compose; `false` in dev. |
| `PYTHON_AGENT_RETENTION_ENABLED` | no | `true` | Hard-delete completed/failed metadata after the retention window. |
| `PYTHON_AGENT_RETENTION_DAYS` | no | `30` | |
| `PYTHON_AGENT_RETENTION_INTERVAL_SECONDS` | no | `3600` | |

The `PYTHON_AGENT_DATABASE_URL` shape: `postgresql://${POSTGRES_USER}:${POSTGRES_PASSWORD}@db:5432/${POSTGRES_DB}?options=-c%20search_path%3Dlanggraph%2Cagent`. The `options=` value is URL-encoded (`%20` for space, `%3D` for `=`) because compose interpolates unencoded values into the env literally; URL-encoding is the no-decoding-required form.

`PUBLIC` is intentionally **not** in the search path. The Python service never reads Go's tables. Adding `public` would be misleading documentation without changing runtime behavior.

## Rollout order

The release pipeline must apply changes in this order, otherwise a live replica can boot with a half-migrated schema:

1. **Pre-deploy**: roll the new `agent-migrate` image. The container runs `alembic upgrade head` against the production DB. The service exits 0 on success; exits non-zero on any SQL error in the migration chain.
2. **Verify migration**: `docker compose logs agent-migrate` shows the alembic version line for the new head. Cross-check with `psql -c "SELECT version_num FROM agent.alembic_version"` and confirm it matches the head revision. (One `agent-migrate` is enough; replicas don't re-run alembic.)
3. **Roll agent replicas**: the existing rolling-update strategy in the compose file. Each replica boots, runs `await saver.setup()` (idempotent), and accepts traffic. Because the `agent` service `depends_on: agent-migrate (condition: service_completed_successfully)`, no replica can start before step 1 succeeded.
4. **Flip `AGENT_ENABLED=true`**: this is a Go-side change. Until flipped, `/api/chat` is still the Go path. Once flipped, every `/api/chat` request goes Go → Python. The Python service is now load-bearing for the chat UX.
5. **Flip the Go proxy**: when rolling back the Go proxy's route is straightforward — set `AGENT_ENABLED=false` and restart Go. Existing in-flight runs on the Python side finish their stream; new requests go to Go.

## Verification at each step

### After step 1 (`agent-migrate` runs)

```bash
docker compose logs agent-migrate
# Expect: "Running upgrade 0001_langgraph_checkpoint, 0001 langgraph schema" + "Running upgrade 0001_langgraph_checkpoint -> 0002_runs_metadata, 0002 agent.runs"
# Expect exit 0.
```

```sql
SELECT schema_name FROM information_schema.schemata
  WHERE schema_name IN ('agent', 'langgraph');
-- Expect 2 rows.

SELECT count(*) FROM information_schema.tables
  WHERE table_schema = 'langgraph';
-- Expect 4 (langGraph's checkpoint_* tables).

SELECT count(*) FROM information_schema.tables
  WHERE table_schema = 'agent';
-- Expect 2 (runs + alembic_version).
```

### After step 3 (agent replicas rolled)

```bash
docker compose ps agent
# Expect: state = "Up" with healthy healthcheck.

curl -fsS http://localhost:9000/healthz
# Expect: {"status":"ok"}
```

### After step 4 (`AGENT_ENABLED=true`)

Smoke a single chat request end-to-end:

```bash
# Register a test user + get a token (existing flow).
TOKEN=$(curl -fsS -X POST http://localhost:8080/api/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"deploy_smoke_'$(date +%s)'","password":"testpass123"}' | jq -r .access_token)

# Send a chat.
curl -fsS -X POST http://localhost:8080/api/chat \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"hi"}]}' \
  -N | head -5
# Expect: run_id frame, note{fresh_run} frame, plan frame, then per-step frames.
```

Confirm a `agent.runs` row landed:

```sql
SELECT id, user_id, status, started_at FROM agent.runs
  ORDER BY started_at DESC LIMIT 1;
-- Expect: 1 row, status='completed' or 'failed', started_at within the last minute.
```

### After step 5 (proxy flip back)

```bash
curl -fsS -X POST http://localhost:8080/api/chat \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"hi"}]}' \
  -N | head -3
# Expect: stream opens with a delta frame (the Go path), no run_id / fresh_run frames.
```

## Rollback

See `docs/runbooks/agent-run-state-rollback.md`. The runbook is the authoritative procedure; this doc links to it rather than duplicating.

## Common gotchas

- **`PYTHON_AGENT_DATABASE_URL` without `options=-c search_path=langgraph,agent`**: the service refuses to start with a clear error. Don't add `,public`; DDL goes to the first schema in the search path, DML to the first match, and we never touch `public`. (Was a doc drift in the original change — the proposal mentioned `,public`; the spec landed on the two-schema form.)
- **`psycopg` ImportError on a slim Python image**: the `agent` Docker image needs `libpq5` in its apt deps. Without it, `import psycopg` raises `no pq wrapper available`. Symptom: agent container exits 1 on first request that needs the saver. Fix is in `backend/agent_py/Dockerfile` (added in commit `4b75a96`).
- **`alembic upgrade head` fails partway**: `agent-migrate` exits non-zero, compose refuses to start `agent` (depends_on condition). The DB may be in a half-migrated state. Don't panic — `alembic` is transactional per migration; either the failing migration's transaction rolled back, or the failure is post-commit. Re-run `alembic current` to see where you are, then `alembic upgrade head` to retry. If a migration is genuinely broken, the recovery is to drop the schemas and let the release pipeline re-apply (the runbook's path).
- **Compose `depends_on: agent-migrate (condition: service_completed_successfully)` doesn't fire on first deploy**: the service has never run, so its "completed successfully" history is empty. The first deploy waits for `agent-migrate` to actually run, not for an old "completed" flag. Subsequent deploys benefit from the dependency history. (If you see compose hang on first deploy, check that `agent-migrate` is actually starting — the `condition` doesn't kick in until the service runs once.)
