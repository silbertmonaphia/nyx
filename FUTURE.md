# Nyx Roadmap

Single source of truth for "what's done / what's next" across the stack. Tick an item when the work lands. Deeper context for each area lives in `FUTURE_BACKEND.md` and `FUTURE_FRONTEND.md`.

## 1. Reliability & Observability

- [x] Structured logging (`rs/zerolog`, JSON output, request IDs).
- [x] Prometheus `/metrics` endpoint + `prometheus` middleware.
- [x] Graceful shutdown (`SIGTERM`/`SIGINT` + `context`).
- [x] Health checks — `/api/health` includes DB connectivity.
- [ ] Distributed Tracing — `go.opentelemetry.io/otel` is in `go.mod`; no exporter wired in `main.go` yet.

## 2. API Maturity & Security

- [x] OpenAPI 3.1 — generated at runtime from huma struct tags; UI at `/api/swagger` (Stoplight Elements). See `backend/HUMA.md`.
- [x] JWT auth on write endpoints (`POST/PUT/DELETE /api/movies`).
- [x] Clean architecture — `internal/{movie,user}/` with `model / repository / service / huma_handler`.
- [x] Rate limiting middleware (token bucket).
- [x] Middleware chain on chi v5 — RequestID, RealIP, Recoverer, Prometheus, Logging, CORS, RateLimit.
- [x] Standardized JSON error envelopes.
- [x] Domain error sentinels — `movie.ErrNotFound`, `user.ErrInvalidCredentials`, `auth.ErrInvalidToken`, `auth.ErrExpiredToken`. Handlers translate to HTTP status; never string-compare.
- [x] Semantic API Error Translators — `internal/platform/pgerr` translates `pgconn.PgError` codes + constraint names to domain sentinels (`user.ErrUsernameTaken`, `user.ErrEmailTaken`, `user.ErrRefreshTokenCollision`); `api.MapError` funnels every handler error through one call.
- [x] CORS Hardening — `cors.go` now reads `CORS_ALLOWED_ORIGINS` (default `*`); set a comma-separated origin list in production.
- [x] JWT Secret via Viper Config — `auth.SetSecret(cfg.JWTSecret)` is called once in `main.go`; `auth/jwt.go` no longer reads `os.Getenv`.
- [x] JWT Refresh Tokens — short-lived access tokens (default 15m, configurable via `JWT_ACCESS_TTL`) paired with opaque rotated refresh tokens (default 7d, `JWT_REFRESH_TTL`) and family-level reuse detection. Frontend refreshes on `WWW-Authenticate: Bearer error="invalid_token", error_description="expired"`.

## 3. Database Lifecycle

- [x] Migration engine — `golang-migrate`, applied on every backend boot.
- [x] Audit fields — `created_at`, `updated_at`, `deleted_at` (soft deletes) on `movies` and `users`.
- [x] Integration testing — `testcontainers-go` Postgres in `backend/internal/movie/repository_integration_test.go`.
- [x] Connection pooling — pgxpool settings via viper.
- [x] Cache-aside — Redis for `GET /api/movies` (opt-in via `REDIS_ENABLED=true`). Best-effort, never fails the request.
- [x] Index optimization — `migrations/000006_add_movies_indexes.sql`, `000007_add_movies_description_trgm_index.sql`.
- [x] Type-safe SQL — `sqlc` over `pgx/v5` + `pgxpool`. `make sqlc` to regenerate; `make sqlc-diff` in CI.
- [ ] Container Version Conflict Guardrail — script to warn / auto-prune volumes on Postgres major-version upgrades.

## 4. Frontend

- [x] TypeScript end-to-end.
- [x] Feature-based folder structure (`src/features/{movies,auth}/`).
- [x] Server state — TanStack Query (caching, background refetch, pagination).
- [x] Form validation — React Hook Form + Zod.
- [x] Client state — Zustand (`authStore`, `uiStore`, `movieUiStore`).
- [x] Styling — Tailwind CSS v4.
- [x] UI primitives — Radix UI (`Dialog`, `Label`, `Slot`) behind shadcn-style components (Button, Card, Dialog, Input, Label, Textarea, ToastContainer).
- [x] Optimistic updates — `useMovies.ts` uses `onMutate` for create / update / delete.
- [x] Loading skeletons — `SkeletonCard`, `MovieListSkeleton` in `MovieList.tsx`.
- [x] Confirm dialog — `Dialog` (Radix) replaces `window.confirm()` in `App.tsx`.
- [x] Toast notifications — `ToastContainer` + `uiStore`.
- [x] Axios interceptors — auth header injection + global 401 handling.
- [x] Search Debounce — `App.tsx` fires an API request on every keystroke. Add a 300ms debounce.
- [x] Dynamic Metadata (SEO) — per-page `<title>` and meta description.
- [ ] Persist Auth Token Securely — JWT in `localStorage` via Zustand `persist`. httpOnly cookies (or a documented XSS caveat) pending.
- [x] Token Refresh — single-flight refresh-on-401 in `api.ts`, driven by the `WWW-Authenticate` challenge from the auth middleware. Refresh tokens persist in the auth store alongside the access token.

## 5. Developer Experience & CI/CD

- [x] GitHub Actions — jobs: `backend-test` (lint + sqlc-diff + tests), `frontend-test` (lint + test + build), `e2e-test` (depends on both).
- [x] Backend CI service container — `postgres:17-alpine` in `backend-test` and `e2e-test` jobs so `testcontainers-go` tests run.
- [x] E2E — Playwright in `frontend/tests/e2e`.
- [x] Backend lint — `golangci-lint` in CI.
- [x] Frontend lint — `eslint` + `husky` pre-commit + `lint-staged`.
- [x] User Domain Handler Test Coverage — `user/huma_handler_test.go` covers register/login happy paths, validation rejections (missing/short username, invalid email, short password), duplicate 409, unknown user 401, bad password 401, and 500 paths.
- [x] Kubernetes manifests — `Deployment`, `Service`, `Ingress`, `Secrets`, `StatefulSet` (Postgres), `Deployment` (Redis) in `k8s/`.
- [x] Viper config — multi-source (env + `.env` + defaults).

---

*Nyx: Minimalist by design, production-credible by choice.*
