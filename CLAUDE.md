# CLAUDE.md

## Repo shape
Nyx: Go 1.26.1 API (`backend/`) + React 19 + Vite 8 SPA (`frontend/`), talking REST. PostgreSQL is source of truth; Redis is opt-in cache-aside for `GET /movies`. Migrations in `backend/migrations/`, applied automatically on backend boot via `golang-migrate`.

Roadmap lives in `FUTURE.md`, `FUTURE_BACKEND.md`, `FUTURE_FRONTEND.md` — tick an item when the work lands.

## Commands

Backend (from repo root):
- Build: `cd backend && go build ./...`
- Unit tests (no Docker): `cd backend && SKIP_CONTAINERS=true go test ./...`
- Full tests (needs Docker/testcontainers): `cd backend && go test ./...`
- Lint: `cd backend && golangci-lint run --timeout=5m`
- Regenerate sqlc (after `queries/*.sql` or migrations change): `cd backend && make sqlc`
- Run: `DB_URL=... JWT_SECRET=... go run ./cmd/api` — add `REDIS_ENABLED=true REDIS_URL=redis://localhost:6379 CACHE_TTL=5m` to enable cache

Frontend:
- Dev: `cd frontend && npm run dev` → http://localhost:5173
- Build / lint / unit tests: `npm run build` / `npm run lint` / `npm test`
- E2E (needs backend up): `npm run test:e2e`

Full stack: `cp .env.example .env && sudo docker compose up --build -d`. DB host port is **5433** (mapped from container 5432).

## Layout
- `backend/internal/` — `cmd/api` (wiring), `middleware/`, `movie/` + `user/` (clean-arch domains: `model.go`, `repository.go`, `service.go`, `huma_handler.go`), `platform/` (`config/`, `database/`, `cache/`, `auth/`, `api/`), `reqctx/`.
- `backend/internal/{movie,user}/db/` — sqlc output, do not edit by hand. Regenerate with `make sqlc`. See `backend/SQLC.md`.
- `frontend/src/features/{movies,auth}/` — components, hooks, services, types, store. `services/api.ts` (axios), `store/` (Zustand), `test/setup.js` (IntersectionObserver polyfill).
- Adding an HTTP endpoint: see `backend/HUMA.md`.

## Conventions
- **Commits**: Conventional Commits.
- **Errors**: domain sentinels (e.g. `movie.ErrNotFound`); handlers translate to HTTP status. Never string-compare error messages.
- **Cache is best-effort**: every cache call swallows errors with `log.Warn` and never fails the request.
- **Pagination**: `GET /api/movies?page=N&page_size=M` → `{data, page, page_size, total, has_more}`. Default 20, max 100.
- **Cache keys**: `movies:q={query}:p={page}:s={size}`. Mutations call `DeletePrefix("movies:")` (SCAN + UNLINK, non-blocking).
- **Migrations**: `backend/migrations/00000N_description.{up,down}.sql`. Applied on every backend boot.
- **Config**: env vars via viper (`internal/platform/config/config.go`). `.env` auto-loaded if present. Production must set `JWT_SECRET` off the default placeholder.
- **Test infra**: `backend/internal/movie/repository_integration_test.go` boots a Postgres testcontainer; integration tests self-skip via `t.Skip()` when `dbURL == ""`. Never reintroduce an early `os.Exit(0)` in `TestMain` — it silently skips unit tests.
- **Pre-commit** (`.husky/pre-commit`): `lint-staged` → `eslint --fix` + `vitest related --run --passWithNoTests`. Note: vitest v4's `related` is a subcommand, not a flag.
- **Tests**: backend uses pgxmock (handler/repo) + miniredis (cache); frontend uses vitest + RTL; e2e uses Playwright (auto-starts `vite dev`).

## Open roadmap (next candidates)
- Fix Backend CI Integration Tests (`FUTURE.md` §5) — `ci.yml` runs `go test -v ./...` with no Postgres service container.
- User domain test coverage — `user/huma_handler.go` and `user/service.go` have no test files.
- Search Debounce — `App.tsx` fires an API request on every keystroke.
- Distributed Tracing (OTel) — `go.opentelemetry.io/otel` in `go.mod`; no exporter wired in `main.go` yet.
- Container Version Conflict Guardrail — script checks for Postgres major-version volume upgrades.
- Dynamic Metadata (SEO) — per-page `<title>` and meta description.
