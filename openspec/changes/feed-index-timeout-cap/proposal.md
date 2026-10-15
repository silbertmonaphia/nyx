# Proposal

## Why

The 2026-10-15 embedder outage (vLLM down, port accepting-but-hanging) turned every feed write into a ~62-second request: `CreateFeed` / `UpdateFeed` / `DeleteFeed` call the embedding indexer synchronously (`backend/internal/feed/service.go:151/171/191`), and with the embedder unreachable that best-effort call waits out the shared `LLM_TIMEOUT` (default 60s, `config.go:344`) before finally logging a warning. The outcome of the wait is predetermined — the write already succeeds and indexing failures are tolerated by design (read-time `BackfillOnce`, `internal/chat/service.go:257`) — so the entire 60s is pure latency paid by the user, and it reddened the `feed-crud` e2e spec as collateral. During a long embedder outage, every write pays the tax.

## What Changes

- New setting `FEED_INDEX_TIMEOUT` (viper, default `5s`, validated as a positive duration) — independent from `LLM_TIMEOUT`, which keeps its chat-streaming semantics.
- `feed.Service` applies the setting as a per-call context deadline around the three best-effort indexer calls (create/update/delete). On deadline or error: same `log.Warn` + continue contract as today; the write result is unchanged. A healthy embedder completes a single-feed embed in well under the cap, so normal-path behavior is byte-identical.
- Constructor + `main.go` wiring updated to carry the timeout; `NoopEmbeddingIndexer` deployments unaffected.
- Tests: unit tests with a blocking indexer stub proving each write returns within the cap with a warning and a successful result; a healthy-stub test proving the index call still runs to completion; config default/validation tests.
- **Wire**: none. No new frames, endpoints, or response-shape changes.
- Docs: `backend/.env.example` gains the commented row next to `LLM_TIMEOUT`.

## Capabilities

### New Capabilities

- `feed-embed-indexing`: the write-path embedding contract — best-effort, bounded by a configured deadline, healable by the retrieve-time backfill.

### Modified Capabilities

- None.

## Impact

- **Code**: `backend/internal/feed/service.go` (three call sites + constructor), `backend/internal/platform/config/config.go` (setting + validation), `backend/cmd/api/main.go` (wiring), `backend/internal/feed/service_test.go` (bounded-wait tests).
- **Wire**: none. **Dependencies**: none.
- **Docs**: `backend/.env.example`.
- **Tests**: the unit tests above are the deterministic gate; task 5 re-runs the `feed-crud` e2e against the live stack and records an outage simulation (embedder stopped → write returns within cap, content applied).
- **Rollout gates**: none — the classification test (`openspec/config.yaml`) says dev/CI-completable with no deployed environment or clock; a normal deploy carries it. Why not async/queue-based indexing: design.md D2 records the considered alternatives (in-process queue, transactional outbox, Redis, LISTEN/NOTIFY) and the revival criteria.

**Non-goals**: async/durable indexing infrastructure; changing the retriever or the backfill's trigger cadence; circuit-breaking the embedder client (folded into D2's revival list).
