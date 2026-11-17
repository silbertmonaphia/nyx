# Design

## Context

See `proposal.md` — Why. Relevant current state:

- Mutating feed tools dispatch through `internal/tool` → `feed.Service` → Postgres; the same registry backs the external MCP server and the agent loop.
- The executor retry loop mints `call_id = uuid4()` per attempt and retries timeouts/5xx/408/429; the approval gate pauses inside that loop and brokers decisions through Redis.
- A resumed run re-executes the in-flight graph node, so completed side effects replay.
- The MCP surface is also consumed by external agents; any new argument is a public contract.
- Postgres is the source of truth; Redis is not durable and must never hold the only copy of a write decision.

## Goals / Non-Goals

**Goals:**

- Exactly-once for mutating tools whose effect is a Postgres write, across transient retries and checkpoint resume.
- A deterministic key the agent can reproduce without storing it anywhere.
- Opt-in for external MCP clients; zero behavior change when the key is absent.
- Dedupe enforced where the write commits, in one transaction.

**Non-Goals:**

- Non-Postgres side effects (webhooks, email) — those need an outbox, deferred.
- Semantic dedupe of different arguments; only identical intents dedupe.
- Per-step checkpoints, leases, frame mirroring, supervisors (separate changes).

## Decisions

### Deterministic UUIDv5 over (run, turn, step, tool, canonical args)

Key = `uuid5(namespace, f"{run_id}:{turn_id}:{step_id}:{tool}:{canonical_args}")`. Canonicalisation reuses the executor's existing `_canonical_args` helper (sorted keys, deterministic JSON).

- Why: survives resume without storage — the run id, turn id, and step id are checkpointed, and plan `tool` step arguments come from the checkpoint. A random UUID minted once per intent survives retries but not resume; a DB-stored "current intent" row would add a second write per call.
- Why the turn id: a chat continuation reuses the session id as `run_id`, so without a per-turn discriminator an identical request in a later turn of the same session would collide with the earlier turn's record and silently skip the new write. `new_state` mints a fresh `turn_id` per turn; resume reads it back from the checkpoint.
- React-step caveat: arguments are model-generated; identical regenerated arguments dedupe, different arguments are a new intent (accepted limitation; mutating calls remain approval-gated).
- The key is derived **before** the approval gate; a crash after approval re-derives the same key from the same arguments.
- A model-supplied `idempotency_key` is overwritten by the derived value; it is never trusted.

### Optional tool argument, not protocol metadata

The key rides as an optional argument on the mutating tool's `inputSchema`.

- Why: typed handlers and external agents already speak arguments; MCP has no standard idempotency field, and `_meta` is invisible to typed tool handlers and non-portable.
- The key is added only to the dispatched argument map, so `tool_call` / `confirm_request` frames carry the model-level arguments without it and the confirmation card stays clean.
- The mutating tool descriptions MUST NOT mention the key (the planner catalog renders descriptions into the model prompt). External-client guidance lives only on the `idempotency_key` property description, which the agent strips with the property.
- Schema-driven injection: the agent adds the argument only when the served schema declares it. This makes rollout order irrelevant — a new agent against an old registry never sends an unknown field.

### Postgres table, same transaction as the mutation

```sql
CREATE TABLE tool_idempotency (
    owner_id   bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key        text        NOT NULL CHECK (char_length(key) BETWEEN 1 AND 128),
    tool       text        NOT NULL,
    response   jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, key)
);
CREATE INDEX tool_idempotency_created_at_idx ON tool_idempotency (created_at);
```

Index rationale:

- `PRIMARY KEY (owner_id, key)` is both the conflict target and the owner-isolation boundary. Its `owner_id` prefix also serves per-user erasure and owner-scoped cleanup, so no separate owner index is needed.
- `created_at` is the only non-PK query (retention sweep), so a single b-tree index covers it.
- No index on `tool`: nothing filters by it; it is compared on replay to reject a key reused across different tools.
- `ON DELETE CASCADE` matches the project's newer owner-scoped tables (`kb_sources`, `user_llm_settings`, refresh tokens). The stored response can contain feed content, so a user deletion must not leave it behind.
- This is the only new table in the change; existing tables are untouched. The item is raw SQL in the Go migration chain (not alembic) because only the Go write path reads it.

Flow inside one `pgx.Tx`:

1. `INSERT ... ON CONFLICT (owner_id, key) DO NOTHING` — a conflict blocks until the concurrent writer commits, then reports zero rows.
2. Inserted → run the mutation bound to the same tx (sqlc `WithTx`) → `UPDATE ... SET response` → commit.
3. Conflicted → `SELECT response` → return it without executing the write.

Alternatives rejected:

- Column on `feeds`: only covers create; update/delete cannot replay their response.
- Redis lock: Redis is not durable; a failover reopens the duplicate window, and the write itself is in Postgres.
- Idempotency store after the write: not atomic — a crash between write and record still duplicates.

### Service-layer transaction seam (option A)

The idempotent entry points live on the feed service, not in a tool-layer wrapper that calls the repository directly:

- Three thin methods — `CreateFeedIdempotent`, `UpdateFeedIdempotent`, `DeleteFeedIdempotent` — share one `idempotency.Store.Do(ctx, owner, key, tool, fn, afterCommit)`.
- `Do` owns the transaction; `fn` receives a tx-bound repository via `feed.NewRepositoryFromQuerier(q.WithTx(tx))`. The repository already supports this mode (and `GetAll` already uses `BeginTx` + `WithTx`), so no feed SQL statement changes — `InsertFeed`, `UpdateFeed`, and `SoftDeleteFeed` stay byte-identical.
- The service's post-write side effects — embedding index and cache invalidation — run through `afterCommit`, only after a newly executed mutation commits. A replayed duplicate returns the stored response and skips both: re-indexing would spend a provider call, and the original commit already invalidated the cache.
- The existing `feed.Service` interface is not widened (that would ripple through HTTP handlers and every test stub). The tool layer type-asserts a narrow optional interface; without a key it calls the existing methods unchanged.

Rejected variants: running the hooks inside the transaction (an external embedding call holds a transaction open, and a rollback leaves a dangling embedding); changing the existing `Service` methods to take a key (interface churn across handlers and stubs); a tool-layer wrapper bypassing the service (loses indexing and cache invalidation).

### Served and documented, but hidden from the model

- `tools/list` keeps the optional `idempotency_key` property and documents its semantics for external clients.
- The agent strips the property from the model-bound function schema and from the retry re-prompt schema text; the dispatcher consults the raw served schema to decide injection.
- Why: model output is non-deterministic. A model-chosen key either changes across retries (dedupe silently fails) or collides across different intents (a real write is swallowed). The key is runtime-derived, always.

### Validation and scoping

`idempotency_key` is optional, max 128 chars, charset `[A-Za-z0-9:_-]`, validated with the existing struct-tag validation before the tool body. Scoping is `(owner_id, key)` from the request context — never from arguments — so a key collision across users is impossible and cannot leak a response.

### Retention

A Go-side periodic cleanup deletes rows older than `TOOL_IDEMPOTENCY_RETENTION` (default 7 days). Retention is orders of magnitude larger than any retry/resume window; purging only ends the dedupe window and never touches feed data. The configured window must be at least one hour, which also keeps an hourly sweep from racing the conflict/replay read.

## Risks / Trade-offs

- **React-step duplicate intents (different args)** → accepted; approval gate still applies. Documented in the agent README.
- **Unique-index contention on hot keys** → keys are per-intent, so contention only occurs on genuine duplicates; blocking is bounded by the mutation transaction.
- **Table growth** → retention + `created_at` index.
- **Key visible in UI frames** → opaque, non-secret; optional card filter later.
- **External clients omit the key** → no dedupe, exactly today's behavior.
- **Manual purge inside the retry window** → possible only via operator action; retention default prevents it.
- **Replay skips embedding indexing and cache invalidation** → acceptable: the original commit already ran them, and indexing is best-effort (a failed index is dropped, as today). A replayed duplicate is not a new write and must not spend a provider call.
- **JSONB response bloat** → bounded by retention plus the owner cascade; responses are the tool's own small result objects.
- **External client reuses a key across different tools** → rejected with a typed validation error on replay instead of silently returning the other tool's response.

## Migration Plan

- `backend/migrations/` adds the single `tool_idempotency` table, its `created_at` index, and the `users(id)` foreign key with `ON DELETE CASCADE`; golang-migrate applies it on boot before serving.
- No backfill, no existing-table changes. Rollback: revert the code; the table is inert and can be dropped in a later cleanup migration.
- Deploy order is irrelevant: schema-driven injection means new agent + old Go sends no key; old agent + new Go dedupes nothing.

## Open Questions

- The retention default may need tuning once write volume is known (config-only).
