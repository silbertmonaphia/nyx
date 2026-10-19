# PostgreSQL DML Review — 2026-10-18

## Scope & method

Reviewed every SQL statement and transaction that reads or writes row data:

- **Go backend** — `backend/queries/*.sql` plus the repository / service call sites that execute them, the refresh-token rotation and cleanup paths, the RAG indexer/retriever, and the migrations whose DDL is applied on top of existing rows (`000015`).
- **Python agent** — `agent/chat_store.py`, `prefs.py`, `run_metadata.py`, `audit.py`, `retention.py`, `api/chat_sessions.py`, `api/gdpr.py`, the Alembic chain where it matters for data safety, and the checkpointer/pool wiring.
- **Cross-runtime** — schema/version-table ownership between golang-migrate and Alembic, `search_path`, and the shared `AsyncPostgresSaver` connection.

Method: code trace against the actual SQL, plus live reproduction on a throwaway `pgvector/pgvector:pg17` container (PostgreSQL 17.11, pgvector 0.8.6). The container was stopped afterwards; no repository files were modified.

Verification commands run:

- `go build ./...` — pass
- `SKIP_CONTAINERS=true go test ./...` — pass
- `golangci-lint run --timeout=5m` — pass
- `sqlc diff` — pass (no generated-code drift)
- `make migration-check` — pass
- Python agent suite without a DB DSN — 427 passed, 151 skipped. The skipped set includes every DB-backed suite (chat store, prefs, GDPR stores, retention, run metadata, session API, graph persistence), so the Python concurrency findings below are static analysis against READ COMMITTED semantics, not live reproductions.

Companion document: `reviews/postgresql_ddl_2026-10-18.md` already covers index/type/constraint design. This review focuses on query semantics, transaction boundaries, races, and migration data safety; overlapping index findings are cross-referenced rather than repeated in full.

**Overall verdict: request changes.** Two high-severity defects (a migration that bricks startup on non-empty data, and a refresh-token rotation hole that defeats reuse detection), plus six medium-severity correctness / compliance issues.

---

## High

### H1. `000015` fails on any non-empty `feed_embeddings` table; the next deploy boot-loops

`backend/migrations/000015_resize_feed_embedding.up.sql:26-29` (down: `.down.sql:6-9`)

```sql
ALTER TABLE feed_embeddings DROP COLUMN embedding;
ALTER TABLE feed_embeddings
    ADD COLUMN embedding vector(1024) NOT NULL;
```

`DROP COLUMN` removes the column but leaves every row in place. `ADD COLUMN ... NOT NULL` without a default then fails immediately on a non-empty table:

```
ERROR: column "embedding" of relation "feed_embeddings" contains null values
```

Reproduced live on PostgreSQL 17.11. The comment at the top of the migration claims the operation “wipes every row” — it does not. `backend/cmd/api/main.go:120` calls `log.Fatal` on migration failure, so an existing deployment that ever used RAG fails to boot after upgrading. The down migration has the identical shape for `vector(1536)`.

CI cannot catch this: testcontainers and the integration suite always start from an empty database, so the column is added to zero rows and the migration succeeds.

Fix: delete the derived rows before the column swap, in both directions:

```sql
DELETE FROM feed_embeddings;
DROP INDEX IF EXISTS feed_embeddings_embedding_hnsw_idx;
ALTER TABLE feed_embeddings DROP COLUMN embedding;
ALTER TABLE feed_embeddings ADD COLUMN embedding vector(1024) NOT NULL;
CREATE INDEX feed_embeddings_embedding_hnsw_idx
    ON feed_embeddings USING hnsw (embedding vector_cosine_ops);
```

Embeddings are rebuildable via the RAG backfill. Also add a CI job that applies the migration chain over a database seeded at the previous release, not only over an empty one.

### H2. Refresh-token rotation is not atomic; concurrent replay yields multiple live tokens

`backend/queries/users.sql:47-68`, `backend/internal/user/service.go:286-348`

`RotateRefreshToken` inserts the replacement **unconditionally** and the driving `UPDATE` only matches on id:

```sql
WITH new_row AS (
    INSERT INTO refresh_tokens (...)
    VALUES (...) RETURNING *
)
UPDATE refresh_tokens
SET replaced_by_id = new_row.id,
    revoked_at     = COALESCE(refresh_tokens.revoked_at, now())
FROM new_row
WHERE refresh_tokens.id = @old_id
RETURNING new_row.*;
```

There is no `AND revoked_at IS NULL`. `Service.Refresh` reads the old row (`GetRefreshTokenByHash`), checks `row.RevokedAt == nil`, then rotates — a classic TOCTOU. Two concurrent refresh requests with the same token both observe `NULL`, both insert a new row, and both succeed. Reproduced live by executing the CTE twice against the same old row: the result is two new rows with `revoked_at IS NULL`, i.e. two valid sessions from one token, with the reuse-detection path never firing. The old row's `replaced_by_id` points at the second replacement, so the rotation chain is also broken.

The existing tests do not model this: the service tests are sequential and mock the repository; the integration test only rotates a row once.

Fix: make the old-row update the driver and let the insert depend on it:

```sql
WITH old_row AS (
    UPDATE refresh_tokens
    SET revoked_at = now()
    WHERE id = @old_id AND revoked_at IS NULL
    RETURNING *
), new_row AS (
    INSERT INTO refresh_tokens (user_id, token_hash, family_id, replaced_by_id, expires_at)
    SELECT @user_id, @token_hash, @family_id, old_row.id, @expires_at
    FROM old_row
    RETURNING *
)
UPDATE refresh_tokens
SET replaced_by_id = new_row.id
FROM new_row
WHERE refresh_tokens.id = @old_id
RETURNING new_row.*;
```

A second concurrent rotation blocks on the row lock, re-evaluates `revoked_at IS NULL`, matches zero rows, inserts nothing, and returns `pgx.ErrNoRows`; the service must map that back to the reuse branch (revoke family + `ErrRefreshTokenReuse`). Add a real-database two-goroutine test asserting exactly one new active row.

Residual race (lower severity): `RevokeRefreshTokenFamily` cannot see a token inserted concurrently by a rotation, so logout racing refresh can still leave one live token. Closing that requires locking the family (or a family-level revocation timestamp).

---

## Medium — Go backend

### M1. Filtered HNSW scan under-fills RAG results

`backend/queries/rag.sql:25-37`, `backend/migrations/000014_add_feed_embeddings.up.sql:38-43`

`RetrieveFeedPassages` filters on `fe.user_id`, `f.user_id` and `f.deleted_at` after ordering by cosine distance. With the default `hnsw.iterative_scan=off`, pgvector collects `ef_search` candidates from the **global** index before the SQL filter runs, so other users' rows can exhaust the candidate set.

Reproduced live: one row for the target user and 2,000 rows for another user, `ORDER BY embedding <=> $1 LIMIT 5` returned **zero** rows for the target user. `hnsw.iterative_scan=relaxed_order` and `strict_order` (with `max_scan_tuples=100000`) did not recover the row in this reproduction.

User-visible effect: RAG silently retrieves fewer passages than exist, and answers lose context precisely for users whose corpus is small relative to the global index.

Fix options, in order of confidence:

1. Materialize the user's rows before ordering:
   ```sql
   WITH user_embeddings AS MATERIALIZED (
       SELECT fe.feed_id, fe.embedding, f.title, f.description
       FROM feed_embeddings fe
       JOIN feeds f ON f.id = fe.feed_id
       WHERE fe.user_id = $2 AND f.user_id = $2 AND f.deleted_at IS NULL
   )
   SELECT ..., (1 - (embedding <=> $1))::float8 AS score
   FROM user_embeddings
   ORDER BY embedding <=> $1
   LIMIT $3;
   ```
   Per-user corpora are small, so exact KNN over the `user_id` btree is cheap.
2. Verify `hnsw.iterative_scan = relaxed_order` on the actual query plan before relying on it.
3. Partition the embedding table by user (heavier).

Correct the migration comment that calls the post-filter overhead negligible.

### M2. Unbounded `page` overflows `int32`; negative `OFFSET` returns 500

`backend/internal/feed/huma_handler.go:190`, `backend/internal/feed/repository.go:34,142`, `backend/internal/tool/feeds.go:127-138`

`Page` has `minimum:"1"` but no maximum; only `pageSize` is clamped. `offset := (page - 1) * pageSize` is computed in `int` and then narrowed by `toInt32`. `GET /api/feeds?page=200000000&page_size=20` produces 3,999,999,980, which wraps to a negative `int32`; PostgreSQL rejects it with `ERROR: OFFSET must not be negative`, surfaced as a 500. The MCP tool path applies the same normalization and is equally reachable. The code comment claiming handler-side caps keep the value within `int32` is false.

Fix: cap `page` (e.g. 1,000,000) at both entry points and/or floor the offset at zero before conversion; return 400 for out-of-range input. If a Huma tag changes, regenerate `api/openapi.json`.

### M3. Feed-list index does not match the list predicate

`backend/migrations/000006_add_movies_indexes.up.sql`

Already raised in the DDL review: there is no `(user_id, created_at DESC, id DESC) WHERE deleted_at IS NULL`; `idx_movies_active ON (id) WHERE deleted_at IS NULL` is redundant with the PK; index names are stale after the `movies → feeds` rename. Current data volumes make this a sort, not a correctness bug.

### M4. Refresh-token purge can evict recently revoked rows; unindexed columns

`backend/queries/users.sql:111-120`

```sql
DELETE FROM refresh_tokens
WHERE created_at < @cutoff
  AND (revoked_at IS NOT NULL OR expires_at < now());
```

Retention is anchored on `created_at`, so a row created ten days ago and revoked one minute ago is deleted on the next hourly sweep — contradicting the comment that revoked rows stay available for incident investigation. There is no index on `created_at`, and the self-referencing FK (`replaced_by_id ON DELETE SET NULL`) has no index either, so each purge scans.

Fix: anchor retention on `COALESCE(revoked_at, expires_at)` and add `(created_at)` plus `(replaced_by_id)` indexes (the DDL review already lists both indexes).

### M5. `GetAll` count/page consistency claim is false

`backend/internal/feed/repository.go:126-130,135-215`

`BeginTx(ctx, pgx.TxOptions{})` uses READ COMMITTED; each statement takes a new snapshot, so a concurrent insert can make `total` disagree with the returned page (and with `has_more`). Either use `pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}` for this read or correct the comment.

### M6. pg_trgm “graceful skip” does not skip anything (reproduced live)

`backend/migrations/000006_add_movies_indexes.up.sql:16-22` and the same pattern in `000007_add_movies_description_trgm_index.up.sql`

The `DO` block catches `insufficient_privilege` from `CREATE EXTENSION pg_trgm` and raises a NOTICE, but the following `CREATE INDEX ... USING GIN (title gin_trgm_ops)` runs unconditionally. Reproduced as a non-superuser: the NOTICE fires, then the migration aborts with `operator class "gin_trgm_ops" does not exist`. Move the index creation inside the guarded block (or into a second block guarded by a `pg_extension` check), or fail loudly with an actionable message.

### M7. Other low-severity Go findings

- **`CREATE EXTENSION vector` is unguarded** (`000014_add_feed_embeddings.up.sql:27`). A managed Postgres without pgvector or CREATE privilege fails all migrations at boot even when RAG is disabled. Document the hard prerequisite or wrap with an explicit actionable error.
- **Down migrations do not reverse their up**: `000002` deletes seeded rows by title (can delete user-created rows with the same title); `000009`/`000010` rollback fails if any NULL email exists; `000012` leaves the placeholder user; `000016` is a no-op.
- **`make sqlc-diff` misses the RAG package** (`backend/Makefile:43-46` only diffs `internal/feed/db internal/user/db`; CI runs the same target). Use `sqlc diff`, or add `internal/rag/db`. Also update `sqlc.yaml`: the RAG schema list stops at `000014`, so generated code is validated against `vector(1536)` while runtime is `vector(1024)`.
- **`BackfillOnce` is a per-process, per-user `sync.Once`** (`backend/internal/rag/service.go:207-220`). A failed backfill, or a user with more than `maxBackfill × maxBackfillIterations` feeds, is not retried until the process restarts. The inline comment saying a future chat request retries is false. This matters more once H1’s fix empties the embedding table.
- Cross-type IDs (`users.id INTEGER` vs `feeds.user_id BIGINT`) are legal but inconsistent; covered in the DDL review.

---

## Python agent

### P1. HIGH (compliance) — GDPR erase races in-flight checkpoint writes

`backend/agent_py/agent/api/gdpr.py:223-279`

The erase path deletes `agent.runs` and `agent.chat_sessions`, then calls `checkpointer.adelete_thread` for the union of run ids and session ids. An SSE run already in flight for the same user is not cancelled or fenced. Every subsequent graph step writes `langgraph.checkpoint_*` rows for the deleted thread again.

Those rows contain conversation state, but the `agent.runs` / `chat_sessions` rows that named the thread are gone. Neither the export path nor a later retention / erase pass can discover the id, and retention only GCs ids returned by the deletes it just performed. The erased content therefore persists indefinitely.

Fix: pick one —

- track active run ids per user and cancel / defer / re-GC on erase;
- have the erase path record a per-user tombstone that the run lifecycle honors;
- add an orphan-thread sweep to the retention cron: GC `langgraph` threads whose id has neither an `agent.runs` row nor a `chat_sessions` row. This also covers threads stranded by a failed checkpoint GC.

Add a test where erase runs while a stream is mid-step. (Static analysis; live reproduction was not possible because the Python DB suite requires `TEST_ASYNC_PG_DSN`.)

### P2. MEDIUM — Preference upsert can resurrect an erased row

`backend/agent_py/agent/prefs.py:175-193`

```sql
INSERT INTO chat_user_prefs (user_id, key, value, source_session_id)
SELECT $1, $2, $3::jsonb, $4::uuid
WHERE $4::uuid IS NULL OR EXISTS (
    SELECT 1 FROM chat_sessions s WHERE s.id = $4::uuid
)
ON CONFLICT (user_id, key) DO UPDATE SET ...
```

The docstring claims the guard “cannot be raced between a check and the write”; it can. The `EXISTS` is a plain READ COMMITTED snapshot with no lock, and it does not check `s.user_id = $1` or `s.deleted_at IS NULL`.

Race: the detached preference-extraction task (fired after `[DONE]`, `main.py:1279-1289`) takes its snapshot while `delete_all_for_user` is uncommitted, sees the old session row, inserts; the erase’s `DELETE FROM chat_user_prefs WHERE user_id = $1` has already scanned or is ordered before the new row, and both commit. The `ON CONFLICT DO UPDATE` variant can wait on the erased row and then insert once the conflict is gone. `delete_orphaned` collects the row on the next retention tick **only if chat retention is enabled**; otherwise it persists.

Fix: add `AND s.user_id = $1 AND s.deleted_at IS NULL` to the `EXISTS`, and serialize with the erase (`FOR KEY SHARE` on the session row, or a per-user advisory lock held by both erase and upsert). Add a two-connection race test.

### P3. MEDIUM — `record_message` can kill the stream on unsanitized text

`backend/agent_py/agent/chat_store.py:431-451,468-479`

The retry-on-`DataError` path only re-executes with `steps = NULL`; when `steps_json is None` it re-raises. `content` and `tool_calls` are not sanitized, and every call site catches only `ChatStoreUnavailable` (`main.py:900-913` before the first `run_id` frame; `main.py:1222-1242` in the shielded finalize task).

A JSON request containing `\u0000` (valid JSON) produces a Python string with NUL; asyncpg raises `CharacterNotInRepertoireError` (verified subclass of `DataError`). Lone surrogates similarly raise `UnicodeEncodeError`. Result: the user-message write fails before streaming starts, or the finalize task raises after the stream, instead of the documented best-effort behavior.

Fix: strip NUL / encode with `errors="replace"` at the trust boundary, and translate any residual `DataError` / `UnicodeEncodeError` to `ChatStoreUnavailable` after the steps-only retry. Test `\x00` in content, steps, and tool_calls.

### P4. LOW — `list_messages` does not provide the claimed 404 guarantee

`backend/agent_py/agent/chat_store.py:335-341,349-368`

The docstring says the transaction keeps a concurrent purge from turning a would-be 404 into `[]`. Under READ COMMITTED each statement takes a new snapshot and the ownership probe takes no lock, so a concurrent GDPR erase can commit between the probe and the message read, yielding `200 {"messages": []}` instead of 404. Use `SELECT 1 ... FOR SHARE` on the probe or REPEATABLE READ.

### P5. LOW — `record_message` INSERT is not user-scoped

`backend/agent_py/agent/chat_store.py:432-451`

The INSERT relies solely on the FK; the `user_id` guard lives on the follow-up `UPDATE`, whose affected-row count is ignored and whose failure does not roll back the insert. No current request path is exploitable — the resume decision (`main.py:837-859`) is user-scoped and `POST /agent/sessions` mints server-side UUIDs — but the invariant is broken for any future caller. Worse, `tests/test_chat_store.py:122-132` pins the foreign-user insert as succeeding.

Fix: `INSERT INTO chat_messages (...) SELECT ... FROM chat_sessions WHERE id=$1 AND user_id=$N AND deleted_at IS NULL RETURNING id`, map zero rows to `ChatStoreUnavailable`, and flip the test to assert rejection.

### P6. LOW — Saver teardown is a silent no-op

`backend/agent_py/agent/main.py:481-485,536-539`

Installed `AsyncPostgresSaver` has no `__aenter__` / `__aexit__` (verified against the installed langgraph package); `from_conn_string` is an `@asynccontextmanager`. The lifespan calls `await checkpointer.__aexit__(...)`, catches the resulting `AttributeError` with a bare `except`, and never exits the stashed `saver._cm` — the psycopg connection is not closed on shutdown. Fix: `await checkpointer._cm.__aexit__(None, None, None)` and do not swallow unexpected errors.

### P7. LOW — Pre-stream `get_run` is unguarded

`backend/agent_py/agent/main.py:838`

`get_run` can raise `RunMetadataUnavailable` after retries; every neighboring call on the resume path (`get_session`, `create_run`) is best-effort. A transient pool blip during a resume attempt returns 500 before the `run_id` frame instead of falling back to a fresh run. Catch and treat as fresh (warn-log).

### P8. LOW — Alembic logs the full DSN, including the password

`backend/agent_py/migrations/env.py:86`

`logger.info("alembic: DSN %s", url)` emits the credential if any INFO-level logging configuration (or `fileConfig`) is added. Today the app sets root to WARNING and the CLI has no logging config, so it is latent, not active. Drop the line or render with `hide_password=True`.

### P9. LOW — Retention predicates have no supporting indexes

- `backend/agent_py/agent/run_metadata.py:268-274` — `ended_at < cutoff AND status IN ('completed','failed')`; only `(user_id, started_at DESC)` exists → sequential scan per tick.
- `backend/agent_py/agent/chat_store.py:640-647` — `last_active_at < cutoff AND deleted_at IS NULL`; the existing partial index leads with `user_id` → full scan.
- `backend/agent_py/agent/chat_store.py:666-673` — `deleted_at IS NOT NULL AND deleted_at < cutoff`; the partial index excludes exactly those rows → full scan.
- `backend/agent_py/agent/prefs.py:319-330` — anti-join over `chat_user_prefs`; `source_session_id` has no index → full scan.

Both retention phases are also single unbounded `UPDATE` / `DELETE` statements. Add the partial indexes listed in the DDL review; if these tables can grow large, batch with `LIMIT` / `ctid` loops.

### P10. MEDIUM (tests/CI) — the agent test suite does not run in CI

`.github/workflows/ci.yml:89-154` runs only `alembic upgrade head` twice against a fresh database. There is no `pytest` job for `backend/agent_py`; without `TEST_ASYNC_PG_DSN` every DB-backed suite self-skips (427 passed / 151 skipped locally). The schema check proves the chain runs, not that any query or transaction behaves. Add a Postgres-service pytest job, including at least one two-connection test for the erase / rotation races above.

### P11. Other low-severity Python findings

- `_normalize_database_url` (`backend/agent_py/agent/config.py:319-369`) accepts `postgresql+asyncpg://` (the scheme CI uses for Alembic); asyncpg rejects it at app boot. Normalize the scheme or raise a clear configuration error.
- `create_run` mints the UUID inside the retried function (`run_metadata.py:139-149`). A `PostgresConnectionError` after the server committed leaves an orphaned `running` row on the retry (new id). Mint outside the retry loop; the same duplicate-insert class applies to `record_message` and `audit.record` on post-commit connection loss.
- `_with_retry` catches any `TimeoutError`, including one raised by the operation itself, and misclassifies it as pool exhaustion. Prefer `asyncio.TimeoutError` scoped around `pool.acquire`.

---

## Cross-runtime

- **Schema isolation is clean.** Go owns `public.*` (`schema_migrations` via golang-migrate); Python owns `agent.*` (`alembic_version` via `version_table_schema="agent"`) and `langgraph.*`. The Python `search_path` is `langgraph,agent` and excludes `public`, so an unqualified Go table name fails closed. No table-name or version-table collision.
- **`options=-c search_path=langgraph,agent` in the DSN works for asyncpg**: unknown query parameters become `server_settings`, and PostgreSQL’s startup packet recognizes `options`; Alembic re-SETs its own path in `env.py`. The psycopg-based saver strips the option and sets the path explicitly, correctly before `setup()`.
- **Sharing one `AsyncPostgresSaver` connection across concurrent runs is safe**: the installed langgraph `_cursor()` wraps every operation in `self.lock` (`asyncio.Lock`), so concurrent runs serialize rather than interleave transactions. The subagent-flagged risk does not reproduce from the installed source.
- No Python code references a `public.*` table.

---

## Confirmed clean

- No string-built SQL. Go uses sqlc parameterization throughout; Python builds SQL only from constant column lists plus bound parameters (`_RUN_COLUMNS`, the two `list_sessions` fragments), and the only DDL identifier interpolation is a constant schema name.
- Owner scoping in `feeds` queries is complete; every read/write filters `user_id`, and cross-owner access collapses to `ErrNotFound`. MCP/tool paths route through `feed.Service` with `reqctx` scoping; no direct SQL.
- sqlc annotations and generated output are in sync (`sqlc diff` exit 0); `RETURNING *` column order matches the generated structs; nullable columns use `pgtype` correctly.
- The refresh-token self-stamp (`InsertRefreshToken` + `StampRefreshTokenFamily`) is correctly wrapped in a transaction.
- `000016`’s `setval(pg_get_serial_sequence('users','id'), COALESCE(MAX(id),1), MAX(id) IS NOT NULL)` handles both the empty and non-empty cases correctly.
- pgxpool configuration, `pgxvec.RegisterTypes` in `AfterConnect`, `ErrNoRows` mapping, and `ErrNoChange` handling in the migration runner are sound.
- The cache-aside invariant holds: every cache call swallows with `log.Warn`, and the cache key includes `user_id` (`feeds:u=%d:q=%s:p=%d:s=%d:o=%s`). Note that `CLAUDE.md` still documents the old user-less key format.
- Alembic chain: single head `0008_chat_message_steps`, linear `down_revision` links, every table/index explicitly in the `agent` schema, `timestamptz` / `jsonb` / `uuid` / identity / `ON DELETE CASCADE` used correctly.
- `asyncpg.create_pool` is awaited before use (`build_pool`), matching the asyncpg 0.32+ contract.

---

## Prioritized recommendations

### P0 — release blockers

1. Fix `000015` up/down: `DELETE FROM feed_embeddings;` before the column swap; add a seeded-upgrade CI test.
2. Make `RotateRefreshToken` atomic (`revoked_at IS NULL` as the driving predicate); map zero rows to the reuse branch; add a concurrent-replay test.

### P1 — security / compliance / data integrity

3. Fence GDPR erase against in-flight runs, or add an orphan-thread sweep to retention (Python P1).
4. Add `user_id` + `deleted_at` to the prefs `EXISTS` guard and serialize against erase (Python P2).
5. Scope the `chat_messages` INSERT by `user_id`; fix the test that pins the cross-user insert (Python P5).
6. Sanitize NUL / lone surrogates in `record_message` and map residual `DataError` to `ChatStoreUnavailable` (Python P3).
7. Cap `page` / guard the offset conversion in the HTTP and MCP entry points (Go M2).

### P2 — correctness under concurrency and scale

8. Replace the filtered-HNSW retrieval with a per-user materialized exact scan (or verify iterative scan on a real plan) (Go M1).
9. Use REPEATABLE READ (or `FOR SHARE`) for the count/page and ownership-probe transactions (Go M5, Python P4).
10. Key refresh-token retention on `COALESCE(revoked_at, expires_at)` and add the missing indexes (Go M4).
11. Add the retention indexes and consider batching the sweeps (Python P9).
12. Run the agent pytest suite (with `TEST_ASYNC_PG_DSN`) in CI, including concurrency tests (Python P10).

### P3 — hardening / cleanup

13. Fix the pg_trgm guard, guard/`document` the vector extension, and repair the down-migration asymmetries (Go M6/M7).
14. Extend `make sqlc-diff` to `internal/rag/db`, update `sqlc.yaml` to the latest migration, and stop `BackfillOnce` from being a once-per-process no-retry gate (Go M7).
15. Fix saver teardown, pre-stream `get_run` handling, DSN logging, and non-idempotent insert retries (Python P6/P7/P8/P11).

---

## Reproduction appendix

All four Go-side reproductions ran on `pgvector/pgvector:pg17` (PostgreSQL 17.11, pgvector 0.8.6).

**1. `000015` on a non-empty table**

```sql
CREATE TABLE feed_embeddings (
    feed_id integer PRIMARY KEY,
    user_id bigint NOT NULL,
    embedding vector(1536) NOT NULL,
    chunk_text text NOT NULL,
    content_hash bytea NOT NULL
);
INSERT INTO feed_embeddings VALUES (1, 1, array_fill(0.1, ARRAY[1536])::vector, 'x', '\x00');
ALTER TABLE feed_embeddings DROP COLUMN embedding;
ALTER TABLE feed_embeddings ADD COLUMN embedding vector(1024) NOT NULL;
-- ERROR: column "embedding" of relation "feed_embeddings" contains null values
```

**2. Refresh rotation replay**

```sql
-- old row id = 1, family_id = 1
WITH new_row AS (
    INSERT INTO refresh_tokens (user_id, token_hash, family_id, replaced_by_id, expires_at)
    VALUES (1, '\x02', 1, 1, now() + interval '7 days') RETURNING *
)
UPDATE refresh_tokens
SET replaced_by_id = new_row.id, revoked_at = COALESCE(revoked_at, now())
FROM new_row WHERE refresh_tokens.id = 1 RETURNING new_row.id;
-- succeeded

-- run the same statement again with '\x03'
-- succeeded again → two rows with revoked_at IS NULL
```

**3. Filtered HNSW under-return**

```sql
-- 2,000 rows for user 2, one row for user 1; HNSW index on embedding
SET enable_seqscan = off;
SET hnsw.ef_search = 40;
SELECT count(*) FROM (
    SELECT id FROM ragtest
    WHERE user_id = 1
    ORDER BY embedding <=> (SELECT array_agg(random())::vector FROM generate_series(1, 1024))
    LIMIT 5
) q;
-- 0
```

**4. Negative OFFSET**

```sql
SELECT 1 OFFSET -1;
-- ERROR: OFFSET must not be negative
```

The page overflow that produces this value:

```text
page = 21474837, page_size = 100
offset = (page - 1) * page_size = 2,147,483,600
int32(offset)                       = -2,147,483,696
```
