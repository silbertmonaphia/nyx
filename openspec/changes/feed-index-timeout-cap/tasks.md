# Tasks

## 1. Setting

- [x] 1.1 Add `FeedIndexTimeout string` (`mapstructure:"FEED_INDEX_TIMEOUT"`) with `viper.SetDefault("FEED_INDEX_TIMEOUT", "5s")` and a positive `time.ParseDuration` validation alongside the existing timeout checks in `backend/internal/platform/config/config.go`. Verify: `SKIP_CONTAINERS=true go test ./internal/platform/config/ -count=1` — default loads as 5s; `FEED_INDEX_TIMEOUT=0s` and `FEED_INDEX_TIMEOUT=garbage` each fail validation with a message naming the variable. (Done: `TestLoadAppliesDefaults` pins the 5s default; `TestLoadRejectsUnparseableFeedIndexTimeout` / `TestLoadRejectsNonPositiveFeedIndexTimeout` pin both rejections.)

## 2. Bounded write path

- [x] 2.1 Thread the timeout into `feed.NewService` (constructor parameter or a setter following the existing option style) and wrap the three best-effort calls (`service.go:151/171/191`) with `context.WithTimeout` at the configured value; on a skipped create/update, best-effort drop the embedding row (`indexer.Delete`, same cap) per design D5. No other behavior changes: warn message text, span usage, and the no-op indexer path stay as they are. Verify: `SKIP_CONTAINERS=true go test ./internal/feed/ -count=1 -v` — a stub indexer that blocks past the cap makes `CreateFeed`, `UpdateFeed`, and `DeleteFeed` each return success and well under 2×cap (assert with a wall-clock bound), emitting the warning; a stub that completes proves the index call still runs to completion. (Done: `TestWrites_BoundTheIndexCall` — the blocking stub only exits via the cap deadline, so the test hangs without the fix; `TestNewService_ClampsNonPositiveIndexTimeout` pins the default fallback.)
- [x] 2.2 Pin D5: the capped-out path leaves the feed eligible for backfill — assert the stub recorded the row drop (`Delete` called) after the skipped index, with the write still succeeding. Verify: the same `internal/feed` test file, run as in 2.1. (Done: the bounded-write test asserts `deleteEntered` on all three ops.)

## 3. Wiring

- [x] 3.1 `backend/cmd/api/main.go` parses `cfg.FeedIndexTimeout` once at boot and passes it to `feed.NewService`; a malformed value is already rejected by 1.1 at config load. Verify: `go build ./...` + `SKIP_CONTAINERS=true go test ./... -count=1` — all callers compile, unit suite green. (Done: full suite green; the `feed` + `config` lint pass covers the constructor signature change across handler tests.)

## 4. Docs

- [x] 4.1 `backend/.env.example`: commented `# FEED_INDEX_TIMEOUT=5s` row next to `LLM_TIMEOUT`, one line explaining the best-effort cap. Verify: row present; no other doc references a different name (grep `FEED_INDEX_TIMEOUT`).

## 5. End-to-end verification

- [x] 5.1 Live-stack check: `feed-crud` e2e green against the running stack (`VITE_API_URL=... npx playwright test tests/e2e/feed-crud.spec.ts --workers=1`). Record the run output in this task. (Done 2026-10-15: `1 passed (9.0s)` — worktree-built backend on :8081 with the real vLLM embedder on the write path.)
- [x] 5.2 Outage simulation: stop the embed server (`make stop` host-vLLM embed / compose profile down), then `curl -w %{time_total} -X PUT` an existing feed with a fresh token — response arrives within the cap and the update is applied (re-read returns the new content). Restore the embedder and confirm the next RAG chat backfills the skipped feed (Jaeger span `rag.backfill` or a row appears in `feed_embeddings`). Record timings in this task. (Done 2026-10-15, with one substitution: instead of stopping the operator's vLLM, the side backend pointed its embed endpoint at a local TCP blackhole listener — accept-but-never-respond, the incident's exact failure mode — so the operator stack stayed untouched. Evidence chain: baseline row hash `382663d6…` → timed PUT `200` in **5.24s** (cap; previously 60s) with the content applied and the stale row dropped (`feed_embeddings` count 0, the D5 drop-on-skip) → after the `detachContext` fix (see the sibling commit — the simulation caught that the lazy backfill never ran at all, canceling its own context before first use) a `rag:true` chat restored the row with the new hash `0e25252b…`.)
