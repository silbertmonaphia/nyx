# Deployment — Render (API) + Vercel (SPA)

Production topology for the two PaaS targets this repo configures:

```
browser ──HTTPS──▶ Vercel      https://<app>.vercel.app        static SPA
       │
       └──HTTPS + CORS preflight──▶ Render  https://<api>.onrender.com/api/*
                                            │
                                            └──▶ Postgres (Render-managed or external)
```

The SPA talks to the API **cross-origin**. Every request carries a
`Bearer` token in the `Authorization` header — never a cookie — so the
CORS preflight is the CSRF defence and no `Access-Control-Allow-Credentials`
is needed (`withCredentials: false` on the axios instance).

## What lives in the repo

| File | Purpose |
|---|---|
| `render.yaml` | Render blueprint: the `nyx-api` web service (Docker) + a Postgres instance |
| `frontend/vercel.json` | Vercel build config for the SPA (root directory `frontend`) |
| `backend/Dockerfile` | The image Render builds; the liveness probe follows `$PORT` |

The Python agent (`backend/agent_py`) is **not** part of either config — it is a
dashboard-created Render web service. See [`agent-deployment.md`](agent-deployment.md)
for its env setup, the shared `AGENT_SERVICE_TOKEN`, health-path precheck, deploy
order, rotation, and rollback.

---

## Render — the API

### Option A — blueprint (new service)

1. Render Dashboard → **New → Blueprint** → pick this repo.
2. Render reads `render.yaml` and creates `nyx-api`. It prompts for
   `DB_URL` (paste the external Postgres connection string) and
   `CORS_ALLOWED_ORIGINS`.
3. It prompts for `CORS_ALLOWED_ORIGINS` — put the Vercel origin here,
   e.g. `https://<app>.vercel.app`.
4. First deploy boots → migrations run → the health check passes.

### Option B — an existing service (already running)

A service created by hand is **not** reconfigured by `render.yaml` —
Render only reads the file for **Blueprint-managed** resources. Mirror
these settings on the existing service:

| Setting | Value | Why |
|---|---|---|
| Runtime | Docker | `dockerfilePath: ./backend/Dockerfile`, `dockerContext: ./backend` |
| Health Check Path | `/api/livez` | The backend has **no `/` route** — the Render default 404s and fails every deploy |
| Region | same as the database | Render's internal DB hostname only resolves inside the service's own region |
| `PORT` | don't set it | Render injects it; the backend binds `:$PORT` (`internal/platform/config`) |

A hand-created service *can* be brought under blueprint management
instead: put its exact name and every currently configured option into
the YAML, then create a Blueprint pointing at this repo and sync. Render
replaces omitted settings with defaults, so the file has to mirror the
service in full, and the `databases:` entry has to carry the existing
database's name or Render creates a second one. From then on every push
to the branch re-applies the file — and dashboard edits made in between
can be overwritten by the next sync.

### Environment variables

`backend/.env.example` lists the **whole** configuration surface (~50
keys) so every knob is discoverable — not a required list. Only **two**
vars are hard-required at boot (`DB_URL`, `JWT_SECRET`); everything else
has a default and is opt-in.

| Var | Required | Value | Notes |
|---|---|---|---|
| `DB_URL` | yes | `postgres://…` | External Postgres (Supabase / Neon / RDS). Passwords with special characters must be URL-encoded — see [Database](#database) for the pooler specifics |
| `JWT_SECRET` | yes | random, ≥32 bytes | `config.Load()` **refuses to start** on the default value or a short key. Rotating it invalidates every issued token |
| `CORS_ALLOWED_ORIGINS` | yes for a browser SPA | `https://<app>.vercel.app` | Comma-separated **exact origins**. Empty ⇒ every cross-origin browser request is blocked |
| `MIGRATION_PATH` | no | `file://migrations` | Default already resolves inside the image |
| `REDIS_ENABLED` / `REDIS_URL` | no | — | Cache is off by default; `GET /api/feeds` goes straight to Postgres |
| `LLM_ENABLED` / `LLM_*` | no | — | Only needed for the chat path |

### Database

The database is **external** — the blueprint declares no `databases:`
block, so Render never creates one. Supabase, Neon and RDS all work;
the connection string goes into `DB_URL` in the Render dashboard (or at
blueprint-apply time).

Supabase specifics, because they are the two things that break:

- **Use the pooler host, not the direct host.** Render's egress is
  IPv4-only and Supabase's direct host (`db.<ref>.supabase.co`) is
  IPv6. The pooler host (`aws-0-<region>.pooler.supabase.com`) is
  IPv4-reachable.
- **Port 6543 is transaction mode (PgBouncer).** pgx caches prepared
  statements per connection, and transaction mode does not keep them
  alive across transactions → `prepared statement … does not exist`
  under load. Either use port **5432** on the pooler host (session
  mode, no such problem) or append
  `&default_query_exec_mode=simple_protocol` to the DSN, which makes
  pgx use the simple protocol.
- **`sslmode=require`** and a **URL-encoded password** — Supabase
  passwords routinely contain characters that break a raw DSN.

The backend applies every SQL migration on boot, **before** it starts
listening (`backend/cmd/api/main.go`) — golang-migrate, from the
`./migrations` directory baked into the image.

Two extensions matter:

- **`pg_trgm`** (migrations 000006/000007) — created inside a `DO` block
  that tolerates a role without `CREATE EXTENSION`. Non-fatal.
- **`vector`** (migration 000014, `CREATE EXTENSION IF NOT EXISTS vector`) —
  **not** wrapped, so it fails the boot if pgvector is missing. Supabase
  ships pgvector but leaves it off by default: enable it in
  **Database → Extensions → vector**, or run
  `CREATE EXTENSION IF NOT EXISTS vector;` in the SQL editor, before
  the first deploy.

### CORS — exact origins only

`internal/middleware/cors.go` matches the `Origin` header against an
exact allowlist (or `*`). There is **no `*.vercel.app` wildcard**, so
every Preview deployment — `https://<branch>-<hash>.vercel.app`, a
different origin each time — needs its own entry. Options:

- list each preview origin explicitly, or
- use `*` (technically fine here: no cookies, `withCredentials: false`),
  or
- point Preview deployments at a fixed origin by disabling preview
  deployments for the project.

Include the scheme and no trailing slash: `https://app.vercel.app`, not
`https://app.vercel.app/`.

---

## Vercel — the SPA

### Project settings

| Setting | Value |
|---|---|
| Root Directory | `frontend` |
| Install Command | `npm ci --workspaces=false` |
| Build Command | `npm run build` |
| Output Directory | `dist` |

### Where `vercel.json` has to live

Vercel reads `vercel.json` from the **project's Root Directory** — one
config file per project, and the file must sit next to the
`package.json` that project builds. That is why it is
`frontend/vercel.json` here: the Root Directory is `frontend`.

If the Root Directory is left at the repository root, `frontend/vercel.json`
is **ignored** — either move the file to the repo root (and adjust the
paths: `"buildCommand": "cd frontend && npm run build"`,
`"outputDirectory": "frontend/dist"`) or set the Root Directory to
`frontend`.

Values in `vercel.json` override the same setting in the Project
Settings dashboard, so the file wins once it is in the right place.

With the Root Directory set to `frontend`, the build cannot see the
repository root at all — the app is confined to that directory. The
`--workspaces=false` flag on the install command is therefore belt-and-
braces (it matters only if "Include source files outside of the Root
Directory" is enabled, which would let npm find the workspace root).

### Environment variables

| Var | Value | Notes |
|---|---|---|
| `VITE_API_URL` | `https://<api>.onrender.com/api` | The `/api` suffix is required — backend routes are `/api/*` |

Set it for **Production**, **Preview** and **Development** as needed.

`VITE_*` values are inlined into the bundle **at build time**
(`frontend/src/services/api.ts` — the axios `baseURL`; the chat SSE
client reads the same variable). Changing the variable therefore
requires a **new deployment** — Vercel does not patch a built bundle.
`frontend/.env` is gitignored, so the dashboard (or `vercel env add
VITE_API_URL production`) is the only way to set it.

### Deploy order

Set `VITE_API_URL` and the backend's `CORS_ALLOWED_ORIGINS` to each
other's origin *before* the first deploy — a browser blocks the response
otherwise, and the SPA shows a "no response" toast rather than a CORS
error.

---

## Verification

```bash
# 1. The API is up and the DB answers.
curl -i https://<api>.onrender.com/api/health

# 2. The API is alive (this is what Render's health check hits).
curl -i https://<api>.onrender.com/api/livez

# 3. CORS allows the SPA origin. Expect:
#    Access-Control-Allow-Origin: https://<app>.vercel.app
curl -i -X OPTIONS https://<api>.onrender.com/api/login \
  -H "Origin: https://<app>.vercel.app" \
  -H "Access-Control-Request-Method: POST" \
  -H "Access-Control-Request-Headers: content-type"

# 4. The SPA's bundle points at the API.
JS=$(curl -s https://<app>.vercel.app/ | grep -o '/assets/index-[^"]*\.js' | head -1)
curl -s "https://<app>.vercel.app${JS}" | grep -o 'https://<api>\.onrender\.com/api'
```

In the browser: open the SPA, register, and watch the Network tab — the
`/api/register` request must carry `Authorization: Bearer …` and return
200. API docs live at `https://<api>.onrender.com/api/swagger`.

---

## Caveats

- **Free web services sleep — nothing is deleted.** After ~15 minutes
  without an inbound request the free instance spins down. The service
  itself survives, and the database is external, so no data is lost.
  What *is* lost is the container's local filesystem: a free service
  cannot attach a persistent disk, so every wake-up starts from a
  clean image. The backend keeps no local state (sessions and tokens
  live in Postgres), so this is harmless — but do not put anything
  important on the container's disk.
- **The first request after a sleep pays the cold start** — roughly a
  minute, during which Render serves a waiting page. The SPA sets no
  axios timeout (`frontend/src/services/api.ts`), so the browser just
  waits instead of erroring out; the user sees a spinner, not a
  failure. Expect the very first request of a quiet period to hang for
  a while. A paid instance removes the sleep entirely.
- **Free-tier ceilings.** Each workspace gets 750 free instance hours
  per month (spun-down services don't burn hours); when they run out,
  free services are suspended until the next month. Bandwidth
  exhaustion without a payment method suspends them for the rest of
  the month.
- **Render's own free Postgres expires** 30 days after creation — but
  that only applies to a Render-managed database. With an external
  database (Supabase), the provider's own free-tier rules apply
  instead.
- **The chat path needs more than this blueprint.** With
  `AGENT_ENABLED=false` feeds and auth work, and the single-turn chat
  path needs only the LLM provider. Enabling the agent
  (`AGENT_ENABLED=true`) additionally requires `PYTHON_AGENT_URL` and the
  shared `AGENT_SERVICE_TOKEN` (the same ≥ 32-byte value on the agent
  service). If either is missing the API still starts and serves
  auth/feeds, but `/api/chat`, sessions, and approvals return 503 until
  the agent configuration is complete.
- **The host name moves.** `https://<api>.onrender.com` is stable as
  long as the service name is, but a custom domain is nicer for
  `VITE_API_URL` and the CORS allowlist. Changing the service name
  changes the URL and breaks both — update them together.
- **No CSP on Vercel.** The Docker image ships a `Content-Security-Policy`
  with `connect-src` limited to the API origin
  (`frontend/nginx.conf`). Vercel serves the bundle without one; if you
  add a CSP via `vercel.json` headers, allow the API origin in
  `connect-src` or every XHR is blocked.
