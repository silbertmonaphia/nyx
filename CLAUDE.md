CLAUDE.md

# Repo shape
Nyx: Go 1.26.1 API (`backend/`, `chi v5` + `huma v2`) + React 19 + Vite 8 SPA (`frontend/`), talking REST. PostgreSQL is source of truth; Redis is opt-in cache-aside for `GET /api/movies`. SQL is type-safe via `sqlc` over `pgx/v5` + `pgxpool`. Migrations in `backend/migrations/`, applied automatically on backend boot via `golang-migrate`.

Roadmap lives in `FUTURE.md`, `FUTURE_BACKEND.md`, `FUTURE_FRONTEND.md` — tick an item when the work lands.

---

# Commands

Backend (from repo root):
- Build: `cd backend && go build ./...`
- Unit tests (no Docker): `cd backend && SKIP_CONTAINERS=true go test ./...`
- Full tests (needs Docker/testcontainers): `cd backend && go test ./...`
- Lint: `cd backend && golangci-lint run --timeout=5m`
- Regenerate sqlc (after `queries/*.sql` or migrations change): `cd backend && make sqlc`
- Verify sqlc output is in sync (CI does this): `cd backend && make sqlc-diff`
- Run: `DB_URL=... JWT_SECRET=... go run ./cmd/api` — add `REDIS_ENABLED=true REDIS_URL=redis://localhost:6379 CACHE_TTL=5m` to enable cache

Frontend:
- Dev: `cd frontend && npm run dev` → http://localhost:5173
- Build / lint / unit tests: `npm run build` / `npm run lint` / `npm test`
- E2E (needs backend up): `npm run test:e2e`

Full stack: `cp .env.example .env && sudo docker compose up --build -d`. DB host port is **5433** (mapped from container 5432).

---

# Layout

- `backend/cmd/api/main.go` — wiring (config → DB → cache → domains → router → http.Server).
- `backend/internal/middleware/` — chi middlewares: RequestID, RealIP, Recoverer, Prometheus, Logging, CORS, RateLimit, plus `huma_adapter.go` for huma-compatible ctx values.
- `backend/internal/movie/` and `backend/internal/user/` — clean-arch domains (`model.go`, `repository.go`, `service.go`, `huma_handler.go`). Tests live alongside (`service_test.go`, `repository_test.go`, `repository_integration_test.go`, `handler_test.go`).
- `backend/internal/platform/` — `config/` (viper), `database/` (pgxpool + golang-migrate), `cache/` (Redis + noop), `auth/` (JWT), `api/` (huma helpers, error mapper).
- `backend/internal/reqctx/` — typed values stashed on `context.Context` (auth claims, request ID, etc.).
- `backend/internal/{movie,user}/db/` — sqlc output, **do not edit by hand**. Regenerate with `make sqlc`. See `backend/SQLC.md`.
- `backend/queries/*.sql` — sqlc input.
- `frontend/src/features/{movies,auth}/` — components, hooks, services, types, store. `services/api.ts` (axios), `store/` (Zustand), `test/setup.js` (IntersectionObserver polyfill).
- Adding an HTTP endpoint: see `backend/HUMA.md`.

---

# Conventions

- **Commits**: Conventional Commits.
- **Errors**: domain sentinels (`movie.ErrNotFound`, `user.ErrInvalidCredentials`, `auth.ErrInvalidToken`, `auth.ErrExpiredToken`, plus the `pgerr`-translated `user.ErrUsernameTaken` / `user.ErrEmailTaken` / `user.ErrRefreshTokenCollision`). Handlers translate to HTTP status via `api.MapError` (the central funnel in `internal/platform/api`). Never string-compare error messages.
- **Response details must be safe to ship**: handlers/middleware never copy `err.Error()` into the response `details` field. Use `api.ClassifyAndLog(ctx, err, "Operation failed")` — it logs the wrapped error with the request ID at `Warn` and returns the static `safeDetail` you pass in. SQL fragments, bcrypt strings, and JWT parser errors must never reach the wire.
- **Cache is best-effort**: every cache call swallows errors with `log.Warn` and never fails the request.
- **Pagination**: `GET /api/movies?page=N&page_size=M` → `{data, page, page_size, total, has_more}`. Default 20, max 100.
- **Cache keys**: `movies:q={query}:p={page}:s={size}`. Mutations call `DeletePrefix("movies:")` (SCAN + UNLINK, non-blocking).
- **Migrations**: `backend/migrations/00000N_description.{up,down}.sql`. Applied on every backend boot.
- **Config**: env vars via viper (`internal/platform/config/config.go`). `.env` auto-loaded if present. `JWT_SECRET` is validated at startup: `config.Load()` refuses the built-in default and any key shorter than `auth.MinSecretBytes` (32 bytes, RFC 7518 §3.2). `auth.NewTokenService` repeats the check so the constructor is independently safe.
- **Test infra**: `backend/internal/movie/repository_integration_test.go` boots a Postgres testcontainer; integration tests self-skip via `t.Skip()` when `dbURL == ""`. Never reintroduce an early `os.Exit(0)` in `TestMain` — it silently skips unit tests.
- **Pre-commit** (`.husky/pre-commit`): `lint-staged` → `eslint --fix` + `vitest related --run --passWithNoTests`. Note: vitest v4's `related` is a subcommand, not a flag.
- **Tests**: backend uses pgxmock (haqndler/repo) + miniredis (cache); frontend uses vitest + RTL; e2e uses Playwright (auto-starts `vite dev`).
- **sqlc drift**: CI runs `make sqlc-diff` and fails if generated code is out of sync with `queries/*.sql` or migrations. Regenerate locally and commit before pushing.

---

# Open roadmap (next candidates)

- Distributed Tracing (OTel) — landed on backend and frontend. `OTEL_ENABLED=false` + `VITE_OTEL_ENABLED != "true"` by default. Backend exports via OTLP/HTTP → Jaeger (compose `jaeger` service, UI on `:16686`); frontend exports via the SPA container's nginx `location /otlp/` proxy → Jaeger (same-origin, bypasses Jaeger's missing CORS). Browser axios injects W3C `traceparent` so backend spans are children of the SPA root span. `service.name=nyx-frontend` for browser spans, `service.name=nyx-backend` for server spans. See `FUTURE_BACKEND.md` §5 and `FUTURE_FRONTEND.md` §9.
- Container Version Conflict Guardrail — script checks for Postgres major-version volume upgrades.
- Persist Auth Token Securely — JWT is in `localStorage` via Zustand `persist`. httpOnly cookies (or a documented XSS caveat) pending.

# Communication
Keep replies and commit messages terse. No preamble, no restating the diff, no trailing pleasantries. Skip a commit body if the subject already says it.
