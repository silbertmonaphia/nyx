# Nyx Backend: Architectural Evolution

The history and design decisions behind the current backend. For the high-level "done / next" checklist, see `FUTURE.md` §1–3, 5.

## 1. Core Framework & Architecture

- [x] **Migrate to chi v5 + huma v2** (superseded the original Gin attempt). Stdlib-compatible `func(http.Handler) http.Handler` middleware chain on chi v5; declarative `huma.Operation{}` on huma v2 emits OpenAPI 3.1 from struct tags. REST contract preserved byte-for-byte (paths, methods, JSON envelopes) so the frontend, e2e suite, and CI are unaffected. See `backend/HUMA.md`.
- [x] **Clean Architecture** — `internal/{movie,user}/{model,repository,service,huma_handler}.go`. Cross-cutting infra in `internal/platform/{config,database,cache,auth,api}` and `internal/reqctx/`.
  ```
  backend/
  ├── cmd/api/                 # main.go: viper → DB → cache → domains → router → http.Server
  ├── internal/
  │   ├── middleware/          # chi middlewares (RequestID, RealIP, Recoverer, Prometheus, Logging, CORS, RateLimit)
  │   ├── movie/               # Movie domain — model, repository, service, huma_handler, *tests
  │   ├── user/                # User domain — model, repository, service, huma_handler, *tests
  │   ├── platform/            # config, database, cache, auth, api
  │   └── reqctx/              # Typed ctx values (auth claims, request id)
  ├── migrations/              # golang-migrate, applied on boot
  └── queries/                 # sqlc input (.sql)
  ```

## 2. Validation & Error Handling

- [x] **Struct-based validation** — `go-playground/validator` driven by struct tags on huma input/output types.
- [x] **Standardized JSON errors** — uniform envelope across handlers; central mapper in `internal/platform/api`.
- [x] **Domain error sentinels** — `movie.ErrNotFound`, `user.ErrInvalidCredentials`, `auth.ErrInvalidToken`, `auth.ErrExpiredToken`. Handlers translate to HTTP status; never string-compare in tests or call sites.
- [x] **Internal error messages never echo to clients** — `api.ClassifyAndLog(ctx, err, safeDetail)` logs the wrapped error with the request ID at `Warn` and returns the static `safeDetail`. Handlers and auth middleware call it instead of writing `err.Error()` into the response `details` field, so SQL fragments, bcrypt strings, and JWT parser errors stay off the wire.
- [x] **Semantic API Error Translators** — `internal/platform/pgerr.Translate` / `Map` reads `pgconn.PgError.Code` + `ConstraintName` and returns the matching domain sentinel (`user.ErrUsernameTaken` for `users_username_key`, `user.ErrEmailTaken` for `users_email_key`, `user.ErrRefreshTokenCollision` for `idx_refresh_tokens_token_hash`). `internal/platform/api.MapError(ctx, err, safeDetail)` is the single handler-layer funnel — every handler shrinks to one line on the error path, and unknown errors reuse `ClassifyAndLog` so internal error text never reaches the wire. `ErrUserAlreadyExists` was split into `ErrUsernameTaken` + `ErrEmailTaken` (status 409 stays, messages change); `ErrRefreshTokenCollision` maps to 500 (sha256 collisions are ~10⁻³⁸ per row).

## 3. Security & Authentication

- [x] **JWT auth** — `auth/jwt.go` issues `Bearer` tokens with 24h expiry; `huma.Adapter` validates the header and stores claims on `reqctx`.
- [x] **User management** — `users` table with bcrypt-hashed passwords.
- [x] **Auth middleware** — write/delete routes require a valid token; read routes are public.
- [x] **Rate limiting** — token bucket middleware (`middleware/ratelimit.go`).
- [x] **CORS Hardening** — `middleware.NewCORS` reads `CORS_ALLOWED_ORIGINS` (default `*`); set a comma-separated origin list in production. Wildcard and explicit-allowlist modes are both supported and unit-tested.
- [x] **JWT Secret via Viper Config** — `auth.SetSecret(cfg.JWTSecret)` is called once in `main.go`; `auth/jwt.go` no longer reads `os.Getenv`. (Superseded by constructor injection — see below.)
- [x] **Constructor-injected JWT signing** — `auth.TokenService` interface + `auth.NewTokenService(secret []byte)` constructor hold the signing key in a private field. `main.go` builds one and threads it through the user service and auth middleware; the package-level `secret` / `SetSecret` are gone.
- [x] **Enforce non-default JWT secret at startup** — `config.Load()` and `auth.NewTokenService` both refuse the built-in default placeholder and any key shorter than `auth.MinSecretBytes` (32 bytes). Misconfiguration fails closed before the HTTP server starts.
- [x] **JWT Refresh Tokens** — opaque `refresh_tokens` table (`BIGSERIAL id`, `BYTEA token_hash` from `sha256`, `BIGINT family_id` self-referencing the original row, `replaced_by_id` self-FK). `crypto/rand` 32-byte tokens, base64url-encoded. Atomic rotation via a two-CTE statement; reuse of a revoked token revokes the entire family. `JWT_ACCESS_TTL` / `JWT_REFRESH_TTL` (defaults 15m / 168h). `POST /api/refresh` issues a new pair; `POST /api/logout` (auth-required) revokes the supplied token's family. `WWW-Authenticate: Bearer error="invalid_token", error_description="expired"` on the access-expired path so the frontend can reactively refresh without string-comparing error messages.

## 4. Database Layer

- [x] **Type-safe SQL** — `sqlc` over `pgx/v5` + `pgxpool`. SQL lives in `backend/queries/*.sql`; generated code in `internal/{movie,user}/db/` is regenerated via `make sqlc` and verified in CI via `make sqlc-diff`. See `backend/SQLC.md`.
- [x] **Transaction support** — pooled connections; complex flows use explicit `pgx.Tx` boundaries.
- [x] **Connection pooling** — pgxpool settings via viper (`MaxConns`, `MinConns`, `MaxConnLifetime`, `MaxConnIdleTime`).
- [x] **Index optimization** — `migrations/000006_add_movies_indexes.sql`, `000007_add_movies_description_trgm_index.sql` (pg_trgm for `ILIKE` search).
- [x] **Cache-aside** — Redis for `GET /api/movies`. Keys: `movies:q={query}:p={page}:s={size}`. Mutations call `DeletePrefix("movies:")` (SCAN + UNLINK, non-blocking). Cache is best-effort: every call swallows errors with `log.Warn` and never fails the request.

## 5. Observability & Documentation

- [x] **OpenAPI 3.1** — generated from huma struct tags on each operation's Input/Output. UI at `/api/swagger` (Stoplight Elements); raw spec at `/api/swagger/doc.json` (and `doc.yaml`). Also committed as `api/openapi.json`, regenerated database-free via `cd backend && make openapi` and drift-checked in CI via `make openapi-diff`. See `backend/HUMA.md`.
- [x] **Prometheus metrics** — `/metrics` endpoint + `prometheus` middleware (request count, latency, status, in-flight).
- [x] **Contextual logging** — `context` threaded through every layer; request ID propagated from the `RequestID` middleware through `Logging` → services → repositories.
- [x] **Distributed Tracing (OTel)** — SDK initialised in `cmd/api/main.go` via `internal/platform/observability`. HTTP entry traced by `middleware.Tracing` (chi-native `otelhttp` with chi-route-template span names); service-layer spans in `movie.Service` and `user.Service`; pgx pool traced via internal `pgx.QueryTracer` (`observability/pgx.go`). Exporter: OTLP HTTP → Jaeger all-in-one (compose `jaeger` service, UI on `:16686`). Default-off (`OTEL_ENABLED=false`) — every `tracer.Start` is a no-op and pgx skips its tracer callback when the global provider is noop. Sampling: `ParentBased(AlwaysOn)`; SDK honours `OTEL_TRACES_SAMPLER`. Scope intentionally narrow (HTTP + service + pgx) — repository and cache spans deliberately out of scope (pgx covers the SQL boundary, cache is best-effort noise). `trace_id` and `span_id` are added to the `Logging` middleware's per-request zerolog line whenever a valid span context is on the ctx.
  - **Frontend integration.** The W3C TraceContext + Baggage composite propagator installed on `otel` means inbound `traceparent` headers from upstream calls (the SPA's axios interceptor) are extracted automatically and the backend server span becomes a child span. `TestTracing_PropagatesTraceparentFromUpstream` (`backend/internal/middleware/tracing_test.go`) covers this path using `tracetest.NewSpanRecorder()`.
  - **Cache metrics.** `cache_operations_total{op,result}` in `internal/platform/cache/cache.go` records `hit`/`miss`/`error` per Get. `TestCacheMetrics` (`backend/internal/platform/cache/cache_test.go`) gathers via `prometheus.DefaultGatherer` and asserts both label permutations.

## 6. Configuration & Environment

- [x] **Viper** — multi-source: defaults, env vars, `.env` auto-loaded.
- [x] **Graceful shutdown** — `SIGTERM`/`SIGINT` triggers `http.Server.Shutdown` with a deadline; DB and cache teardown afterwards.
- [ ] **Container Version Conflict Guardrail** — developer-facing script to detect (and warn on, or auto-prune) Postgres major-version volume upgrades.

## 7. Quality Assurance

- [x] **Unit tests** — handlers and services use `pgxmock`; cache uses `miniredis`. No Docker required.
- [x] **Integration tests** — `testcontainers-go` boots a real Postgres. Self-skip via `t.Skip()` when `dbURL == ""`. Never reintroduce an early `os.Exit(0)` in `TestMain` — it silently skips unit tests.
- [x] **golangci-lint** — strict pipeline (revive, gosec, staticcheck, …) in CI.
- [x] **CI service container** — `postgres:17-alpine` in `backend-test` and `e2e-test` jobs so `testcontainers-go` tests run.
- [x] **User huma_handler test coverage** — `user/huma_handler_test.go` covers register/login happy paths, validation rejections (missing/short username, invalid email, short password), duplicate 409, unknown user 401, bad password 401, and 500 paths.
