# Security

## Reporting vulnerabilities

Use GitHub Security Advisories (private disclosure) on this repository. Expect acknowledgement within 72 hours. Please do not file public issues for security bugs.

## Audits

### 2026-08-26 — Full-sweep security review

**Scope.** Entire repo at commit `c9674d4`: backend (Go), frontend (React), Dockerfiles, compose files, CI, dependency manifests, `.env*` / `.gitignore` / `.dockerignore` / `.husky`.

**Method.** 5 parallel read-only sweeps covering auth/JWT, input/error/SQL, CORS/headers/secrets, frontend/XSS/tokens, dependencies/docker.

**Verdict.** No critical findings. 12 high, 12 medium, 14 low. Cookie auth landing (`c9674d4`) is structurally sound; principal weaknesses are CORS default, login timing oracle, JWT claim hardening, missing security headers, and dev-infra exposed ports.

## Findings

Status legend: ☐ open · ☑ fixed · ◌ wontfix (with rationale).

### High

| # | Status | Area | File | Issue | Fix |
|---|---|---|---|---|---|
| H1 | ☑ | CORS | `backend/internal/middleware/cors.go:33-69`, `backend/internal/platform/config/config.go:103` | Default `CORSAllowedOrigins=*`; combined with the `Authorization` header echo, permits third-party origins to issue credentialed cross-site requests | Default empty; require explicit allowlist in prod | Fixed in this batch — default now `""`; `.env.example` documents dev/staging/prod examples; new tests cover empty-allowlist denial |
| H2 | ☐ | Auth | `backend/internal/user/service.go:152-166` | Login timing oracle — bcrypt runs only for existing usernames | Run a dummy `bcrypt.CompareHashAndPassword` against a known-bad hash on the `ErrUserNotFound` branch |
| H3 | ☐ | JWT | `backend/internal/platform/auth/jwt.go:94-97` | Keyfunc returns the secret without checking `token.Method`; no `jwt.WithValidMethods` | Add `WithValidMethods([]string{"HS256"})` and assert `*jwt.SigningMethodHMAC` in the keyfunc |
| H4 | ☐ | JWT | `backend/internal/platform/auth/jwt.go:79-119` | No `iss` / `aud` / `nbf` claims minted or verified | Stamp and verify with `WithIssuer` / `WithAudience` |
| H5 | ☐ | Auth | `backend/internal/user/repository.go:36-38` + `backend/internal/platform/api/map.go:46-55` | Register returns distinct messages for `ErrUsernameTaken` vs `ErrEmailTaken` (asymmetric with login's collapsed `ErrInvalidCredentials`) | Collapse to a single generic `ErrUserAlreadyExists` |
| H6 | ☐ | Frontend / Infra | `frontend/nginx.conf` | No CSP / X-Frame-Options / Referrer-Policy / Permissions-Policy / X-Content-Type-Options / HSTS | Add the headers; strict CSP `default-src 'self'` |
| H7 | ☐ | Frontend | `frontend/src/services/api.ts:136-141`, `frontend/src/components/app/ToastContainer.tsx` | Server `ErrorResponse.error` rendered verbatim as toast | Cap length, strip control chars, show generic message + request id; log full payload |
| H8 | ☐ | Frontend | `frontend/src/services/api.ts:104-117` | Refresh only fires when 401 carries `WWW-Authenticate: error_description="expired"`; non-expired 401s force-logout | Refresh on any 401 (unless `_retried` / `skipAuthRefresh`) |
| H9 | ☐ | Docker | `backend/Dockerfile:21` | `FROM alpine:latest` — floating tag | Pin to `alpine:3.20@sha256:<digest>` |
| H10 | ☐ | Docker | `frontend/Dockerfile:8` | `npm install` instead of `npm ci` | Use `npm ci --no-audit --no-fund` |
| H11 | ☐ | Docker | `backend/Dockerfile:18` | `go build` missing `-trimpath -ldflags="-s -w -buildid="` | Add the flags |
| H12 | ☐ | Docker | `docker-compose.yml:13-14, 27` | DB `5433` and Redis `6379` bound `0.0.0.0` with default `postgres/postgres` | Bind to `127.0.0.1` |

### Medium

| # | Status | Area | File | Issue | Fix |
|---|---|---|---|---|---|
| M1 | ☐ | Auth | `backend/internal/user/service.go:265-292` | Logout revokes the entire refresh-token family (kills all devices) | Track `session_id` per row; revoke only the supplied session |
| M2 | ☐ | Auth | `backend/migrations/000008_create_refresh_tokens.up.sql` + `user/service.go` | No background cleanup of expired / revoked refresh rows; no per-user cap | Background goroutine pruning; cap active families per user |
| M3 | ☐ | Auth | `backend/internal/middleware/ratelimit.go:78-108` | Global per-IP limit (10 rps / burst 20) covers login equally | Per-route stricter limit on `/api/login`, `/api/register`; per-username lockout |
| M4 | ☐ | JWT | `backend/internal/platform/auth/jwt.go:63-65` | Startup-failure error string includes `len=%d` (secret-length leak) | Drop the format spec |
| M5 | ☐ | Auth | `backend/internal/platform/config/config.go:78` | Default placeholder is 41 bytes; would pass any future check that drops to 32 | Refuse the literal; require operator-supplied |
| M6 | ☐ | Input | `backend/internal/movie/huma_handler.go:134`, `repository.go:101-104` | `Q` query has no length cap; multi-MB terms reach Postgres | Add `maxLength:"200"` |
| M7 | ☐ | Input | `backend/internal/user/model.go:25-28` | `LoginRequest` has no `minLength` / `maxLength` on password | Cap server-side before bcrypt (e.g. 128 bytes) |
| M8 | ☐ | Frontend | `frontend/src/store/authStore.ts:53,86` | `loginAxios.post('/logout')` has no `baseURL`; dev hits `localhost:5173/logout` (404) | Use the wrapped `api` instance or set `baseURL: '/api'` |
| M9 | ☐ | Docker | `docker-compose.prod.yml:62` + `.env.example` | `sslmode=disable` in prod; `DB_URL` interpolated from `POSTGRES_PASSWORD` shell expansion (visible in `docker inspect`) | Set `DB_URL` directly in secrets; `sslmode=require` |
| M10 | ☐ | Docker | (no `.dockerignore`) | `COPY . .` ships tests, docs, `.env`, `coverage.out` into build image | Add `.dockerignore` per service |
| M11 | ☐ | Docker | both Dockerfiles | No `HEALTHCHECK` directive | Add `HEALTHCHECK` per service |
| M12 | ☐ | Docker | both compose files | No `cap_drop` / `read_only` / `security_opt` | Add hardening flags |

### Low / informational

| # | Status | Area | File | Issue | Fix |
|---|---|---|---|---|---|
| L1 | ☐ | Auth | `backend/internal/middleware/auth.go:88-101` | `Authorization: Bearer` fallback still accepted | Remove after cookie rollout completes |
| L2 | ☐ | Auth | `backend/internal/user/refresh.go:42-45` | Refresh tokens stored as raw SHA-256 | Consider HMAC pepper for defense-in-depth |
| L3 | ☐ | JWT | `backend/internal/platform/auth/jwt.go:69-77` | Secret retained in heap; no finalizer to zero | Finalizer for compliance (informational) |
| L4 | ☐ | Logging | `backend/internal/middleware/logging.go:32-55` | Raw query string logged (zerolog escapes JSON; PII scope only) | Document retention scope |
| L5 | ☐ | Frontend | `frontend/vite.config.ts:13-48` + `frontend/Dockerfile:20` | Source maps emitted and copied to prod image | `build.sourcemap = false` |
| L6 | ☐ | Frontend | `frontend/src/main.tsx:35-46` | No React error boundary | Wrap `<App>` with a fallback boundary |
| L7 | ☐ | Frontend | `frontend/src/store/authStore.ts:37-127` | Persisted `user` can desync on token expiry | Server `GET /api/me` round-trip at boot |
| L8 | ☐ | Frontend | `frontend/src/features/movies/components/MovieList.tsx:135` | Search term echoed in empty-state without length cap or encoding | `maxLength={200}` on input |
| L9 | ☐ | Frontend | `frontend/src/App.tsx:81-90,242-249` | Delete dialog fire-twice if double-clicked rapidly | Disable button while `deleteMovie.isPending` |
| L10 | ☐ | Process | `.github/workflows/ci.yml`, `frontend/.husky/pre-commit` | No secret scanner in CI or pre-commit | Add `gitleaks-action` |
| L11 | ☐ | Process | `.github/workflows/ci.yml` | Actions pinned to majors (`@v4`, `@v5`), not commit SHAs | Pin to SHAs |
| L12 | ☐ | Repo | `k8s/secrets.yaml` | Gitignored but on disk with base64 placeholder; confirm it was never deployed | If deployed, rotate. Use `kubectl create secret` from `openssl rand` |
| L13 | ☐ | Repo | `frontend/package.json:36` | `lucide-react@^1.7.0` is an unusual version specifier | Verify on npm |
| L14 | ☐ | Process | repo | No `SECURITY.md` (this file) | Resolved by this commit |

## Verified safe

- **SQL injection** — all queries via sqlc; `ILIKE` wildcards are Go-side concat then bound as a single parameter. No `fmt.Sprintf` into `pgx.Query`. Migration files are pure DDL.
- **Error leakage to wire** — `api.MapError` + `ClassifyAndLog` never serialize `err.Error()` into `details`. Verified at every call site (`movie/huma_handler.go:210-232`, `user/huma_handler.go:152-203`).
- **Token storage** — raw tokens never reach the DB (SHA-256 hash) or logs (grep-confirmed). Refresh tokens are 32 random bytes.
- **Cookie flags** — `__Host-` prefix correctly enforced only when `Secure && Domain==""`. `Secure=false + SameSite=None` cross-validated at config load.
- **JWT secret** — refused at startup if default literal or `< 32` bytes; `auth.NewTokenService` repeats the check independently.
- **Bcrypt** — `DefaultCost` (10); 1 MiB body cap prevents pre-auth DoS.
- **Refresh-token rotation** — atomic CTE marks `revoked_at` + `replaced_by_id` in one statement, no TOCTOU race.
- **XSS surface** — zero `dangerouslySetInnerHTML` / `innerHTML` / `eval` / `javascript:` URLs / dynamic `href` in `frontend/src/**` (grep-confirmed). No DOMPurify needed today; revisit if MD rendering is added.
- **Single-flight refresh** — correctly handled via shared `refreshing` promise + `_retried` flag (`api.ts:26-127`).
- **Zustand migration** — `migrate` strips every token-shaped field from pre-cookie payloads (fail-safe).
- **State-changing methods** — all POST/PUT/DELETE; no state-changing GETs.
- **Dependencies** — no actively-exploited CVEs at current versions (`golang-jwt v5.3.1`, `chi v5.3.1`, `pgx v5.10.0`, `golang.org/x/crypto v0.54.0`, `axios 1.13.6`, React 19.2, Vite 8, Tailwind 4.2).
- **Repo hygiene** — `.env` gitignored, no hardcoded secrets, multi-stage Dockerfiles, non-root USER, prod compose keeps DB/Redis/Jaeger on an internal network.
- **Log injection** — zerolog JSON-escapes user input. Response splitting via `X-Request-ID` blocked by `net/http` header validation.
- **Authorization** — no object-level gaps (no user-CRUD or movie-owner endpoints in scope). Movie create/update/delete is auth-required but not admin-restricted (by design).

## Suggested remediation order

1. **Backend hardening** — H1–H5, M1–M7, L1–L4
2. **Frontend hardening** — H6–H8, M8, L5–L9
3. **Docker + compose** — H9–H12, M9–M12, L12
4. **Process** — L10–L11, L13

Tick a row by changing ☐ → ☑ and adding the fix commit SHA in the issue column.
