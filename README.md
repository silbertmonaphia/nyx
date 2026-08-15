# Nyx

A minimalist media rating application — Go 1.26.1 API, React 19 SPA, PostgreSQL, optional Redis cache.

## Highlights

- **Clean-arch backend** — `chi v5` router + `huma v2` for declarative, OpenAPI 3.1-emitting HTTP handlers. SQL is type-safe via `sqlc` + `pgx/v5` + `pgxpool`.
- **Modern frontend** — React 19 + Vite 8 + TypeScript, Tailwind CSS v4, Radix UI primitives (shadcn-style), TanStack Query, Zustand, React Hook Form + Zod, axios.
- **Auth** — JWT access tokens (default 15m) paired with rotated refresh tokens (default 7d, opaque, sha256-hashed, family-level reuse detection). `POST /api/refresh` + `POST /api/logout`. Bcrypt-hashed passwords.
- **Pagination** — `GET /api/movies` returns `{data, page, page_size, total, has_more}`. Default 20, max 100.
- **Cache-aside** — Redis opt-in for `GET /api/movies` (`REDIS_ENABLED=true`). Cache is best-effort; failures never fail the request.
- **Observability** — `/metrics` (Prometheus), structured JSON logging via `zerolog`, graceful shutdown.
- **CI/CD** — GitHub Actions (lint + unit + integration + sqlc drift + e2e), `golangci-lint`, `husky` pre-commit on the frontend.
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
| POST | `/api/register` | — | Body `{username, email, password}`. Returns `{token, refresh_token, expires_at, user}` |
| POST | `/api/login` | — | Body `{username, password}`. Returns `{token, refresh_token, expires_at, user}` |
| POST | `/api/refresh` | — | Body `{refresh_token}`. Rotates the refresh token; reuse revokes the entire family. Returns `{token, refresh_token, expires_at, user}` |
| POST | `/api/logout` | JWT | Body `{refresh_token}`. Revokes the supplied token's family. Returns 204 |
| GET | `/api/movies` | — | `?q=`, `?page=`, `?page_size=` |
| POST | `/api/movies` | JWT | |
| PUT | `/api/movies/{id}` | JWT | |
| DELETE | `/api/movies/{id}` | JWT | |

Full schema: `http://localhost:8080/api/swagger/doc.json` (interactive docs at `/api/swagger`).
The same spec is committed at `api/openapi.json` and regenerated offline with
`cd backend && make openapi`; CI runs `make openapi-diff` to catch drift.

## Conventions

- **Commits** — Conventional Commits (`feat:`, `fix:`, `refactor:`, `docs:`, `test:`, `chore:`).
- **Errors** — domain sentinels (`movie.ErrNotFound`, `user.ErrInvalidCredentials`, `auth.ErrInvalidToken`, …). Handlers translate to HTTP status. Never string-compare error messages.
- **Cache** — best-effort. Mutations call `DeletePrefix("movies:")` (SCAN + UNLINK, non-blocking).
- **Migrations** — `backend/migrations/00000N_description.{up,down}.sql`. Applied on every backend boot.

## Roadmap

See `FUTURE.md`, `FUTURE_BACKEND.md`, `FUTURE_FRONTEND.md`. Tick an item when the work lands.

## License

[PolyForm Noncommercial License 1.0.0](LICENSE), with an additional restriction prohibiting use of this software for training, fine-tuning, evaluating, or developing any AI/ML model or system.

Free for personal, academic, educational, and evaluation use. **Commercial use and AI training use are not permitted.** See [`LICENSE`](LICENSE) for the full terms.
