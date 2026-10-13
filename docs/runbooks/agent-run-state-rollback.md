# Runbook: agent-run-state rollback drill

Exercises the recovery path for `openspec/changes/agent-run-state/`. Goal: prove that dropping both new schemas (`agent.*` and `langgraph.*`) reverts `/api/chat` to single-run behavior with no other changes — i.e. the new feature is **removable** without leaving the rest of the system in a broken state.

**When to run this**: before flipping `AGENT_ENABLED=true` in production (one of the rollout gates in task 13.1), and after every change to the migration chain or the saver wiring.

## What "rollback" means here

1. Drop both new Postgres schemas:
   ```sql
   DROP SCHEMA IF EXISTS agent CASCADE;
   DROP SCHEMA IF EXISTS langgraph CASCADE;
   ```
2. Leave `public.*` untouched (Go's tables live there; not part of this slice).
3. The Python service's env vars (`PYTHON_AGENT_DATABASE_URL`, `PYTHON_AGENT_RETENTION_*`) stay set — the goal is to verify graceful behavior with the schemas gone, not to undo the env config.
4. The Go service is unaffected — it never reads `agent.*` or `langgraph.*`.

## Pre-flight checklist

- [ ] **Backup first**. The rollback is destructive. `pg_dump --schema=agent --schema=langgraph` before dropping, even in staging.
- [ ] **Confirm staging, not prod**. Run `SELECT current_database(), inet_server_addr();` and verify it's not the production cluster.
- [ ] **No in-flight runs**. Staging traffic is paused for the drill window. If any user is mid-run, the resume path will read a missing schema and the run will fail to resume (it'll mint a fresh `run_id` — the contract holds, but the user's progress is lost).
- [ ] **Smoke before**. Run `tests/test_smoke_run_state.py` against the staging DB and confirm the schemas exist + the roundtrip works. That's the "before" baseline.
- [ ] **Window the drill**. ~10 min. Notify #ops-staging before starting.

## The drill

### 1. Drop the schemas

```bash
# From a host with psql + network access to the staging DB.
PGPASSWORD=$STAGING_PG_PASSWORD psql -h $STAGING_PG_HOST -U $STAGING_PG_USER -d $STAGING_PG_DB <<'SQL'
DROP SCHEMA IF EXISTS agent CASCADE;
DROP SCHEMA IF EXISTS langgraph CASCADE;
-- Sanity: should be empty now.
SELECT schema_name FROM information_schema.schemata
  WHERE schema_name IN ('agent', 'langgraph');
SQL
```

Expected: zero rows. Both schemas gone.

### 2. Restart the Python service

The service was last started when the schemas existed; `await saver.setup()` ran the `checkpoint_*` migrations into `langgraph.*`. After the drop, the service still has the open saver connection but the tables it wrote to are gone. A restart forces a clean re-init.

```bash
# Compose path:
docker compose --profile app restart agent

# Bare-metal / dev path:
make stop-agent && make dev-agent
```

### 3. Verify the service starts cleanly

Expected log lines on startup:

```
INFO  alembic.runtime.migration  Running upgrade  -> 0001_langgraph_checkpoint, 0001 langgraph schema
INFO  alembic.runtime.migration  Running upgrade 0001_langgraph_checkpoint -> 0002_runs_metadata, 0002 agent.runs
INFO  agent.checkpointer  setup() applied langGraph MIGRATIONS to langgraph.checkpoint_*
```

**What "passing" looks like**: alembic re-runs `0001` (creates `langgraph`), `0002` (creates `agent.runs`), the saver's `setup()` re-creates `langgraph.checkpoint_*`. The service is back to a consistent state — the rollback is fully recoverable.

**What "failing" looks like** (one of these means the drill failed):

- `alembic` errors mid-migration (e.g. an old alembic_version row references a now-missing revision).
- `setup()` errors (langGraph's hardcoded MIGRATIONS list expects the `langgraph` schema to be pre-created).
- The service exits with code 1 before `/healthz` returns 200.

If any of those happen, **STOP**. The rollback path has a bug. Investigate before declaring the drill complete.

### 4. Drive a single-turn chat

From a browser pointed at the staging frontend, send a chat message. Expected:

- A `run_id` frame first, then `note{kind:"fresh_run"}`, then a `plan` frame, then per-step frames, then `done` + `terminator`. The wire shape is unchanged from the pre-rollback run.
- The assistant bubble renders the response.
- No `error` frame on the happy path.

If `/api/chat` returns 5xx, or the response stream contains `error{code: "agent_unavailable"}`, the saver wiring is broken and the rollback needs to be backed out.

### 5. Verify the run landed in the metadata table

```sql
SELECT id, user_id, status, total_tokens, error_code
  FROM agent.runs
  ORDER BY started_at DESC
  LIMIT 5;
```

Expected: one row for the chat you just sent, with `status='completed'` (or `'failed'` if the smoke bot doesn't assert a particular LLM response). The schema is fresh; this is the first row.

### 6. Optional: verify resume semantics

This part is harder to test in a drill (you'd need a way to interrupt a run mid-execution). If you want to be thorough:

- Send a chat that triggers a multi-step plan.
- Kill the Python service mid-stream (Ctrl+C or `docker compose kill agent`).
- Restart the service.
- Re-send the same `run_id` (or echo it back via the API if the frontend exposes it).
- Verify the resumed run completes the remaining steps (check `agent.runs` ends up with `status='completed'`, not stuck in `running`).

If you skip this step, the smoke test (`tests/test_run_resume.py`) is the next-best signal — it exercises the same code path against a testcontainer.

## Roll-forward

The rollback is one-way. To re-enable run-state persistence after a rollback:

1. **Don't** restore from a `pg_dump` of just the schemas. The savepoint in langGraph's checkpoint is in the checkpoint blob; restoring a stale blob plus fresh DB state will produce inconsistent reads.
2. Instead, restart the service (the lifespan runs the migrations + `setup()` from scratch). The previous runs' state is lost; new runs use the new schemas.
3. If you need to preserve historical run metadata across a rollback, take a `COPY agent.runs TO ...` dump before the drop, and reload it into the new schema after the schemas are recreated. langGraph checkpoint data is not portable across this kind of reset; treat the rollback as a state-loss event for in-flight runs.

## Acceptance

- [ ] Pre-flight checklist complete.
- [ ] Schemas dropped (Step 1).
- [ ] Service restarted cleanly (Step 3 logs match).
- [ ] Single-turn chat works (Step 4).
- [ ] `agent.runs` has the new row (Step 5).
- [ ] (Optional) Resume semantics verified (Step 6).
- [ ] Drill outcome recorded in the change folder: edit `openspec/changes/agent-run-state/tasks.md`, check 13.3, append a one-line "drilled YYYY-MM-DD in $staging-env" note with the operator's initials.

## Related

- `openspec/changes/agent-run-state/proposal.md` — why this exists.
- `openspec/changes/agent-run-state/tasks.md` — task 13.3, the gate this runbook satisfies.
- `tests/test_smoke_run_state.py` — the test that pins the "before" state.
- `tests/test_run_resume.py` — the test that pins the resume contract (run as a follow-up signal if Step 6 is skipped).
