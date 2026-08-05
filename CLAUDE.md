# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repo shape

Nyx is a two-tier app: a Go 1.26.1 API (`backend/`) and a React 19 + Vite 8 SPA (`frontend/`), talking over REST. PostgreSQL is the source of truth; Redis is an opt-in cache-aside layer for `GET /movies`. Migrations live in `backend/migrations/` and are applied automatically on backend startup via `golang-migrate`.

The three FUTURE files (`FUTURE.md`, `FUTURE_BACKEND.md`, `FUTURE_FRONTEND.md`) are the live roadmap — tick an item there when the work lands.

## Common commands

All commands run from the repo root unless noted.

### Backend (Go)
- **Build**: `cd backend && go build ./...`
- **Unit tests (no Docker)**: `cd backend && SKIP_CONTAINERS=true go test ./...`
- **All tests (needs Docker for testcontainers)**: `cd backend && go test ./...`
- **Single package**: `cd backend && go test -v ./internal/movie/...`
- **Lint**: `cd backend && golangci-lint run --timeout=5m` (config at `backend/.golangci.yml`)
- **Regenerate sqlc bindings** (after editing `backend/queries/*.sql` or `backend/migrations/`): `cd backend && make sqlc`. See `backend/SQLC.md`.
- **Run with cache disabled**: `DB_URL=... JWT_SECRET=... go run ./cmd/api`
- **Run with cache enabled**: `DB_URL=... JWT_SECRET=... REDIS_ENABLED=true REDIS_URL=redis://localhost:6379 CACHE_TTL=5m go run ./cmd/api`

`SKIP_CONTAINERS=true` is the signal `TestMain` in `repository_integration_test.go` uses to skip the Postgres container bootstrap while still running unit tests. See "Test infra caveat" below.

### Frontend (Vite + Vitest + Playwright)
- **Dev server**: `cd frontend && npm run dev` → http://localhost:5173
- **Build**: `cd frontend && npm run build`
- **Lint**: `cd frontend && npm run lint` (config at `frontend/eslint.config.js`)
- **Unit tests**: `cd frontend && npm test`
- **Single file**: `cd frontend && npx vitest run src/features/movies/hooks/useMovies.test.ts`
- **E2E** (needs backend up): `cd frontend && npm run test:e2e` (config at `frontend/playwright.config.ts`; Playwright auto-starts the dev server)

### Full stack via Docker
- `cp .env.example .env`
- `sudo docker compose up --build -d` — brings up db (Postgres, host port 5433), redis, backend (8080), frontend (5173)
- DB host port is **5433** (mapped from container 5432). The README's `psql` line uses `-p 5433`.

## Architecture

### Backend layering (`backend/internal/`)
- `cmd/api/main.go` — wiring: config → DB → migrations → cache → movie + user services → gin router → graceful shutdown. Routes: `GET /api/health`, `/api/movies` (public list), `POST/PUT/DELETE /api/movies*` (JWT-guarded via `middleware.Auth()`), `/api/register`, `/api/login`, `/api/swagger/*`.
- `middleware/` — request ID, structured logging (zerolog), CORS, rate limit, JWT auth.
- `movie/` — clean-architecture domain. Files: `model.go` (entity + `MoviesPage` envelope + `NewMoviesPage`), `repository.go` (sqlc-generated querier; `GetAll` opens an internal tx for SELECT+COUNT consistency; "not found" returns `ErrNotFound` sentinel — handlers map it to HTTP 404), `service.go` (cache-aside wrapper around repo; `invalidate()` deletes the `movies:` prefix on every mutation), `handler.go` (Gin handlers). Tests use pgxmock for the handler and miniredis for the service.
- `user/` — registration + login (JWT issuance). No tests yet (open roadmap item).
- `platform/` — cross-cutting infrastructure:
  - `config/` — viper-based loader; see `Config` struct for the full env var list.
  - `database/` — `New(cfg)` opens a `*pgxpool.Pool` with pool tuning (MaxConns / MinConns / MaxConnLifetime / MaxConnIdleTime); `RunMigrations(url)` applies `backend/migrations/` via golang-migrate.
  - `cache/` — `Cache` interface (`Get/Set/Delete/DeletePrefix/Ping`) with two impls: `NewRedis(url)` (go-redis v9; `DeletePrefix` uses `SCAN MATCH + UNLINK`, non-blocking) and `NewNoop()` (used when `REDIS_ENABLED=false`).
  - `auth/`, `api/` — JWT helpers and error-response types.
- **Generated code** — `internal/movie/db/` and `internal/user/db/` are sqlc output (do not edit by hand). Regenerate with `make sqlc` after any query or schema change. See `backend/SQLC.md`.

### Frontend (`frontend/src/`)
Feature-first layout. Each feature owns its components, hooks, services, types, store.
- `features/movies/` — the main feature.
  - `services/movieService.ts` — paginated `getMovies(searchTerm, page, pageSize)` returning the `{data, page, page_size, total, has_more}` envelope.
  - `hooks/useMovies.ts` — `useInfiniteQuery` keyed on `['movies', searchTerm]`; mutations (`addMovie`/`updateMovie`/`deleteMovie`) use `onMutate` for optimistic updates with rollback in `onError` and reconciliation in `onSettled`. `addMovie.onSuccess` swaps the negative-id placeholder for the real Movie from the server; `updateMovie`/`deleteMovie` refuse negative ids. Optimistic placeholders use `-Date.now()` as a sentinel id.
  - `components/MovieList.tsx` — IntersectionObserver sentinel drives infinite scroll; skeleton placeholders shown while loading.
  - `components/MovieForm.tsx` — react-hook-form + zod.
- `features/auth/` — login/register modal.
- `components/ui/` — Radix-based primitives (`Dialog`, `Button`, `Label`); `cn()` helper in `src/utils` merges Tailwind classes.
- `services/api.ts` — configured axios instance with auth header injection.
- `store/` — Zustand stores (auth state).
- `test/setup.js` — IntersectionObserver polyfill for JSDOM.

### Cross-cutting
- **Pagination contract**: backend `GET /api/movies?page=N&page_size=M` → `{data, page, page_size, total, has_more}` (default 20, max 100). `has_more = page * page_size < total`. Search via `?q=`.
- **Cache contract**: backend key format `movies:q={query}:p={page}:s={size}`. Mutations call `cache.DeletePrefix(ctx, "movies:")` which SCANs + UNLINKs. Failures are logged but never fail the request.
- **Config**: all knobs are env vars read via viper (see `internal/platform/config/config.go`). `.env` is auto-loaded if present. Production must set `JWT_SECRET` to something other than the default placeholder.
- **Migrations**: numbered SQL files in `backend/migrations/`. New migration = `00000N_description.{up,down}.sql`. Applied on every backend boot.

## Conventions worth knowing

- **Commit style**: Conventional Commits (`feat:`, `fix:`, `refactor:`, `docs:`, `test:`, `chore:`). The repo just shipped a 4-commit split of a 3-phase plan (pagination + cache + UI + review fixes) — see git log if you need a template.
- **Backend error pattern**: use `errors.Is(err, movie.ErrNotFound)` (or the relevant domain sentinel) to detect missing rows. The handler translates `ErrNotFound` to HTTP 404; the old `err.Error() == "movie not found"` string compare has been removed.
- **Cache is best-effort**: every cache call site should swallow errors with a `log.Warn` and never fail the request — that's the documented invariant.
- **Tests**: backend uses pgxmock for handler/repo and miniredis for cache. Frontend uses vitest + RTL; e2e uses Playwright (auto-starts `vite dev`).
- **Pre-commit hook** (`.husky/pre-commit`): runs `lint-staged` on staged `src/**/*.{js,jsx,ts,tsx}` — `eslint --fix` + `vitest related --run --passWithNoTests`. Note: vitest v4's `related` is a subcommand, not a `--related` flag.

## Test infra caveat

`backend/internal/movie/repository_integration_test.go` defines a package-level `TestMain` that boots a Postgres testcontainer and runs migrations. It was previously calling `os.Exit(0)` when `SKIP_CONTAINERS=true`, which silently skipped every unit test in the package. It now always runs `m.Run()`; the integration tests self-skip via `t.Skip(...)` when `dbURL == ""`. Don't reintroduce the early-exit without checking the unit tests still execute.

## Open roadmap items (next candidates)

- **Fix Backend CI Integration Tests** (`FUTURE.md` §5) — `ci.yml` runs `go test -v ./...` with no Postgres service container. Set `SKIP_CONTAINERS=true` in CI, or add a Postgres service container.
- **User domain test coverage** — `user/handler.go` and `user/service.go` have no test files.
- **Search Debounce** — `App.tsx` fires an API request on every keystroke.
- **Distributed Tracing (OTel)** — partial wiring already in `go.mod` (`go.opentelemetry.io/otel`); no exporter wired in `main.go` yet.
- **Container Version Conflict Guardrail** — script checks for Postgres major-version volume upgrades.
- **Dynamic Metadata (SEO)** — per-page `<title>` and meta description.