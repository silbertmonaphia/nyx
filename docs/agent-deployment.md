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
| `PYTHON_AGENT_GDPR_ENABLED` | no | `true` | Kill switch for `GET /agent/me/export` + `DELETE /agent/me` (503 when `false`). |
| `PYTHON_AGENT_AUDIT_RETENTION_DAYS` | no | `1095` | `agent.data_audit_log` window (3y); values below 365 refuse startup. |
| `PYTHON_AGENT_CHAT_RETENTION_DAYS` | no | `365` | Chat-history window (1y); values below 365 refuse startup. Set by the chat slice, listed here for the erasure story. |

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

## GDPR data handling: export + erasure

Companion to `openspec/changes/agent-gdpr-compliance/` (Slice 4 of `AgentMemoryRoadmap.md`). Two owner-scoped endpoints on the agent service:

- `GET /agent/me/export` — one JSON body carrying every row tied to the subject (covers a subject-access request).
- `DELETE /agent/me` — physical erasure of every row tied to the subject (covers a right-to-erasure request). Idempotent.

**Precondition for an erasure while the subject may be chatting:** run the erase when no run for the subject is in flight, then re-check `agent.chat_user_prefs` for the user after a few seconds. A turn streaming at erase time can write its assistant message into a just-deleted session (contained and warn-logged — the FK rejection surfaces as store-unavailable) and its late preference extraction is skipped by the in-SQL source-session guard, but the precondition removes the ambiguity entirely. If a run was in flight, re-run the erase (idempotent).

Both are scoped to the numeric user id in the `X-Nyx-User-Id` header — the same Go-verified header `/agent/sessions*` uses. A missing / non-integer header is a 400 (never an implicit user 0) and there is no admin override or cross-user parameter. This hop has no auth of its own: the agent service's network boundary is the trust boundary, so the operator invocations below must run from a host that can reach the agent port (host dev: `http://localhost:9000`; compose: from inside the compose network) and the header value must be the *Go* user id (the `users.id` behind the subject's JWT).

### Data inventory (what a subject's data touches)

| Table / store | Holds | Retention window | Covered by `DELETE /agent/me` |
| --- | --- | --- | --- |
| `agent.chat_sessions` | session rows, soft-deleted included | 365d (soft-delete + 7d grace, then hard-delete) | yes — every row, soft-deleted included |
| `agent.chat_messages` | every message turn (content, tool calls, tokens) | follows its session (FK ON DELETE CASCADE) | yes — deleted with its session |
| `agent.chat_user_prefs` | derived preference rows (`key` / `value`) | follows the source session; orphan-sweep backstop collects stragglers | yes — every row for the user |
| `agent.runs` | run metadata (status, tokens, timings) | 30d (terminal rows) | yes — every row, any status |
| `langgraph.checkpoint_*` | langGraph checkpoint state, keyed by thread id: the run id for single-turn runs, the **session id** for continuation turns (which carry no `agent.runs` row) | run-keyed threads GC with their `agent.runs` row; session-keyed threads GC when the chat-retention cron hard-deletes the session | yes — `adelete_thread` once per run id AND per deleted session id (the deduped union) |
| `agent.data_audit_log` | export/erase evidence — counts only, never content | 1095d (3y) | **No — deliberately retained.** The audit row is the evidence the erasure happened; it must outlive the deleted data (design D5) |
| **Langfuse** (opt-in, `PYTHON_AGENT_LANGFUSE_ENABLED=true`) | prompts + replies — i.e. message **content** — plus run/user ids | Langfuse instance retention (operator-configured; align with the run-retention parity goal, see the Langfuse rollout sibling) | **No — outside the erase path.** For a subject erasure with tracing enabled: delete the subject's traces in the Langfuse UI/API, or disable tracing. Not silent by design — this row is the operator checklist item |
| **Redis** budget counters (`agent:budget:u={user_id}:h=…`) | pseudonymous token counts keyed by user id | ~90 minutes (self-expiring) | not needed — expires on its own |
| **Jaeger** spans | run/step/tool metadata; `user_id` attribute; no message content | Jaeger storage retention (operator-configured) | not needed for content (ids only); expires with trace retention |

`threads` in the erase response counts only `adelete_thread` calls that completed without error; a per-thread failure is warn-logged, counted as not-deleted, and does not abort the remaining ids or the audit row — re-running the erase is safe (idempotent) and retries whatever is left.

### Operator playbook

Export a subject:

```bash
curl -fsS -X GET http://localhost:9000/agent/me/export \
  -H "X-Nyx-User-Id: 42" | jq .
# → {"user_id": 42, "exported_at": "2026-10-14T12:00:00+00:00",
#    "sessions": [{"id": "…", "title": "…", "started_at": "…", "deleted_at": null, …}],
#    "messages": [{"id": 123, "session_id": "…", "role": "user", "content": "…", "tool_calls": null, "tokens": 8, "created_at": "…"}],
#    "prefs": [{"key": "…", "value": …, "source_session_id": "…", "updated_at": "…"}],
#    "runs": [{"id": "…", "status": "completed", "total_tokens": 1234, …}],
#    "checkpoint_thread_ids": ["<run-id or session-id uuid>", …],
#    "audit_entries": [{"action": "export", "payload_summary": {…counts…}, "requested_at": "…"}]}
```

A subject with no data gets the same shape with empty arrays (200 — never a 404). Each call appends an `export` audit row with counts only.

Erase a subject:

```bash
curl -fsS -X DELETE http://localhost:9000/agent/me \
  -H "X-Nyx-User-Id: 42" | jq .
# → {"deleted": {"sessions": 3, "messages": 12, "prefs": 1, "runs": 2, "threads": 4, "thread_failures": 0}}
```

A second call returns all-zero counts with 200. Verify the post-state:

```sql
SELECT count(*) FROM agent.chat_sessions   WHERE user_id = 42;
SELECT count(*) FROM agent.chat_user_prefs WHERE user_id = 42;
SELECT count(*) FROM agent.runs            WHERE user_id = 42;
SELECT action, payload_summary, requested_at FROM agent.data_audit_log
  WHERE user_id = 42 ORDER BY requested_at;
-- Expect: zeros above; 'export' + 'erase' rows here (counts only, no content).
```

**If an erase returned 200 but its audit row is missing** (log line `gdpr erase: audit record failed after data was deleted` + the `agent_gdpr_audit_write_failed_total{endpoint="erase"}` counter ticked): the data IS gone — the evidence row was lost. Do not re-run the erase to "fix" it (a re-run appends an all-zero row and the true counts are unrecoverable); record the erasure manually from the ERROR log line, which carries the full counts and the user id.

**Backup / restore rule.** Backups may be restored for disaster recovery only — and a restored copy contains data for every subject erased since the backup was taken, so before the restored copy serves traffic again, re-apply every erasure recorded since that backup. The record of which erasures to re-apply is `agent.data_audit_log` (query `WHERE action = 'erase'`, take the `user_id`s with `requested_at` after the backup's timestamp), and the re-application is simply re-invoking `DELETE /agent/me` per recorded user id — it is idempotent, and the additional zero-count `erase` audit row it appends is correct evidence that the erasure was re-applied to the restored copy.

### Kill switch

`PYTHON_AGENT_GDPR_ENABLED=false` makes both endpoints return 503 with the standard error envelope (`{"error": "GDPR endpoints disabled", "details": …}`) and touch nothing: no store read, no store write, no audit row — "the forbidden operation never happened". The audit-retention iteration keeps running while the switch is off (the trail must expire on schedule regardless); only the two subject-facing endpoints are frozen. Re-enable by removing the variable / setting it `true` and restarting the agent service.

## Rollback

See `docs/runbooks/agent-run-state-rollback.md`. The runbook is the authoritative procedure; this doc links to it rather than duplicating.

## Common gotchas

- **`PYTHON_AGENT_DATABASE_URL` without `options=-c search_path=langgraph,agent`**: the service refuses to start with a clear error. Don't add `,public`; DDL goes to the first schema in the search path, DML to the first match, and we never touch `public`. (Was a doc drift in the original change — the proposal mentioned `,public`; the spec landed on the two-schema form.)
- **`psycopg` ImportError on a slim Python image**: the `agent` Docker image needs `libpq5` in its apt deps. Without it, `import psycopg` raises `no pq wrapper available`. Symptom: agent container exits 1 on first request that needs the saver. Fix is in `backend/agent_py/Dockerfile` (added in commit `4b75a96`).
- **`alembic upgrade head` fails partway**: `agent-migrate` exits non-zero, compose refuses to start `agent` (depends_on condition). The DB may be in a half-migrated state. Don't panic — `alembic` is transactional per migration; either the failing migration's transaction rolled back, or the failure is post-commit. Re-run `alembic current` to see where you are, then `alembic upgrade head` to retry. If a migration is genuinely broken, the recovery is to drop the schemas and let the release pipeline re-apply (the runbook's path).
- **Compose `depends_on: agent-migrate (condition: service_completed_successfully)` doesn't fire on first deploy**: the service has never run, so its "completed successfully" history is empty. The first deploy waits for `agent-migrate` to actually run, not for an old "completed" flag. Subsequent deploys benefit from the dependency history. (If you see compose hang on first deploy, check that `agent-migrate` is actually starting — the `condition` doesn't kick in until the service runs once.)
