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
- **Errors**: domain sentinels (`movie.ErrNotFound`, `user.ErrInvalidCredentials`, `auth.ErrInvalidToken`, `auth.ErrExpiredToken`). Handlers translate to HTTP status via the central error mapper. Never string-compare error messages.
- **Cache is best-effort**: every cache call swallows errors with `log.Warn` and never fails the request.
- **Pagination**: `GET /api/movies?page=N&page_size=M` → `{data, page, page_size, total, has_more}`. Default 20, max 100.
- **Cache keys**: `movies:q={query}:p={page}:s={size}`. Mutations call `DeletePrefix("movies:")` (SCAN + UNLINK, non-blocking).
- **Migrations**: `backend/migrations/00000N_description.{up,down}.sql`. Applied on every backend boot.
- **Config**: env vars via viper (`internal/platform/config/config.go`). `.env` auto-loaded if present. Production must set `JWT_SECRET` off the default placeholder.
- **Test infra**: `backend/internal/movie/repository_integration_test.go` boots a Postgres testcontainer; integration tests self-skip via `t.Skip()` when `dbURL == ""`. Never reintroduce an early `os.Exit(0)` in `TestMain` — it silently skips unit tests.
- **Pre-commit** (`.husky/pre-commit`): `lint-staged` → `eslint --fix` + `vitest related --run --passWithNoTests`. Note: vitest v4's `related` is a subcommand, not a flag.
- **Tests**: backend uses pgxmock (handler/repo) + miniredis (cache); frontend uses vitest + RTL; e2e uses Playwright (auto-starts `vite dev`).
- **sqlc drift**: CI runs `make sqlc-diff` and fails if generated code is out of sync with `queries/*.sql` or migrations. Regenerate locally and commit before pushing.

---

# Open roadmap (next candidates)

- User domain test coverage — `user/huma_handler.go` lacks a `handler_test.go`. (`service_test.go` and `repository_test.go` exist.)
- Search Debounce — `App.tsx` fires an API request on every keystroke.
- Distributed Tracing (OTel) — `go.opentelemetry.io/otel` is in `go.mod`; no exporter wired in `main.go` yet.
- Container Version Conflict Guardrail — script checks for Postgres major-version volume upgrades.
- Dynamic Metadata (SEO) — per-page `<title>` and meta description.
- CORS Hardening — `cors.go` still allows `*`. Configurable allowlist pending.
- JWT Secret via Viper Config — `auth/jwt.go` reads via `os.Getenv`; consolidate to the viper config struct.
- JWT Refresh Tokens — current tokens expire in 24h, no refresh flow.
- Persist Auth Token Securely — JWT is in `localStorage` via Zustand `persist`. httpOnly cookies (or a documented XSS caveat) pending.
