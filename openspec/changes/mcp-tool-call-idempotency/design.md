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

### Deterministic UUIDv5 over (run, step, tool, canonical args)

Key = `uuid5(namespace, f"{run_id}:{step_id}:{tool}:{canonical_args}")`. Canonicalisation reuses the executor's existing `_canonical_args` helper (sorted keys, compact separators).

- Why: survives resume without storage — the run id and step id are checkpointed, and plan `tool` step arguments come from the checkpoint. A random UUID minted once per intent survives retries but not resume; a DB-stored "current intent" row would add a second write per call.
- React-step caveat: arguments are model-generated; identical regenerated arguments dedupe, different arguments are a new intent (accepted limitation; mutating calls remain approval-gated).
- The key is derived **before** the approval gate; a crash after approval re-derives the same key from the same arguments.
- A model-supplied `idempotency_key` is overwritten by the derived value; it is never trusted.

### Optional tool argument, not protocol metadata

The key rides as an optional argument on the mutating tool's `inputSchema`.

- Why: typed handlers and external agents already speak arguments; MCP has no standard idempotency field, and `_meta` is invisible to typed tool handlers and non-portable.
- Trade-off: the opaque (non-secret) key appears in `tool_call` / `confirm_request` frames and therefore the confirmation card. Filtering it in the card is UI polish, not a contract change.
- Schema-driven injection: the agent adds the argument only when the served schema declares it. This makes rollout order irrelevant — a new agent against an old registry never sends an unknown field.

### Postgres table, same transaction as the mutation

```sql
CREATE TABLE tool_idempotency (
    owner_id   bigint      NOT NULL,
    key        text        NOT NULL,
    tool       text        NOT NULL,
    response   jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, key)
);
CREATE INDEX tool_idempotency_created_at_idx ON tool_idempotency (created_at);
```

Flow inside one `pgx.Tx`:

1. `INSERT ... ON CONFLICT (owner_id, key) DO NOTHING` — a conflict blocks until the concurrent writer commits, then reports zero rows.
2. Inserted → run the mutation bound to the same tx (sqlc `WithTx`) → `UPDATE ... SET response` → commit.
3. Conflicted → `SELECT response` → return it without executing the write.

Alternatives rejected:

- Column on `feeds`: only covers create; update/delete cannot replay their response.
- Redis lock: Redis is not durable; a failover reopens the duplicate window, and the write itself is in Postgres.
- Idempotency store after the write: not atomic — a crash between write and record still duplicates.

### Validation and scoping

`idempotency_key` is optional, max 128 chars, charset `[A-Za-z0-9:_-]`, validated with the existing struct-tag validation before the tool body. Scoping is `(owner_id, key)` from the request context — never from arguments — so a key collision across users is impossible and cannot leak a response.

### Retention

A Go-side periodic cleanup deletes rows older than `TOOL_IDEMPOTENCY_RETENTION` (default 7 days). Retention is orders of magnitude larger than any retry/resume window; purging only ends the dedupe window and never touches feed data.

## Risks / Trade-offs

- **React-step duplicate intents (different args)** → accepted; approval gate still applies. Documented in the agent README.
- **Unique-index contention on hot keys** → keys are per-intent, so contention only occurs on genuine duplicates; blocking is bounded by the mutation transaction.
- **Table growth** → retention + `created_at` index.
- **Key visible in UI frames** → opaque, non-secret; optional card filter later.
- **External clients omit the key** → no dedupe, exactly today's behavior.
- **Manual purge inside the retry window** → possible only via operator action; retention default prevents it.

## Migration Plan

- `backend/migrations/` adds the table and index; golang-migrate applies it on boot before serving.
- No backfill. Rollback: revert the code; the table is inert and can be dropped in a later cleanup migration.
- Deploy order is irrelevant: schema-driven injection means new agent + old Go sends no key; old agent + new Go dedupes nothing.

## Open Questions

- Whether the confirmation card should hide the `idempotency_key` argument (cosmetic; can land later without spec changes).
- The retention default may need tuning once write volume is known (config-only).
