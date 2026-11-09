# Deployment — Self-hosted server (Docker Compose + Caddy + CD)

Operator runbook for a single self-managed server (written against a
Tencent Cloud Lighthouse box). The PaaS path (Render + Vercel) is
covered in [`deployment.md`](deployment.md); the two share the same
backend contract (Bearer auth, cross-origin CORS, `/api/livez`).

```
GitHub push mvp ──▶ CI (test/e2e/audit gates) ──▶ release
                                        │ green
                                        ▼
                              deploy-mvp job (ci.yml)
                              rsync tree ──▶ server
                                        ▼
                       scripts/deploy.sh on the server:
                       docker compose --profile app up
                                      --build --wait
                                        ▼
                       public smoke: GET https://api.<domain>/api/livez

browser ──HTTPS──▶ Caddy :443 ──▶ api.<domain>  → backend:8080 (compose net)
                             └──▶ app.<domain>  → frontend:8080 (compose net)
```

Nothing is exposed on the host except Caddy's 80/443; SSH for
operators. `docker-compose.override.yml` (server-local, untracked)
rewires port bindings; `Caddyfile` terminates TLS.

## Part 1 — one-time server bootstrap

### 1.1 Base install

```bash
curl -fsSL https://get.docker.com | sh          # Docker + compose plugin
docker compose version                          # needs >= v2.20 (--wait)
sudo apt install -y rsync                       # CD rsync target
```

DNS: `A` records for `api.<domain>` and `app.<domain>` → server IP.
Tencent console 安全组 / `ufw`: allow `22, 80, 443/tcp` only. Jaeger UI
and the DB/Redis ports must stay off the public internet (the compose
override below rebinds Jaeger to loopback).

A 2 GB box should add swap before the first (cache-cold) build:

```bash
sudo fallocate -l 2G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
```

### 1.2 Server-local config files

Get the repo onto the server once (rsync from your machine, or `git
clone`). Everything below lives **only** on the server — the CD rsync
explicitly excludes these paths, so deploys never overwrite them:

| Path | Role |
|---|---|
| `.env` | compose interpolation + db creds + agent LLM endpoint |
| `backend/.env` | Go backend runtime config (env_file) |
| `frontend/.env` | must exist (compose env_file); build args come from root `.env` |
| `docker-compose.override.yml` | port rewiring + Caddy service |
| `Caddyfile` | TLS + routing |

**`.env`** (root):

```dotenv
POSTGRES_USER=nyx
POSTGRES_PASSWORD=<openssl rand -hex 16>
POSTGRES_DB=nyx
VITE_API_URL=https://api.<domain>/api
API_ORIGIN=https://api.<domain>
PYTHON_AGENT_LLM_BASE_URL=https://api.deepseek.com/v1
PYTHON_AGENT_LLM_MODEL=deepseek-chat
```

`VITE_API_URL` / `API_ORIGIN` are interpolated into the frontend
image **at build time** — changing them means rebuilding the image.
`PYTHON_AGENT_LLM_BASE_URL` must be here and **not** in
`backend/.env`: the compose `agent.environment:` block interpolates it
from the root `.env` and masks any `env_file` value; left unset it
points at the (profile-off) in-compose vLLM service.

**`backend/.env`** (copy `backend/.env.example`, then set):

```dotenv
JWT_SECRET=<openssl rand -base64 48>
DB_URL=postgres://nyx:<password>@db:5432/nyx?sslmode=disable
SERVER_HOST=0.0.0.0
REDIS_ENABLED=true
REDIS_URL=redis://redis:6379
CORS_ALLOWED_ORIGINS=https://app.<domain>
OTEL_ENABLED=true
OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318
LLM_ENABLED=true
LLM_PROVIDER=openai
LLM_BASE_URL=https://api.deepseek.com/v1
LLM_API_KEY=sk-...
LLM_MODEL=deepseek-chat
USER_LLM_ENCRYPTION_KEY=<openssl rand -hex 32>
AGENT_ENABLED=true
PYTHON_AGENT_URL=http://agent:9000
AGENT_SERVICE_TOKEN=<openssl rand -hex 32>
```

`SERVER_HOST=0.0.0.0` is **required for the compose backend**: the bind
default is loopback (`127.0.0.1`) since 2026-11, and a container bound
to loopback is unreachable through its published port. Upgrading a
server provisioned before that change: append the line to
`backend/.env` and `docker compose --profile app up -d backend`.

`USER_LLM_ENCRYPTION_KEY` (≥ 32 bytes) is required once
`LLM_ENABLED=true`; rotating it makes every stored per-user BYOK key
undecryptable. The agent hop is in-network `http://agent:9000` (dotless
service name → the https-only rule does not apply). RAG/vector index:
without `LLM_EMBEDDING_*` the embedder reuses the chat endpoint, and
chat-tuned models serve no `/v1/embeddings` — feeds stay unindexed
(warn only). See the `LLM_EMBEDDING_*` block in
`backend/.env.example` for the dedicated-embedpoint recipe and the
`/v1`-suffix asymmetry.

**`frontend/.env`**: `cp frontend/.env.example frontend/.env`
(compose requires the file to exist; the `VITE_*` build args read the
root `.env`).

**`Caddyfile`** (repo root on the server):

```caddyfile
api.<domain> {
	reverse_proxy backend:8080 {
		flush_interval -1
	}
}

app.<domain> {
	reverse_proxy frontend:8080 {
		flush_interval -1
	}
}
```

`flush_interval -1` keeps chat SSE unbuffered.

**`docker-compose.override.yml`** (repo root on the server):

```yaml
services:
  backend:
    ports: !override []
  frontend:
    ports: !override []
  jaeger:
    ports: !override ["127.0.0.1:16686:16686"]

  caddy:
    image: caddy:2
    restart: always
    ports: ["80:80", "443:443"]
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
      - caddy_config:/config
    networks: [default]

volumes:
  caddy_data:
  caddy_config:
```

`!override` replaces the base `ports:` list (needs compose ≥ 2.24; on
older versions the lists merge and the public bindings survive).
Jaeger keeps a loopback UI: `ssh -L 16686:127.0.0.1:16686
<server>` then open `http://localhost:16686`.

### 1.3 First bring-up + verify

```bash
docker compose --profile app up --build -d --wait
docker compose --profile app ps        # backend / agent healthy, agent-migrate exited 0

curl -i https://api.<domain>/api/livez
curl -i https://api.<domain>/api/health
curl -i -X OPTIONS https://api.<domain>/api/login \
  -H "Origin: https://app.<domain>" \
  -H "Access-Control-Request-Method: POST" \
  -H "Access-Control-Request-Headers: content-type"
```

CORS preflight must echo `Access-Control-Allow-Origin:
https://app.<domain>`. Then register a user in the SPA, send a chat
message (exercises Go proxy → Python agent → external LLM), swagger at
`https://api.<domain>/api/swagger`.

## Part 2 — continuous deployment

`ci.yml` gains a `deploy-mvp` job: `needs: [release]` on pushes to
`mvp` only, and gated on the repo variable `NYX_DEPLOY_ENABLED` —
unset or anything but `true` skips the job, so the pipeline deploys
nothing until you opt in. CI green + release done → rsync →
`scripts/deploy.sh` on
the server (`docker compose --profile app up --build -d --wait`) →
public smoke check. The rollout gate is the backend image's own
`HEALTHCHECK` (`wget /api/livez`, shallow by design); `--wait` blocks
until every app service is healthy and `agent-migrate` has exited 0.
Job-level `concurrency: deploy-mvp` (no cancel-in-progress) serialises
rapid pushes — a newer pending run supersedes an older queued one.

### 2.1 Server prerequisites

```bash
sudo adduser <deploy-user> docker       # or reuse the admin user
sudo mkdir -p /opt/nyx && sudo chown <deploy-user>: /opt/nyx
```

### 2.2 GitHub secrets / variables

On your machine (not the server):

```bash
ssh-keygen -t ed25519 -N '' -C nyx-deploy -f nyx_deploy_key
ssh-keyscan -H <SERVER_IP> > nyx_known_hosts
cat nyx_deploy_key.pub | ssh <user>@<SERVER_IP> \
  'mkdir -p ~/.ssh && cat >> ~/.ssh/authorized_keys'

gh secret set NYX_DEPLOY_SSH_KEY < nyx_deploy_key
gh secret set NYX_DEPLOY_KNOWN_HOSTS < nyx_known_hosts
gh variable set NYX_DEPLOY_HOST --body <SERVER_IP>
gh variable set NYX_DEPLOY_USER --body <user>
gh variable set NYX_DEPLOY_API_URL --body https://api.<domain>
gh variable set NYX_DEPLOY_PATH --body /opt/nyx
gh variable set NYX_DEPLOY_ENABLED --body true
```

`NYX_DEPLOY_PATH` defaults to `/opt/nyx`. `NYX_DEPLOY_ENABLED` is the
kill switch from §2 — while it is unset or false, every push to `mvp`
runs CI and release but never touches the server.

Delete `nyx_deploy_key` locally after `gh secret set`. The runner
IP pool is Azure-wide — the Tencent 安全组 must allow 22 from
`0.0.0.0/0`; it is inbound-only SSH with a dedicated key.

### 2.3 Rehearse manually first

Before trusting the workflow, replay its steps from your dev box:

```bash
rsync -az --delete -e ssh \
  --exclude=.git --exclude=.claude --exclude=.env \
  --exclude=backend/.env --exclude=frontend/.env \
  --exclude=backend/agent_py/.env \
  --exclude=docker-compose.override.yml --exclude=Caddyfile \
  --exclude=node_modules --exclude=.venv \
  ./ <user>@<SERVER_IP>:/opt/nyx/

ssh <user>@<SERVER_IP> 'bash -s' -- < scripts/deploy.sh
```

### 2.4 Failure / rollback

- **deploy-mvp skipped** → the `release` job failed (usually a
  non-Conventional commit message on mvp); fix the message and push.
- **`--wait` times out** → `docker compose logs backend agent` on the
  server; the job stays red, old containers have already been replaced
  by `up -d` recreation.
- **Rollback** = revert the bad commit and push — the same pipeline
  redeploys the previous tree. Server-side there is no image-level
  undo: `docker compose build` overwrites the `nyx-*` image tags and
  `scripts/deploy.sh` prunes dangling images after each deploy.
- **Server-local edits to tracked files** are wiped by the next
  deploy's `rsync --delete` — server-specific configuration belongs in
  the excluded files listed in §1.2.

### 2.5 Known costs

- Cache-cold first build ≈ 5–10 min on 2 vCPU; warm ≈ 2–4 min
  (Go module / npm layers are cached by Docker).
- `GOPROXY` defaults to `https://goproxy.cn,direct` in the compose
  build args — China-friendly. npm registry traffic from a mainland
  box is occasionally slow but unthrottled.
- `docker compose build` always passes `VERSION=dev` (compose does not
  wire the release semver into the build arg) — `service.version` in
  traces reads `dev` on this topology.
- The `vllm` / `embed` / `langfuse` profiles are untouched; flipping
  them on later is a server-side `--profile` change plus the env vars
  documented in `docker-compose.yml`.
