# Nyx Roadmap

Single source of truth for "what's done / what's next" across the stack. Tick an item when the work lands. Deeper context for each area lives in `FUTURE_BACKEND.md` and `FUTURE_FRONTEND.md`.

## 1. Reliability & Observability

- [x] Structured logging (`rs/zerolog`, JSON output, request IDs).
- [x] Prometheus `/metrics` endpoint + `prometheus` middleware.
- [x] Cache hit/miss Prometheus counter — `cache_operations_total{op,result}` in `internal/platform/cache/cache.go`. Reuses the `prometheus.DefaultGatherer` text format from the request middleware.
- [x] Graceful shutdown (`SIGTERM`/`SIGINT` + `context`).
- [x] Health checks — `/api/health` includes DB connectivity.
- [x] Distributed Tracing (OpenTelemetry) — SDK initialised in `cmd/api/main.go` via `internal/platform/observability`. HTTP entry traced by `middleware.Tracing` (`otelhttp` + chi route-template span names); service-layer spans in `movie.Service` / `user.Service`; pgx pool traced via internal `pgx.QueryTracer`. Exporter: OTLP/HTTP → Jaeger all-in-one (compose `jaeger` service, UI on `:16686`). Default-off (`OTEL_ENABLED=false`) — every `tracer.Start` is a no-op and pgx skips its tracer callback when the global provider is noop. W3C TraceContext + Baggage composite propagator on the global; the existing `traceparent` extract path is covered by `TestTracing_PropagatesTraceparentFromUpstream`.
- [x] Frontend → Jaeger end-to-end — `@opentelemetry/sdk-trace-web` initialised in `frontend/src/services/telemetry.ts`, exports OTLP/HTTP via the SPA container's nginx `location /otlp/` (same-origin proxy → Jaeger, bypasses Jaeger's missing CORS without an OTel Collector). axios request interceptor calls `injectTraceparent()` so backend spans are children of browser-initiated root spans. `service.name=nyx-frontend`; traceparent flows both directions via W3C TraceContext.
- [x] Frontend structured logging — `frontend/src/services/logger.ts` (JSON-shaped `console.*`, `trace_id`/`span_id` pulled from the active span). Global `window.error` + `unhandledrejection` handlers in `main.tsx`.

## 2. API Maturity & Security

- [x] OpenAPI 3.1 — generated at runtime from huma struct tags; UI at `/api/swagger` (Stoplight Elements). See `backend/HUMA.md`.
- [x] JWT auth on write endpoints (`POST/PUT/DELETE /api/movies`).
- [x] Clean architecture — `internal/{movie,user}/` with `model / repository / service / huma_handler`.
- [x] Rate limiting middleware (token bucket).
- [x] Middleware chain on chi v5 — RequestID, RealIP, Recoverer, Prometheus, Logging, CORS, RateLimit.
- [x] Standardized JSON error envelopes.
- [x] Domain error sentinels — `movie.ErrNotFound`, `user.ErrInvalidCredentials`, `auth.ErrInvalidToken`, `auth.ErrExpiredToken`. Handlers translate to HTTP status; never string-compare.
- [x] Semantic API Error Translators — `internal/platform/pgerr` translates `pgconn.PgError` codes + constraint names to domain sentinels (`user.ErrUsernameTaken`, `user.ErrEmailTaken`, `user.ErrRefreshTokenCollision`); `api.MapError` funnels every handler error through one call.
- [x] CORS Hardening — `cors.go` reads `CORS_ALLOWED_ORIGINS` (default empty — deny-by-default). Set a comma-separated origin list in production. The cross-origin custom-header preflight (Bearer is not a CORS-safelisted header) is the natural CSRF defence, so `Access-Control-Allow-Credentials` is not needed and is intentionally never set.
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
- [x] Persist Auth Token Securely — Bearer tokens (RFC 6750). `/api/login`, `/api/register`, `/api/refresh` return `{access_token, refresh_token, token_type: "Bearer", expires_at, user}` in the JSON body; every protected endpoint expects `Authorization: Bearer <access_token>`. `/api/logout` reads the refresh token from the body and revokes only that row. The SPA stores tokens in memory + `sessionStorage` (per-tab) via a module-level `tokenStore`; the Zustand `authStore` persist carries only `user` (tokens must NEVER land in localStorage — XSS-exfiltration target). Same wire contract works for native clients (iOS / Android via Keychain / Keystore; Windows / desktop / mobile games via OS credential store; console SDKs via platform OAuth → `POST /api/auth/exchange`). The custom Authorization header forces a cross-origin CORS preflight; that's the natural CSRF defence.
- [x] Split-origin deployment — SPA at `app.nyx.com` and API at `api.nyx.com`. CORS allowlist on the backend gates cross-origin XHR; nginx CSP widens `connect-src` to include the API origin via `__API_ORIGIN__` build-time substitution; k8s ingress split into two host-routed Ingresses with cert-manager + TLS.
- [x] Mobile client token storage — Keychain (iOS, `kSecClassGenericPassword` + `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`) / Keystore-backed EncryptedFile (Android). The mobile client itself is out of scope (separate project); this lands in tandem.
- [x] Game client token storage — Windows Credential Manager / DPAPI on PC; Keychain / Keystore on mobile games (same as native apps). Game client implementations are out of scope (separate projects).
- [ ] Console platform exchange — `POST /api/auth/exchange` to bridge PSN / Xbox Live / Nintendo / Steam OAuth into the standard Bearer pair. Follow-up; not in the current PR.
- [ ] Multi-tab concurrent refresh race — known issue: if two tabs simultaneously hit `/api/refresh` with the same `refresh_token`, the atomic CTE marks one as "already rotated" and that tab logs out. UX-harsh but security-correct. Mitigation is follow-up (lock-window on rotation, or BroadcastChannel-based token sync).
- [x] Traceparent propagation — axios request interceptor calls `injectTraceparent()` so backend spans are children of browser-initiated root spans (`nyx-frontend` service). Verified by the new `api.test.ts` traceparent test.
- [x] Token Refresh — single-flight refresh-on-401 in `api.ts`, driven by the `WWW-Authenticate: Bearer error="invalid_token", error_description="expired"` challenge from the auth middleware. The refresh POST sends `{refresh_token}` in the body (no cookies); on success, the `tokenStore` swaps in the fresh `access_token` / `refresh_token` pair and the original request is replayed with the new Bearer header. The auth store carries only `user`.

## 5. Developer Experience & CI/CD

- [x] GitHub Actions — jobs: `backend-test` (lint + sqlc-diff + tests), `frontend-test` (lint + test + build), `e2e-test` (depends on both).
- [x] Backend CI service container — `postgres:17-alpine` in `backend-test` and `e2e-test` jobs so `testcontainers-go` tests run.
- [x] E2E — Playwright in `frontend/tests/e2e`.
- [x] Backend lint — `golangci-lint` in CI.
- [x] Frontend lint — `eslint` + `husky` pre-commit + `lint-staged`.
- [x] User Domain Handler Test Coverage — `user/huma_handler_test.go` covers register/login happy paths, validation rejections (missing/short username, invalid email, short password), duplicate 409, unknown user 401, bad password 401, and 500 paths.
- [x] Kubernetes manifests — `Deployment`, `Service`, `Ingress`, `Secrets`, `StatefulSet` (Postgres), `Deployment` (Redis) in `k8s/`.
- [x] Viper config — multi-source (env + `.env` + defaults).
- [x] Commitlint hook — `.husky/commit-msg` runs `commitlint --edit` with `@commitlint/config-conventional`; `commitlint.config.js` pins the scope enum (`backend`, `frontend`, `auth`, `infra`, `security`, `ci`, `docs`, `claude`, `env`, `observability`, `data`, `repo`) and ignores merge/revert commits.
- [x] Commitizen interactive authoring — `git cz` alias wired by root `npm install` (postinstall). Drives `cz-customizable` against `.cz-config.cjs`, whose scope list mirrors commitlint's so messages pass the hook first try. Plain `git commit -m` is also accepted.
- [x] Semantic-release per-package versioning — `release.config.js` + `@semantic-release/monorepo` compute `backend@X.Y.Z` / `frontend@X.Y.Z` from commit paths, write per-package `CHANGELOG.md`, stamp the new version into `backend/internal/platform/observability/tracing.go` (`ServiceVersion`) + `frontend/package.json` via `scripts/bump-version.sh`, and push tags + release commits. CI `release` job gated on `e2e-test` and only on push to `main` / `mvp`. No npm publish — both packages stay `private: true`.

## 6. AI / LLM

- [x] Streaming Chat (OpenAI / vLLM) — `POST /api/chat` returns `text/event-stream` (event: delta / done / error + data: [DONE]). Provider-agnostic via `internal/llm.Provider`; `internal/llm/openai.Client` works against both OpenAI and vLLM because vLLM exposes the identical `/v1/chat/completions` wire contract. Frontend consumes via raw `fetch` + `ReadableStream` (axios can't stream) with the auth-gated `MessageSquare` button in `App.tsx`. Opt-in via `LLM_ENABLED=true` + `LLM_BASE_URL` / `LLM_API_KEY` / `LLM_MODEL`; defaults to off so deployments without an LLM pay nothing. System prompt is server-only; model is server-only; client request shape is `{messages:[{role,content}]}` with optional `model` (currently ignored). Per-user in-memory rate limit (5 streams/min, burst 3). See `FUTURE_BACKEND.md` §8 + §9 and `FUTURE_FRONTEND.md` §10.
- [ ] Chat history persistence — currently stateless (client sends full history each request). When conversations outgrow `LLM_MAX_HISTORY_MESSAGES` or cross-device history / analytics / GDPR-delete become product needs, add `chat_sessions` + `chat_messages` tables with soft-delete + retention cron.
- [ ] Chat persona allowlist — let authenticated users pick from an operator-curated model allowlist (cost-controlled).
- [ ] Streaming tool calls / JSON mode — extend the provider interface once we have a feature that needs structured output beyond free text.

---

*Nyx: Minimalist by design, production-credible by choice.*
