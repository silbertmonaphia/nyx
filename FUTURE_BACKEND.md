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

- [x] **JWT auth** — `auth/jwt.go` issues `Bearer` tokens (HS256, 15m default via `JWT_ACCESS_TTL`); `middleware.NewAuth` / `NewHumaAuth` validate the `Authorization` header and store claims on `reqctx`. Algorithm pinning + issuer + audience + type claim guards (`type: "access"`) defend against alg-confusion and refresh-token-as-access attacks.
- [x] **User management** — `users` table with bcrypt-hashed passwords.
- [x] **Auth middleware** — write/delete routes require a valid Bearer token; read routes are public.
- [x] **Rate limiting** — token bucket middleware (`middleware/ratelimit.go`).
- [x] **CORS Hardening** — `middleware.NewCORS` reads `CORS_ALLOWED_ORIGINS` (default empty — deny-by-default); set a comma-separated origin list in production. Wildcard and explicit-allowlist modes are both supported and unit-tested. `Access-Control-Allow-Credentials` is intentionally never set: Bearer is a plain Authorization header (custom → preflight → backend allowlist = CSRF defence), so ACAC would only matter if cookies returned to the picture.
- [x] **Bearer-only auth transport** — `/api/login`, `/api/register`, `/api/refresh` return `{access_token, refresh_token, token_type: "Bearer", expires_at, user}` in the body. `/api/refresh` and `/api/logout` read the refresh token from the request body. The legacy cookie / `CookieConfig` plumbing is gone (`backend/internal/platform/auth/cookies.go` deleted; `COOKIE_*` viper bindings removed). Same wire contract works for SPA, iOS / Android native apps, Unity / Unreal game clients, and (after `POST /api/auth/exchange`) PSN / Xbox / Nintendo / Steam console clients.
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
- [x] **Per-package release automation** — `semantic-release` (monorepo plugin) computes `backend@X.Y.Z` / `frontend@X.Y.Z` independently from commit paths; the CI `release` job runs on push to `main` / `mvp`, writes `backend/CHANGELOG.md` + `frontend/CHANGELOG.md`, and `scripts/bump-version.sh` stamps the new version into `observability.ServiceVersion` (also wired into the Docker build via `ARG VERSION`) + `frontend/package.json`. Both packages stay `private: true` — no npm publish. Commit messages are gated by commitlint (scope enum in `commitlint.config.js`) and authored interactively via `git cz` (commitizen + cz-customizable).

## 8. LLM Streaming Chat

- [x] **Provider abstraction** — `internal/llm.Provider` is the seam future features (summaries, semantic search) consume. Callback-based streaming (`Chat(ctx, req, onDelta) (*ChatUsage, error)`) keeps the wire format in the handler. Errors funnel through `internal/llm.ErrProviderUnavailable` / `ErrRateLimited` / `ErrInvalidInput` / `ErrContextCanceled`, registered with `api.RegisterSentinel` for the standard MapError path. See §9 for the OpenAI-compatible implementation.
- [x] **OpenAI-compatible client** — `internal/llm/openai.Client` uses `github.com/sashabaranov/go-openai` with `BaseURL` set from `LLM_BASE_URL`. The same client works against OpenAI's hosted endpoint (`https://api.openai.com/v1`) and any self-hosted vLLM server (`http://vllm.internal:8000/v1`) because vLLM exposes the identical `/v1/chat/completions` SSE wire contract. Streaming: `Stream=true` + `StreamOptions.IncludeUsage=true` so the trailing usage chunk arrives on the same stream. Errors mapped via `errors.As` on `*openai.APIError` (429 → rate-limited; ≥500 → provider-unavailable) and `*openai.RequestError` (network → provider-unavailable); context cancellation → `ErrContextCanceled`. Null Delta.Content frames (OpenAI's role-change frames) are skipped.
- [x] **`POST /api/chat` SSE endpoint** — `internal/chat.Handler.stream` writes `text/event-stream; charset=utf-8` with `X-Accel-Buffering: no` (defeats nginx ingress buffering), `Cache-Control: no-cache`, `Connection: keep-alive`. Wire format: `event: delta\ndata: {"delta":"..."}\n\n` per content chunk, `event: done\ndata: {"usage":{...}}\n\n` on completion, `data: [DONE]\n\n` terminator. Mid-stream upstream failure emits `event: error\ndata: {"error":"<safe static>","request_id":"..."}\n\n` then `[DONE]` — the HTTP status is already 200 because the SSE headers flushed before the error.
- [x] **Route mounted on chi, not huma** — huma v2 has no first-class SSE (response of unknown length, JSON schema can't capture the streaming shape). `chat.RegisterChatRoute(router, handler, tokens, limiter)` mounts the route directly on chi after the huma adapter wraps the router, so the route still runs through the standard middleware chain (RequestID → Recoverer → Prometheus → Logging → CORS → RateLimit → maxBodyBytes → NewAuth → UserRateLimiter). See `backend/HUMA.md` for the rationale.
- [x] **Server-only system prompt** — `LLM_SYSTEM_PROMPT` is prepended by `chat.Service.Chat` (defaults to a hard-coded movie-catalog prompt when empty). Clients cannot override it: any `role: "system"` message is rejected at `Service.Validate` with `ErrInvalidInput` → 400 envelope before any SSE bytes hit the wire.
- [x] **Per-user in-memory rate limiter** — `chat.NewUserRateLimiter(rate, burst)` is a token bucket keyed by `reqctx.UserIDFromContext`. Defaults from `cmd/api/main.go`: 5 streams/min, burst 3. Charged on stream-open (a single stream holds one slot regardless of duration). Background goroutine GCs buckets idle for over an hour on a 1-minute tick; stopped on shutdown. In-memory only — a horizontally-scaled deployment multiplies the cap by replica count, which is the right behaviour for a soft cap.
- [x] **Fail-closed config** — `LLM_ENABLED=true` requires non-empty `LLM_BASE_URL` / `LLM_API_KEY` / `LLM_MODEL`; `LLM_API_KEY` is checked against a placeholder blocklist (`changeme`, `your-key`, `sk-xxx`, `xxx`, `test-key`) so a stray `.env` with a literal placeholder refuses to start. Durations (`LLM_TIMEOUT`, `LLM_MAX_STREAM_DURATION`) parsed at startup.
- [x] **Safe-error-detail contract preserved** — handlers never copy `err.Error()` into the wire. Validation errors → 400 JSON envelope before SSE headers. Mid-stream errors → trailing `event: error` with a static safe message + request ID; the underlying error is logged at Warn via `api.ClassifyAndLog` for operator correlation.

## 9. Provider Abstraction (seam for future features)

- [x] **Interface** — `internal/llm.Provider.Chat(ctx, req, onDelta) (*ChatUsage, error)`. `onDelta` is invoked once per non-empty content chunk with `(delta, nil)`, and once on the trailing usage chunk with `("", *ChatUsage)`. Returning a non-nil error from `onDelta` aborts the read and surfaces to the caller — used by the chat handler to propagate client-disconnect signals from the SSE writer's `Flush()` error.
- [x] **Reusable across features** — the abstraction was designed to support the chat feature, but the same shape (validated messages, provider call, usage capture) extends naturally to: per-movie summary generation, semantic re-ranking of search results, "explain why this movie matched" tool calls. Each future feature adds its own domain package under `internal/{feature}/` and reuses `internal/llm/openai.Client`.
- [ ] **Provider allowlist (future)** — when an operator wants to host multiple upstream LLMs (different cost / latency profiles), introduce `internal/llm/multi/router.go` that picks among configured providers based on per-request hints.
