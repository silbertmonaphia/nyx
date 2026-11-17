# Design

## Context

See `proposal.md` — Why. The current KB has two source families: a boot-provisioned `scope='shared'` directory (`KB_ROOT`, imported by `cmd/kb index --root`, embedded with the deployment identity) and a per-user `scope='user'` inline source (embedded with the user's BYOK identity). Scope/owner are denormalised onto `kb_sources`, `kb_documents`, and `kb_chunks`; every visibility query branches on `scope='shared' OR (scope='user' AND owner_id=…)`, and the shared branch is what lets deployment-owned content reach every user.

Constraints that shape the approach:

- The backend applies migrations at boot; the deploy is not a long-lived multi-version fleet, so a single destructive contract migration is acceptable.
- The Python agent never reads KB config; it calls the Go RAG bridge with the caller's Bearer token, so owner-only SQL is the single enforcement point.
- `kb.NewIndexer`/`kb.NewService` currently receive the deployment embedder; the per-user resolver is layered on top via setters.
- OpenSpec's archive apply refuses a `MODIFIED` block that omits an existing scenario name, so requirement blocks whose scenarios are inherently shared-only cannot be cleaned up with `MODIFIED`.

## Goals / Non-Goals

**Goals:**

- Every KB source belongs to exactly one user and is indexed/retrieved only with that user's embedding configuration.
- Keep directory ingestion and its read-only `origin='file'` semantics, now per-user and registered by the operator CLI.
- Leave no deployment-key code path in the KB (no shared provisioning, no deployment embedder fallback).

**Non-Goals:**

- No HTTP/UI source creation or `KB_PRIVATE_BASE` path confinement — the CLI is the trusted operator surface.
- No automatic re-embedding when a user changes embedding model (pre-existing gap, documented below).
- No change to feed RAG, chunking/normalisation, or the KB/LLM feature coupling.

## Decisions

**D1 — Drop the `scope` columns instead of keeping a constant `scope='user'`.** A one-valued column would stay in every index, query, and wire type while carrying no information. Dropping it, making `owner_id` NOT NULL on all three tables, and replacing the source-name index with `(owner_id, name)` makes ownership the single visibility dimension.

**D2 — Owner equality is re-asserted across chunk, document, and source rows.** The retrieval predicate requires `c.owner_id = d.owner_id = s.owner_id = caller` in addition to the existing generation, enabled-source, and embedding-model predicates. This keeps the defense-in-depth property the old scope predicates provided: a desynchronised denormalised column cannot widen the result.

**D3 — Directory sources are operator-CLI-only, per user.** The user confirmed they need to index directories such as `posts_douban/`, but only as private knowledge. Registration is `kb index --user <id> --root <dir> [--name <name>]`. Because the path never crosses an HTTP boundary, no `KB_PRIVATE_BASE` confinement or user-facing path validation is introduced; the CLI resolves and validates the directory like the old shared import did. The caller-facing API keeps only enable/disable/delete for sources the caller owns.

**D4 — No deployment embedder anywhere in KB.** `kb.NewIndexer`/`kb.NewService` take only the per-user resolver; `resolveEmbedder` returns nil when the resolver is absent or errors. `rag.NewDeploymentEmbedder` loses its last caller and is removed. This is what makes the deployment-spend guarantee structural rather than a side effect of which rows happen to exist.

**D5 — The CLI resolves the target user's BYOK embedding and refuses to index without it.** Reuse `llmsettings.NewService` + `llmsettings.NewResolver` + `rag.NewEmbedder`, exactly like `newPerUserEmbedderResolver` in `cmd/api`. Indexing under a deployment identity would recreate the shared-corpus spend; failing loudly tells the operator to have the user save embedding settings first.

**D6 — The migration deletes shared data by design.** Shared sources are deleted (FK cascade to documents and chunks), then the scope columns are dropped and `owner_id` is made NOT NULL. The down migration restores schema only; the shared corpus is not recoverable. Operators re-register and re-index. A `-- safe-migration:` annotation documents why a one-step contract is acceptable (KB is default-off; migrations run at boot before serving).

**D7 — The inline source name is reserved and both upserts are kind-guarded.** The new unique index is `(owner_id, name)`, so a directory named `inline` could otherwise collide with the inline source. `UpsertDirSource` carries `ON CONFLICT (owner_id, name) DO UPDATE … WHERE kind='dir'`, `EnsureInlineSource` carries `WHERE kind='inline'`, the CLI rejects `--name inline`, and a zero-row upsert maps to `ErrSourceNameConflict` (409) instead of a 500.

**D8 — Listing includes disabled sources; scans and outline exclude them.** With the shared source gone, every source is user-managed, so hiding a disabled source would make disabling irreversible from the UI. `GET /api/kb/sources` returns all owned sources; `IndexVisible` and `Outline` filter on `Enabled`; the retrieval SQL keeps `s.enabled`.

**D9 — The wire drops `scope`.** `SourceView` and the internal outline no longer carry it. The SPA is bundled with the backend; the internal outline is consumed only by the Python planner, which stops rendering `(scope: …)`. OpenAPI and the generated frontend types are regenerated.

**D10 — Sources are listed/retrieved by owner directly; no shared requirement survives.** Because the shared corpus is removed, the spec deltas use `REMOVED Requirements` + `ADDED` replacements for the four requirement blocks whose scenarios are inherently shared-only (the archive guard cannot drop a scenario under `MODIFIED`), and `MODIFIED` for blocks whose scenario names survive.

## Risks / Trade-offs

- **Destructive migration** → shared rows and their vectors are unrecoverable; down restores schema only. Documented in the proposal and migration annotation; KB is default-off.
- **Rolling-upgrade skew** → a pre-change binary expects the `scope` column. Mitigation: migrations run at boot before the new binary serves; deploy the new build atomically (this repo's deploy is restart-based, not a long-lived multi-version fleet).
- **Embedding-identity change strands existing chunks** → `ScanSource` skips unchanged hashes and `FillDocument` only fills the next generation, so changing the user's embedding model makes indexed directory documents unretrievable until they change or the source is re-registered. Mitigation: document the delete/re-register (and inline delete/recreate) workaround; a targeted stale-model re-index is a follow-up change, not part of this one.
- **Disabled-source visibility change** → `GET /api/kb/sources` now returns disabled sources; every consumer must keep excluding them from scans/retrieval. Mitigation: filters live in `IndexVisible`/`Outline` and the retrieval SQL is unchanged in that respect; covered by tests.
- **Inline-name collision** → guarded in SQL and the CLI (D7); a direct-SQL-created dir named `inline` cannot receive inline documents.
- **Coverage** → removing shared branches and their tests can lower the `internal/kb` package below the gate. Mitigation: replace them with owner-isolation, dir-upsert-conflict, disabled-filtering, and no-resolver tests.

## Migration Plan

1. Migration `000020_drop_kb_shared_scope` runs on backend boot (and on the CLI's boot, which also applies migrations): delete shared sources, drop the scope columns/constraints, set `owner_id NOT NULL`, install `(owner_id, name)` and `(owner_id)` indexes.
2. Rollback: `migrate down 1` restores the schema (scope columns default to `user`); shared data is gone by design.
3. Operator follow-up: re-register each former shared directory per user with `kb index --user <id> --root <dir>`; the user must have saved a BYOK embedding configuration.

## Open Questions

None.
