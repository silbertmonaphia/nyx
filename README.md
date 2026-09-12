# Nyx

A minimalist media rating application — Go 1.26.1 API, React 19 SPA, PostgreSQL, optional Redis cache.

## Highlights

- **Clean-arch backend** — `chi v5` router + `huma v2` for declarative, OpenAPI 3.1-emitting HTTP handlers. SQL is type-safe via `sqlc` + `pgx/v5` + `pgxpool`.
- **Modern frontend** — React 19 + Vite 8 + TypeScript, Tailwind CSS v4, Radix UI primitives (shadcn-style), TanStack Query, Zustand, React Hook Form + Zod, axios.
- **Auth** — JWT access tokens (default 15m) paired with rotated refresh tokens (default 7d, opaque, sha256-hashed, family-level reuse detection). Auth endpoints (`/api/register`, `/api/login`, `/api/refresh`) return `{access_token, refresh_token, token_type: "Bearer", expires_at, user}` in the body; every protected endpoint expects `Authorization: Bearer <access_token>`. `/api/logout` + `/api/refresh` send `{refresh_token}` in the body. Bcrypt-hashed passwords. The SPA stores tokens in a module-level `tokenStore` (in-memory + `sessionStorage`); the Zustand `authStore` persist carries only `user`. Native clients (iOS / Android / Unity / Unreal / console SDKs) speak the identical wire contract — only the token storage differs. Production topology: SPA at `app.nyx.com`, API at `api.nyx.com` (separate Ingress hosts); Bearer is a custom header → CORS preflight = CSRF defence, no token flow needed.
- **Pagination** — `GET /api/movies` returns `{data, page, page_size, total, has_more}`. Default 20, max 100.
- **Cache-aside** — Redis opt-in for `GET /api/movies` (`REDIS_ENABLED=true`). Cache is best-effort; failures never fail the request.
- **Observability** — Distributed tracing via OpenTelemetry (browser → nginx → Jaeger, all spans stitched by W3C `traceparent`), `/metrics` (Prometheus: HTTP request count/latency, cache hit/miss), structured JSON logging via `zerolog`, graceful shutdown.
- **CI/CD** — GitHub Actions (lint + unit + integration + sqlc drift + e2e, plus a semantic-release job on push to `main` / `mvp`), `golangci-lint`, `husky` pre-commit + commit-msg hooks on the frontend. Commits are authored via `git cz` (commitizen + cz-customizable) and linted by `@commitlint/config-conventional`; semantic-release per-package versioning drives `backend@X.Y.Z` / `frontend@X.Y.Z` tags from Conventional Commit messages.
- **Deploy** — Docker Compose for dev/prod, manifests in `k8s/`.

## Repository Layout

```
nyx/
├── backend/              # Go API server
│   ├── cmd/api/          # Entry point (main.go)
│   ├── internal/         # cmd/api, middleware, movie/, user/, platform/, reqctx/
│   ├── migrations/       # golang-migrate, applied on every backend boot
│   ├── queries/          # sqlc input (.sql)
│   └── internal/{movie,user}/db/  # sqlc output (regenerated via `make sqlc`)
├── frontend/             # React SPA
│   └── src/
│       ├── features/     # Domain-driven modules (movies/, auth/)
│       ├── components/   # Shared UI primitives (Button, Dialog, Card, …)
│       ├── services/     # API client (axios)
│       ├── store/        # Zustand stores
│       ├── hooks/        # Custom React hooks
│       ├── types/        # TypeScript types
│       └── utils/        # Helpers (cn, etc.)
├── k8s/                  # Deployment, Service, Ingress, Secrets manifests
├── openspec/             # OpenSpec change/spec records
├── docker-compose.yml        # Dev stack
└── docker-compose.prod.yml   # Production stack
```

## Quick Start (Docker Compose)

```bash
# 1. Configure env
cp .env.example .env

# 2. Build the backend binary locally (Alpine images need a static binary)
(cd backend && CGO_ENABLED=0 go build -o main ./cmd/api)

# 3. Start the full stack
sudo docker compose up --build -d
```

- Frontend: <http://localhost:5173>
- API: <http://localhost:8080/api/movies>
- API docs (Stoplight Elements): <http://localhost:8080/api/swagger>
- PostgreSQL: `localhost:5433` (mapped from container `5432`)

```bash
# Database
PGPASSWORD=postgres psql -h localhost -p 5433 -U postgres -d nyx

# Redis (if REDIS_ENABLED=true)
sudo docker compose exec redis redis-cli KEYS 'movies:*'
```

## Local Development

### Backend

```bash
cd backend
go run ./cmd/api
# Requires DB_URL, JWT_SECRET in env (or .env at repo root)
# Add REDIS_ENABLED=true REDIS_URL=redis://localhost:6379 CACHE_TTL=5m for cache
```

### Frontend

```bash
cd frontend
npm install
npm run dev
# http://localhost:5173
```

### OpenTelemetry (optional)

Tracing is off by default — both backend (`OTEL_ENABLED`) and frontend (`VITE_OTEL_ENABLED`) gate the SDK behind a `"true"` flag, so a vanilla `npm run dev` carries zero observability overhead.

To turn it on in dev:

```bash
# 1. Start Jaeger (already part of `docker compose up`)
sudo docker compose up jaeger -d

# 2. Backend — env in repo-root .env or shell
export OTEL_ENABLED=true
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
cd backend && go run ./cmd/api

# 3. Frontend — Vite inlines these at build time
export VITE_OTEL_ENABLED=true
export VITE_OTEL_SERVICE_NAME=nyx-frontend
export JAEGER_HOST=localhost   # dev: nginx in container proxies via container DNS; in `npm run dev`, set to whatever Jaeger is reachable on
cd frontend && npm run dev

# 4. Open the Jaeger UI
open http://localhost:16686
# Service dropdown → "nyx-frontend". Click any trace and the tree shows:
#   HTTP POST /api/movies
#   ├── nyx.movie.Create (service span)
#   └── pgx.query (db.operation = INSERT)
```

In the bundled `docker compose` stack, `OTEL_ENABLED=true` and `VITE_OTEL_ENABLED=true` are defaults; the frontend container's nginx proxies `/otlp/` to the `jaeger` service, so the browser hits same-origin and Jaeger's missing CORS is a non-issue.

## Self-hosted LLM (vLLM, optional)

The `/api/chat` SSE endpoint streams from any OpenAI-compatible server. The default `openai` provider uses `sashabaranov/go-openai` and targets OpenAI's hosted endpoint or any compatible server via `LLM_BASE_URL`. A second provider — `vllm` — implements the same `llm.Provider` interface against a self-hosted [vLLM](https://docs.vllm.ai/) server with raw `net/http` + a hand-rolled SSE decoder and a per-dial DNS allowlist that the SDK path lacks. Selection happens once at startup via `LLM_PROVIDER={openai,vllm}` (default `openai`).

To run a self-hosted model in dev:

```bash
# Pull a model from HuggingFace (default: Qwen/Qwen2.5-3B-Instruct, Apache-2.0, ungated)
sudo docker compose --profile vllm up -d

# Watch the vLLM container finish model load (~30-90s on CPU, ~10-30s on GPU)
sudo docker compose logs -f vllm

# Use the chat panel in the SPA, or curl directly:
TOKEN=$(curl -s -X POST http://localhost:8080/api/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"...","password":"..."}' | jq -r .access_token)

curl -N -X POST http://localhost:8080/api/chat \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{"messages":[{"role":"user","content":"Say hi in one word"}]}'
# Expect: event: delta frames, then event: done with usage, then data: [DONE]
```

What ships:

- **Image** — `vllm/vllm-openai:v0.6.3.post1` (pinned; bump deliberately).
- **Model cache** — named volume `vllm_cache` persists HuggingFace downloads across `compose down` so a 2 GB model weight doesn't re-pull every restart.
- **GPU** — `deploy.resources.reservations.devices` requests one NVIDIA device. On a host without the NVIDIA container runtime this is silently ignored and vLLM boots in CPU mode (slow but functional for smoke tests).
- **Healthcheck** — `curl -fsS http://localhost:8000/v1/models` with `start_period: 120s` to absorb cold load. Larger models may need a higher value.
- **Hardening** — `cap_drop: [ALL]`, `no-new-privileges`, `tmpfs /tmp` (mirrors every other service per `SECURITY.md` M12).
- **Network** — loopback-only host port (`:8000` → `127.0.0.1:8000`) for dev debugging; the backend reaches vLLM on the compose network via the `vllm` DNS alias.

Tuning knobs (all `compose .env` overrides):

| Variable | Default | Purpose |
|---|---|---|
| `LLM_PROVIDER` | `openai` | `openai` (SDK) or `vllm` (raw HTTP) |
| `LLM_BASE_URL` | `http://vllm:8000/v1` | URL of the chat-completions root |
| `LLM_API_KEY` | empty | Bearer token; vLLM without `--api-key` accepts empty |
| `LLM_MODEL` | `Qwen/Qwen2.5-3B-Instruct` | Forwarded as the request `model` field; must match `--served-model-name` |
| `LLM_ALLOW_PRIVATE_URL` | `true` | Required when `LLM_BASE_URL` resolves to a private IP (the compose default) |
| `VLLM_MODEL` | `Qwen/Qwen2.5-3B-Instruct` | HuggingFace repo id passed to `vllm serve --model` |
| `HUGGING_FACE_HUB_TOKEN` | empty | Required for gated models (Llama, Mistral). Qwen2.5-3B is ungated |

Gated-model caveat: Llama / Mistral / Gemma require (a) a HuggingFace account, (b) accepting the model's license on the model card, and (c) `HUGGING_FACE_HUB_TOKEN` on the vLLM container. Without the token vLLM fails the download with `403 Forbidden`.

CPU caveat: a Qwen2.5-3B smoke test on CPU runs at ~3-8 tokens/sec after cold load — fine for "does it work?" verification, not a usable dev experience. For real iteration, run vLLM on a GPU host or switch to the smaller `Qwen/Qwen2.5-0.5B-Instruct` (~1 GB bf16).

Safety: `LLM_BASE_URL` is validated at startup (scheme allowlist http/https, no userinfo, host refuses loopback / RFC1918 / link-local / multicast / unspecified unless `LLM_ALLOW_PRIVATE_URL=true`). The vLLM provider repeats the IP-class check inside its `http.Transport.DialContext` so DNS rebinding can't swap a public hostname's answer for a private IP between startup and the first dial. See `SECURITY.md` for the residual risk on the OpenAI provider path.

## Testing

| Layer | Command | Notes |
|---|---|---|
| Backend unit | `cd backend && SKIP_CONTAINERS=true go test ./...` | No Docker needed |
| Backend full | `cd backend && go test ./...` | Needs Docker for testcontainers |
| Backend lint | `cd backend && golangci-lint run --timeout=5m` | |
| Backend sqlc drift | `cd backend && make sqlc-diff` | Verifies generated code is in sync |
| Frontend unit | `cd frontend && npm test` | Vitest + React Testing Library |
| Frontend lint | `cd frontend && npm run lint` | |
| Frontend build | `cd frontend && npm run build` | |
| E2E | `cd frontend && npm run test:e2e` | Playwright, needs backend running |

## Production

```bash
VITE_API_URL=http://your-production-ip/api \
  sudo docker compose -f docker-compose.prod.yml up -d --build
```

The app is served on port 80. API is proxied at `/api`.

## Kubernetes

```bash
cp k8s/secrets.yaml.example k8s/secrets.yaml   # edit with base64-encoded secrets
kubectl apply -f k8s/
```

See `k8s/*.yaml` for per-resource config.

## API Endpoints

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/health` | — | Liveness + DB status |
| POST | `/api/register` | — | Body `{username, email, password}`. Returns `{access_token, refresh_token, token_type: "Bearer", expires_at, user}` |
| POST | `/api/login` | — | Body `{username, password}`. Returns the Bearer pair + user |
| POST | `/api/refresh` | — | Body `{refresh_token}`. Returns a fresh Bearer pair + user. Reuse revokes the entire family |
| POST | `/api/logout` | Bearer | Body `{refresh_token}`. Revokes only that row. Returns 204 |
| GET | `/api/movies` | — | `?q=`, `?page=`, `?page_size=` |
| POST | `/api/movies` | JWT | |
| PUT | `/api/movies/{id}` | JWT | |
| DELETE | `/api/movies/{id}` | JWT | |

Full schema: `http://localhost:8080/api/swagger/doc.json` (interactive docs at `/api/swagger`).
The same spec is committed at `api/openapi.json` and regenerated offline with
`cd backend && make openapi`; CI runs `make openapi-diff` to catch drift.

## Conventions

- **Commits** — Conventional Commits (`feat:`, `fix:`, `refactor:`, `docs:`, `test:`, `chore:`).
  - Author via `git cz` (alias set up by `npm install`; runs commitizen + cz-customizable interactively). Plain `git commit -m` is also fine — the `.husky/commit-msg` hook runs `commitlint --edit` on the message file and rejects anything that doesn't match the schema.
  - Allowed scopes (kept in sync between `.cz-config.cjs` and `commitlint.config.js`): `backend`, `frontend`, `auth`, `infra`, `security`, `ci`, `docs`, `claude`, `env`, `observability`, `data`, `repo`.
  - Per-package releases: `semantic-release` (monorepo plugin) computes `backend@X.Y.Z` / `frontend@X.Y.Z` independently from commit paths, writes per-package `CHANGELOG.md`, stamps the new version into `ServiceVersion` + `frontend/package.json`, and pushes the release commit + tags. No npm publish.
- **Errors** — domain sentinels (`movie.ErrNotFound`, `user.ErrInvalidCredentials`, `user.ErrUsernameTaken` / `ErrEmailTaken` / `ErrRefreshTokenCollision` translated from `pgconn.PgError` by `internal/platform/pgerr`, `auth.ErrInvalidToken`, …). Handlers funnel every error through `api.MapError(ctx, err, "Failed to <op>")` — unknown errors reuse `ClassifyAndLog` so internal error text never reaches the wire. Never string-compare error messages.
- **Cache** — best-effort. Mutations call `DeletePrefix("movies:")` (SCAN + UNLINK, non-blocking).
- **Migrations** — `backend/migrations/00000N_description.{up,down}.sql`. Applied on every backend boot.

## Roadmap

See `FUTURE.md`, `FUTURE_BACKEND.md`, `FUTURE_FRONTEND.md`. Tick an item when the work lands.

## License

[PolyForm Noncommercial License 1.0.0](LICENSE), with an additional restriction prohibiting use of this software for training, fine-tuning, evaluating, or developing any AI/ML model or system.

Free for personal, academic, educational, and evaluation use. **Commercial use and AI training use are not permitted.** See [`LICENSE`](LICENSE) for the full terms.
