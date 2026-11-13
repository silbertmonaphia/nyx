#!/usr/bin/env bash
# Bootstrap a fresh dev box for Nyx in one command: toolchain checks,
# .env generation with fresh secrets, frontend deps, agent venv, backend
# build. Idempotent — existing .env files and node_modules are kept, so
# re-running never overwrites operator-edited values or rotates secrets.
#
#   scripts/bootstrap-dev.sh [--domain example.com] [--go-proxy] [--dry-run]
#
#   --domain X  also bake VITE_API_URL for the domain and stage an nginx
#               site config to /tmp (the sudo cp / certbot steps are
#               printed, never executed — the script stays sudo-free)
#   --go-proxy  build the backend via goproxy.cn (slow upstream proxy)
#   --dry-run   print every mutation instead of performing it
#
# uv is pinned to UV_WANT so the box resolves the committed uv.lock with
# the same resolver as the dev machine. Agent deps install with --frozen,
# not --locked: a user-level PyPI mirror in ~/.config/uv/uv.toml changes
# the default index, uv counts the index as part of the resolution, and
# --locked then reports a false "lockfile needs to be updated" (see
# scripts/check_no_comments.py — this repo documents shell knowledge in
# headers since shell is outside its comment gate).
#
# Secrets are generated in place and never echoed. Generated .env files
# get chmod 600.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

DOMAIN=""
GO_PROXY=false
DRY_RUN=false
NGINX_STAGED=""

say()  { printf '%s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --domain)
      [ "${2:-}" != "" ] || die "--domain needs a value"
      DOMAIN="$2"
      shift 2
      ;;
    --go-proxy) GO_PROXY=true; shift ;;
    --dry-run) DRY_RUN=true; shift ;;
    *) echo "unknown arg: $1 (supported: --domain X --go-proxy --dry-run)" >&2; exit 2 ;;
  esac
done

run() {
  if $DRY_RUN; then
    say "  would run: $*"
  else
    say "  run: $*"
    "$@"
  fi
}

run_in() {
  local dir="$1"
  shift
  if $DRY_RUN; then
    say "  would run (in $dir): $*"
  else
    say "  run (in $dir): $*"
    (cd "$dir" && "$@")
  fi
}

ensure_env_file() {
  local src="$1" dst="$2"
  if [ -f "$dst" ]; then
    say "  $dst: exists, keeping your values"
  else
    if $DRY_RUN; then
      say "  would create $dst from $src"
    else
      cp "$src" "$dst"
      chmod 600 "$dst"
      say "  created $dst"
    fi
  fi
}

ensure_key() {
  local file="$1" key="$2" value="$3" how="${4:-}"
  if grep -qE "^${key}=[^[:space:]#]+" "$file" 2>/dev/null; then
    say "  ${key}: already set"
  else
    if $DRY_RUN; then
      say "  would set ${key} in $file (${how:-static})"
    else
      printf '%s=%s\n' "$key" "$value" >> "$file"
      say "  set ${key} in $file (${how:-static})"
    fi
  fi
}

if [ ! -f Makefile ] || [ ! -d backend ] || [ ! -d frontend ]; then
  die "run from the repo checkout (expected Makefile + backend/ + frontend/)"
fi

if [ -n "$DOMAIN" ] && ! printf '%s' "$DOMAIN" | grep -qE '^[a-z0-9.-]+\.[a-z]{2,}$'; then
  die "invalid --domain: $DOMAIN"
fi

step "toolchain"

command -v openssl >/dev/null 2>&1 || die "openssl required for secret generation"
command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
  || die "docker + compose plugin required (infra: db/redis/jaeger)"

command -v go >/dev/null 2>&1 || die "go not on PATH — backend/go.mod pins go 1.26.11 (gvm install go1.26.11)"
say "go: $(go version)"

command -v python3 >/dev/null 2>&1 || die "python3 not on PATH — agent_py requires >= 3.12"
py_ok="$(python3 -c 'import sys; print(1 if sys.version_info >= (3, 12) else 0)')"
[ "$py_ok" = "1" ] || die "python >= 3.12 required, found $(python3 --version)"
say "python: $(python3 --version)"

NVM_DIR="${NVM_DIR:-$HOME/.nvm}"
if [ -s "$NVM_DIR/nvm.sh" ]; then
  # shellcheck disable=SC1091
  . "$NVM_DIR/nvm.sh"
  nvm use >/dev/null 2>&1 || true
fi
command -v node >/dev/null 2>&1 || die "node not on PATH — install nvm, then nvm install per .nvmrc (node 24)"
[ "$(node -p 'process.versions.node.split(".")[0]')" = "24" ] || die "node 24 required (.nvmrc), found $(node --version)"
say "node: $(node --version), npm: $(npm --version)"

UV_WANT="0.10.9"
uv_version() { "$1" --version 2>/dev/null | awk '{print $2}'; }
UV_BIN=""
if command -v uv >/dev/null 2>&1 && [ "$(uv_version uv)" = "$UV_WANT" ]; then
  UV_BIN="$(command -v uv)"
  say "uv: $UV_WANT at $UV_BIN"
else
  say "uv $UV_WANT not active (found: $(command -v uv 2>/dev/null || echo none) $(uv_version uv 2>/dev/null || true))"
  if $DRY_RUN; then
    say "  would install uv $UV_WANT via https://astral.sh/uv/${UV_WANT}/install.sh"
    UV_BIN="$HOME/.local/bin/uv"
  else
    curl -LsSf "https://astral.sh/uv/${UV_WANT}/install.sh" | sh
    export PATH="$HOME/.local/bin:$PATH"
    UV_BIN="$HOME/.local/bin/uv"
    [ -x "$UV_BIN" ] || die "uv installer did not produce $UV_BIN"
    [ "$(uv_version "$UV_BIN")" = "$UV_WANT" ] \
      || die "installed uv is $(uv_version "$UV_BIN"), expected $UV_WANT (another uv is shadowing; check PATH order)"
    say "  installed uv $UV_WANT at $UV_BIN"
  fi
fi
UV_BIN="${UV_BIN:-uv}"

step "infra"

running="$(docker ps --filter 'status=running' --filter 'label=com.docker.compose.service' --format '{{.Label "com.docker.compose.service"}}' 2>/dev/null | sort -u || true)"
missing=""
for svc in db redis jaeger; do
  printf '%s\n' "$running" | grep -qx "$svc" || missing="$missing $svc"
done
if [ -n "$missing" ]; then
  say "infra not running:$missing"
  run make up || die "make up failed — try: sudo docker compose up -d db redis jaeger"
else
  say "db / redis / jaeger running"
fi

step "env files"

ensure_env_file .env.example .env
ensure_env_file backend/.env.example backend/.env
ensure_env_file frontend/.env.example frontend/.env
ensure_env_file backend/agent_py/.env.example backend/agent_py/.env

pg_user="$(sed -n 's/^POSTGRES_USER=//p' .env 2>/dev/null || true)"
pg_pass="$(sed -n 's/^POSTGRES_PASSWORD=//p' .env 2>/dev/null || true)"
pg_db="$(sed -n 's/^POSTGRES_DB=//p' .env 2>/dev/null || true)"
pg_user="${pg_user:-postgres}"
pg_db="${pg_db:-nyx}"
if [ -z "$pg_pass" ]; then
  pg_pass="$(openssl rand -hex 16)"
  ensure_key .env POSTGRES_PASSWORD "$pg_pass" "generated"
fi

ensure_key backend/.env JWT_SECRET "$(openssl rand -base64 48 | tr -d '\n')" "generated"
ensure_key backend/.env AGENT_ENABLED true
ensure_key backend/.env AGENT_SERVICE_TOKEN "$(openssl rand -hex 32)" "generated, same value both runtimes"
if grep -qE '^LLM_ENABLED=true' backend/.env 2>/dev/null; then
  ensure_key backend/.env USER_LLM_ENCRYPTION_KEY "$(openssl rand -hex 32)" "generated"
fi

if [ -n "$DOMAIN" ]; then
  ensure_key frontend/.env VITE_API_URL "https://${DOMAIN}/api" "domain-baked"
  ensure_key frontend/.env API_ORIGIN "https://${DOMAIN}" "domain-baked"
fi

ensure_key backend/agent_py/.env PYTHON_AGENT_DATABASE_URL \
  "postgresql://${pg_user}:${pg_pass}@127.0.0.1:5433/${pg_db}" "local infra DSN (search_path added by the app)"

if ! $DRY_RUN; then
  chmod 600 .env backend/.env frontend/.env backend/agent_py/.env
fi

step "frontend deps"

if [ -x frontend/node_modules/.bin/vite ]; then
  say "  frontend/node_modules present — skipping npm ci (rm -rf frontend/node_modules to force)"
else
  run_in frontend npm ci --workspaces=false
fi

step "agent venv"

run_in backend/agent_py "$UV_BIN" sync --frozen --extra dev

step "backend build"

if $GO_PROXY; then
  export GOPROXY="https://goproxy.cn,direct"
fi
run_in backend go build ./...

if [ -n "$DOMAIN" ]; then
  step "nginx config (staged)"
  NGINX_STAGED="/tmp/nyx-nginx-${DOMAIN}.conf"
  if $DRY_RUN; then
    say "  would write $NGINX_STAGED"
  else
    cat > "$NGINX_STAGED" <<EOF
server {
    listen 80;
    server_name ${DOMAIN};

    location / {
        proxy_pass http://127.0.0.1:5173;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";
    }

    location /api/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_buffering off;
        proxy_read_timeout 700s;
    }
}
EOF
    chmod 644 "$NGINX_STAGED"
    say "  wrote $NGINX_STAGED"
  fi
fi

step "done — next steps (manual)"

say "  1. LLM endpoints: set PYTHON_AGENT_LLM_BASE_URL / PYTHON_AGENT_LLM_API_KEY /"
say "     PYTHON_AGENT_LLM_MODEL in backend/agent_py/.env (vLLM or any OpenAI-compatible"
say "     server). backend/.env LLM_* only if you use the Go single-turn chat path."
say "  2. start: make dev        (all three, detached; logs /tmp/nyx-*.log)"
say "     or:    make dev-backend / make dev-frontend / make dev-agent"
say "  3. verify: curl http://127.0.0.1:8080/api/feeds (401 = up),"
say "     curl http://127.0.0.1:9000/healthz (200 = up), http://localhost:5173 (SPA)"

if [ -n "$NGINX_STAGED" ] && [ -f "$NGINX_STAGED" ]; then
  say "  4. nginx + TLS for ${DOMAIN} (needs sudo, DNS A record, port 80 open):"
  say "      sudo cp $NGINX_STAGED /etc/nginx/sites-available/nyx"
  say "      sudo ln -sf /etc/nginx/sites-available/nyx /etc/nginx/sites-enabled/nyx"
  say "      sudo rm -f /etc/nginx/sites-enabled/default"
  say "      sudo nginx -t && sudo systemctl reload nginx"
  say "      sudo ufw allow 80,443/tcp   # if ufw is active"
  say "      sudo certbot --nginx -d ${DOMAIN}"
fi
