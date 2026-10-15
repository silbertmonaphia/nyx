# Tasks

## 1. Testcontainer image

- [x] 1.1 Change `backend/test/testcontainers.go`'s image to `pgvector/pgvector:pg17` (the image CI's `backend-test` service already runs) and verify `cd backend && go test ./internal/feed/...` is green with Docker and no `DB_URL` — the path that currently dies on `CREATE EXTENSION vector` (2026-10-15: `ok nyx/internal/feed 8.3s` — previously a hard fail on the missing extension)
- [x] 1.2 Verify `internal/user`'s integration suite is green on the same helper (it shares `test.StartPostgres`), so no package depends on the old image (2026-10-15: `ok nyx/internal/user 4.1s`)
- [x] 1.3 Verify the skip path is untouched: `SKIP_CONTAINERS=true go test ./...` green, and `go vet ./test/...` + `go build ./...` clean (2026-10-15: all green; the helper is only reached on the Docker path)
- [x] 1.4 Flake found while verifying 3.1: `TestRedisIntegrationContainerRestartReconnectsTransparently` failed ~1 run in 3 under full `go test ./...` load with `container start: No such container` — the PING-failure test's best-effort cleanup restart left the shared container reaped/terminated, and the next test's `Start` then had nothing to start. Fix: the three mutating tests (noeviction, PING-failure, restart) each take a dedicated container via `newDedicatedRedis`, leaving the shared one read-only, so no test's outcome depends on another's container surgery. Evidence: full `go test -count=1 ./...` × 5 → 0 failures (previously ~1 in 3)

## 2. Workflow token scope

- [x] 2.1 Add `permissions: contents: read` at the top level of `.github/workflows/ci.yml` (after the `on:` block) and verify the file's YAML parses via `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"` (2026-10-15: parsed; top-level `permissions: {'contents': 'read'}`)
- [x] 2.2 Verify the `release` job keeps its job-level `permissions:` block (contents/issues/pull-requests write) and that it is the only job needing more than read, so the default cannot silently break tag/release creation (2026-10-15: `release` is the only job with its own block, and job-level overrides the workflow default)

## 3. Verification

- [x] 3.1 Run `cd backend && go test ./...` locally with Docker and no `DB_URL` and confirm `internal/feed` and `internal/user` both pass (previously the two red packages) (2026-10-15: five consecutive full runs green after 1.4 — `internal/feed` 10.2s, `internal/user` 5.9s, cache suite 16.2s)
- [x] 3.2 Push and verify the CI run: `backend-test` unchanged-green (it uses `DB_URL`), `release` still creates its release (job-level permissions intact), every other job unchanged; record the run URL (2026-10-15: https://github.com/silbertmonaphia/nyx/actions/runs/38732575566 — run `success`; `backend-test`, `frontend-test` and `release` all green, so the workflow-level `contents: read` default did not break tag/release creation)
