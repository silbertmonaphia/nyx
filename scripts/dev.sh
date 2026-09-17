#!/usr/bin/env bash
# Run the backend (Go) and frontend (Vite) side-by-side against the
# local docker-compose infra (db / redis / jaeger). Assumes the
# infra containers are already up — bring them with `make up` or
# `docker compose up -d db redis jaeger`.
#
# Output: both streams tee to /tmp/nyx-{backend,frontend}.log so you
# can tail either independently while keeping this terminal quiet.
# PIDs land in /tmp/nyx-{backend,frontend}.pid for `make stop`.
#
# Ctrl+C kills both processes and the script exits cleanly.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

LOG_DIR="${TMPDIR:-/tmp}"
BACKEND_LOG="$LOG_DIR/nyx-backend.log"
FRONTEND_LOG="$LOG_DIR/nyx-frontend.log"
BACKEND_PID="$LOG_DIR/nyx-backend.pid"
FRONTEND_PID="$LOG_DIR/nyx-frontend.pid"

# --- preflight ---------------------------------------------------------
command -v go >/dev/null  || { echo "go not on PATH"    >&2; exit 1; }
command -v npm >/dev/null || { echo "npm not on PATH"   >&2; exit 1; }

if [[ ! -f .env ]]; then
  echo ".env missing — copy .env.example and set JWT_SECRET (32+ bytes)" >&2
  exit 1
fi

running=$(docker compose ps --status running --format '{{.Service}}' 2>/dev/null | sort -u)
missing=()
for svc in db redis jaeger; do
  grep -qxF "$svc" <<<"$running" || missing+=("$svc")
done
if (( ${#missing[@]} > 0 )); then
  echo "infra containers not running: ${missing[*]} — run 'make up' (or 'docker compose up -d db redis jaeger') first" >&2
  exit 1
fi

# .env.example ships DB_URL with the in-network hostname `db:5432`,
# which only resolves from inside the compose bridge. Override here
# so the locally-run backend reaches the host-mapped port instead.
# Same story for REDIS_URL — `redis:6379` only resolves on the
# compose network; the host-mapped port is 6380 (see docker-compose.yml).
export DB_URL='postgres://postgres:postgres@127.0.0.1:5433/nyx?sslmode=disable'
export REDIS_URL='redis://127.0.0.1:6380'

# --- start both, forward their stdout/stderr to per-process logs -----
cleanup() {
  local code=$?
  [[ -f "$BACKEND_PID"  ]] && kill "$(cat "$BACKEND_PID")"  2>/dev/null || true
  [[ -f "$FRONTEND_PID" ]] && kill "$(cat "$FRONTEND_PID")" 2>/dev/null || true
  rm -f "$BACKEND_PID" "$FRONTEND_PID"
  exit "$code"
}
trap cleanup INT TERM EXIT

echo ">> backend  → $BACKEND_LOG"
( cd backend  && exec go run ./cmd/api ) >"$BACKEND_LOG"  2>&1 &
echo $! >"$BACKEND_PID"

echo ">> frontend → $FRONTEND_LOG"
( cd frontend && exec npm run dev     ) >"$FRONTEND_LOG" 2>&1 &
echo $! >"$FRONTEND_PID"

cat <<EOF

    backend  : http://localhost:8080/api
    frontend : http://localhost:5173
    jaeger   : http://localhost:16686

    tail backend  : tail -f $BACKEND_LOG
    tail frontend : tail -f $FRONTEND_LOG

Ctrl+C to stop both.
EOF

# Block until either child exits, then tear the other one down too.
wait -n