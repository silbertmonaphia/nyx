# Proposal

## Why

The knowledge base's shared corpus is deployment-owned: it is the only KB path that spends the deployment's embedding key (its own scans plus the per-user catch-up that includes shared documents), and it exposes operator content to every user whose embedding identity matches. Knowledge is per-user data — each user brings their own LLM/embedding configuration — so every knowledge source should belong to exactly one user. Directory ingestion is still required (e.g. a personal `posts_douban/` tree), but as a private source rather than a deployment-wide one.

## What Changes

- Remove the `shared` scope entirely: the migration deletes the shared source and its documents/chunks, drops the `scope` columns from `kb_sources`/`kb_documents`/`kb_chunks`, makes `owner_id` NOT NULL, and enforces one source name per owner via `(owner_id, name)`.
- Keep `kind='dir'` file-backed sources, now owner-scoped. They are registered only by the operator CLI: `kb index --user <id> --root <dir> [--name <name>]`, indexed with the target user's BYOK embedding configuration. No HTTP or UI source creation; no `KB_PRIVATE_BASE`.
- **BREAKING** `GET /api/kb/sources` and `GET /internal/rag/outline` stop emitting `scope`; `KB_ROOT` and `KB_SHARED_SOURCE_NAME` are removed and boot-time shared provisioning is deleted.
- The KB never constructs or falls back to a deployment embedder; indexing and retrieval require the user's own embedding settings.
- `GET /api/kb/sources` lists all of the caller's sources, including disabled ones (so a source can be re-enabled); scans, the agent outline, and retrieval exclude disabled sources.
- The SPA drops the scope badge and the `scope === 'user'` control gate; enable/disable and delete are available for every source. The agent planner outline no longer carries or prints a scope.
- The CLI `kb status` reports `owner_id` instead of `scope`; it no longer reports a deployment embedding model.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `kb-sources`: a source records its owner (not a scope); directory sources are operator-registered per user; the shared-source visibility and deletion rules are removed.
- `kb-ingestion`: the explicit-index trigger scans only the caller's own enabled sources (no shared source).
- `kb-retrieval`: visibility is owner-only with chunk/document/source ownership re-assertion; the shared-corpus identity requirement is removed; the outline no longer carries scope and lists only the caller's sources.
- `agent-rag-bridge`: knowledge retrieval and the outline are scoped to the caller's own sources only.

## Impact

- **Code**: `backend/queries/kb.sql` + sqlc output, `backend/internal/kb/*`, a new migration `000020`, `backend/cmd/kb/*`, `backend/cmd/api/main.go`, `backend/internal/rag/deployment.go` + wire-contract test, `backend/internal/platform/config/config.go`, `backend/agent_py/agent/planner.py`, the KB page/types/i18n and regenerated OpenAPI types, and operator docs.
- **API**: breaking removal of the `scope` field from the KB source list and the internal outline. No new endpoints.
- **Data**: existing shared documents and their vectors are deleted; operators re-register the directory against a user and re-index. The down migration restores schema only.
- **Operations**: `KB_ROOT`/`KB_SHARED_SOURCE_NAME` disappear; `kb index --user` needs `USER_LLM_ENCRYPTION_KEY` and a saved BYOK embedding configuration for the target user.
- **Rollout gates**: none — the KB is default-off and every task is completable in the dev/CI environment, so no sibling `<change>-rollout` change is created.
