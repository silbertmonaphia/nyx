# Tasks

## 1. Setting

- [ ] 1.1 Add `FeedIndexTimeout string` (`mapstructure:"FEED_INDEX_TIMEOUT"`) with `viper.SetDefault("FEED_INDEX_TIMEOUT", "5s")` and a positive `time.ParseDuration` validation alongside the existing timeout checks in `backend/internal/platform/config/config.go`. Verify: `SKIP_CONTAINERS=true go test ./internal/platform/config/ -count=1` — default loads as 5s; `FEED_INDEX_TIMEOUT=0s` and `FEED_INDEX_TIMEOUT=garbage` each fail validation with a message naming the variable.

## 2. Bounded write path

- [ ] 2.1 Thread the timeout into `feed.NewService` (constructor parameter or a setter following the existing option style) and wrap the three best-effort calls (`service.go:151/171/191`) with `context.WithTimeout` at the configured value. No other behavior changes: warn message text, span usage, and the no-op indexer path stay as they are. Verify: `SKIP_CONTAINERS=true go test ./internal/feed/ -count=1 -v` — a stub indexer that blocks past the cap makes `CreateFeed`, `UpdateFeed`, and `DeleteFeed` each return success and well under 2×cap (assert with a wall-clock bound), emitting the warning; a stub that completes proves the index call still runs to completion.
- [ ] 2.2 Pin D5: the capped-out path leaves the feed eligible for backfill — assert no "indexed" marker is set (the stub's skip is recorded, the service result is success). Verify: the same `internal/feed` test file, run as in 2.1.

## 3. Wiring

- [ ] 3.1 `backend/cmd/api/main.go` parses `cfg.FeedIndexTimeout` once at boot and passes it to `feed.NewService`; a malformed value is already rejected by 1.1 at config load. Verify: `go build ./...` + `SKIP_CONTAINERS=true go test ./... -count=1` — all callers compile, unit suite green.

## 4. Docs

- [ ] 4.1 `backend/.env.example`: commented `# FEED_INDEX_TIMEOUT=5s` row next to `LLM_TIMEOUT`, one line explaining the best-effort cap. Verify: row present; no other doc references a different name (grep `FEED_INDEX_TIMEOUT`).

## 5. End-to-end verification

- [ ] 5.1 Live-stack check: `feed-crud` e2e green against the running stack (`VITE_API_URL=... npx playwright test tests/e2e/feed-crud.spec.ts --workers=1`). Record the run output in this task.
- [ ] 5.2 Outage simulation: stop the embed server (`make stop` host-vLLM embed / compose profile down), then `curl -w %{time_total} -X PUT` an existing feed with a fresh token — response arrives within the cap and the update is applied (re-read returns the new content). Restore the embedder and confirm the next RAG chat backfills the skipped feed (Jaeger span `rag.backfill` or a row appears in `feed_embeddings`). Record timings in this task.
