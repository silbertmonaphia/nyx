# Deployment — VPS smoke rig (host dev stack + nginx + bare IP)

A minimal rig for eyeballing the stack on a rented VPS within minutes of
`make bootstrap`: the host dev stack (`make dev-{backend,agent,frontend}`
`DETACH=1`) behind one nginx server block. **This is a smoke-test rig, not
a durable deployment** — no restart-on-failure, vite dev serves source
maps, `go run` recompiles on boot. For anything long-lived use
[`deployment.md`](deployment.md) (Render + Vercel) or
[`deployment-selfhost.md`](deployment-selfhost.md) (Compose + Caddy + CD).

## Topology

Every service binds loopback (`127.0.0.1`) by default — the dev-stack
binds were made loopback-only deliberately, so **nginx is the only
public surface**:

| Port | Service | Bind | Public? |
|---|---|---|---|
| 80 | nginx | `0.0.0.0` | yes — the front door |
| 5173 | vite dev | `127.0.0.1` | via nginx `location /` |
| 8080 | Go API | `127.0.0.1` | via nginx `location /api/` |
| 9000 | agent API | `127.0.0.1` | never — the Go backend proxies it |
| 9001 | agent /metrics | `127.0.0.1` | never |

`/mcp` and `/otlp/` are deliberately not proxied: external MCP clients
and browser span export don't exist in this rig.

## Server-local config

`frontend/.env` — in dev mode vite reads this at startup; the default
fallback (`http://localhost:8080/api`) points the **visitor's browser**
at their own machine, so it must be set absolutely:

```dotenv
VITE_API_URL=http://<public-ip>/api
```

Changing it needs a frontend restart (`make stop && make dev-frontend
DETACH=1`), not just a refresh.

`backend/.env` and infra `.env` — as produced by `make bootstrap`; no
overrides needed (the compose-only `SERVER_HOST=0.0.0.0` default does
not apply to the host-run path, which pins loopback in `scripts/dev.sh`).

## nginx

`/etc/nginx/sites-enabled/nyx`:

```nginx
server {
    listen 80;
    server_name <public-ip>;

    client_max_body_size 25m;

    location / {
        proxy_pass http://127.0.0.1:5173;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }

    location /api/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

- `proxy_buffering off` is **mandatory**: `POST /api/chat` is
  `text/event-stream`; buffered, the stream renders only at the end.
- `client_max_body_size` — nginx defaults to 1 MB, which 413s KB uploads.
- Bare IPs can't get CA certificates, so this rig is `http://`-only.
  Real TLS needs a domain (`make bootstrap --domain <domain>` bakes
  `VITE_API_URL` and stages a certbot-ready site config).

## Verify

```sh
curl -o /dev/null -w '%{http_code}\n' http://<ip>/            # 200 SPA
curl -o /dev/null -w '%{http_code}\n' http://<ip>/api/feeds   # 401 auth-gated
```

Then register through the UI and confirm in browser DevTools → Network
that the request went to `http://<ip>/api/register` (not `localhost:8080`).

Gotchas seen in practice: a plain `curl localhost` on a proxy-configured
box can return `000` — use `--noproxy '*'`; if port 5173 was taken at
vite spawn it silently rebinds to 5174 — `ss -tlnp` is the source of
truth for what actually got bound.

## Tear-down

`make stop` halts the dev processes; infra containers stay up until
`make down`.
