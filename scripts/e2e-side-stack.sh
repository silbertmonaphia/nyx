#!/usr/bin/env bash
# Bring up a SIDE instance of the full stack on spare ports so Playwright
# can drive the *new code* without ever touching the operator's dev stack
# (which owns :8080 / :9000 / :5173 — see the worktree-sharing memory note).
#
#   Go backend   :8180   (new code; talks to the side agent)
#   Python agent :9100   (new code; metrics :9101)
#   Vite SPA     :5273   (VITE_API_URL=http://localhost:8180)
#
# Assumes the shared infra is already up (`make up`: Postgres :5433,
# Redis :6380) and the host vLLM is serving the chat model on :8000
# (`make dev-vllm-main`) — this script starts NOTHING in docker.
#
# Usage:
#   ./scripts/e2e-side-stack.sh up      # start all three, wait for health
#   ./scripts/e2e-side-stack.sh stop    # kill what `up` started
#   ./scripts/e2e-side-stack.sh status  # ports + pids
#
# Then drive Playwright against it:
#   PLAYWRIGHT_BASE_URL=http://localhost:5273 \
#     npx playwright test tests/e2e/<spec>.ts
# (Playwright's webServer block reuses the operator's vite on :5173 and
# is harmless; the baseURL override is what routes the browser to :5273.)
#
# Logs/PIDs: /tmp/nyx-e2e-<name>${E2E_STACK_SUFFIX}.{log,pid} — set
# E2E_STACK_SUFFIX (e.g. -ab) to run several side stacks concurrently
# without clobbering each other's files; empty keeps the original names.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

GO_PORT="${E2E_GO_PORT:-8180}"
AGENT_PORT="${E2E_AGENT_PORT:-9100}"
AGENT_METRICS_PORT="${E2E_AGENT_METRICS_PORT:-9101}"
VITE_PORT="${E2E_VITE_PORT:-5273}"
SUFFIX="${E2E_STACK_SUFFIX:-}"

pidfile() { echo "/tmp/nyx-e2e-${1}${SUFFIX}.pid"; }
logfile() { echo "/tmp/nyx-e2e-${1}${SUFFIX}.log"; }

DB_URL_SIDE='postgres://postgres:postgres@127.0.0.1:5433/nyx?sslmode=disable'
AGENT_DATABASE_URL='postgresql://postgres:postgres@localhost:5433/nyx?options=-c%20search_path%3Dlanggraph%2Cagent'

start_agent() {
  echo "starting side agent on :$AGENT_PORT (metrics :$AGENT_METRICS_PORT)"
  (
    cd "$ROOT/backend/agent_py"
    # Explicit env wins over the operator's .env (pydantic-settings
    # priority: env vars > dotenv file) so none of the compose hostnames
    # or operator ports leak in.
    # setsid → own process group, so `stop` can kill the whole tree by
    # PGID without ever matching the operator's processes.
    setsid env \
    PYTHON_AGENT_PORT="$AGENT_PORT" \
    PYTHON_AGENT_METRICS_PORT="$AGENT_METRICS_PORT" \
    PYTHON_AGENT_DATABASE_URL="$AGENT_DATABASE_URL" \
    PYTHON_AGENT_REDIS_URL='redis://localhost:6380' \
    PYTHON_AGENT_LLM_BASE_URL='http://localhost:8000/v1' \
    PYTHON_AGENT_LLM_API_KEY='side-stack' \
    PYTHON_AGENT_MCP_URL="http://localhost:${GO_PORT}/mcp" \
    PYTHON_AGENT_RAG_BRIDGE_URL="http://localhost:${GO_PORT}/internal/rag/retrieve" \
    PYTHON_AGENT_OTEL_ENDPOINT='http://localhost:4318' \
      ./.venv/bin/python -m agent.main > "$(logfile agent)" 2>&1 &
    echo $! > "$(pidfile agent)"
  )
}

start_backend() {
  echo "starting side backend on :$GO_PORT"
  (
    cd "$ROOT/backend"
    # PORT/PYTHON_AGENT_URL/CORS/DB_URL/REDIS_URL are env overrides on top
    # of backend/.env (viper: env > file), so JWT_SECRET and the other
    # operator settings carry over while the side ports don't collide.
    setsid env \
    PORT="$GO_PORT" \
    DB_URL="$DB_URL_SIDE" \
    REDIS_URL='redis://localhost:6380' \
    AGENT_ENABLED='true' \
    MCP_ENABLED='true' \
    PYTHON_AGENT_URL="http://127.0.0.1:${AGENT_PORT}" \
    CORS_ALLOWED_ORIGINS="http://localhost:${VITE_PORT},http://127.0.0.1:${VITE_PORT}" \
      go run ./cmd/api > "$(logfile backend)" 2>&1 &
    echo $! > "$(pidfile backend)"
  )
}

start_frontend() {
  echo "starting side vite on :$VITE_PORT"
  (
    cd "$ROOT/frontend"
    setsid env \
    VITE_API_URL="http://localhost:${GO_PORT}/api" \
      npx vite --port "$VITE_PORT" --strictPort > "$(logfile frontend)" 2>&1 &
    echo $! > "$(pidfile frontend)"
  )
}

wait_health() {
  local url="$1" name="$2" tries="${3:-60}"
  for _ in $(seq 1 "$tries"); do
    if curl -sf -m 2 "$url" >/dev/null 2>&1; then
      echo "  $name healthy"
      return 0
    fi
    sleep 1
  done
  echo "  $name NOT healthy after ${tries}s — check its /tmp/nyx-e2e-*.log" >&2
  return 1
}

cmd_up() {
  # Refuse to start onto occupied ports — otherwise a half-dead previous
  # run silently leaves a stale instance serving the port while the new
  # one fails to bind (the new PID file then points at a dead process).
  for port in "$GO_PORT" "$AGENT_PORT" "$AGENT_METRICS_PORT" "$VITE_PORT"; do
    if ss -ltn "sport = :$port" 2>/dev/null | tail -n +2 | grep -q .; then
      echo "port :$port is already in use — run '$0 stop' first (or a previous side stack is still up)" >&2
      exit 1
    fi
  done
  start_agent
  start_backend
  start_frontend
  echo "waiting for health..."
  # Agent healthz pings Redis; backend /api/feeds returns 401 unauthenticated
  # — a 401 means the process is up and serving, so accept any HTTP answer.
  wait_health "http://localhost:${AGENT_PORT}/healthz" "agent" 60
  for _ in $(seq 1 60); do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://localhost:${GO_PORT}/api/feeds" || true)"
    [[ "$code" == "401" ]] && { echo "  backend healthy"; break; }
    sleep 1
  done
  wait_health "http://localhost:${VITE_PORT}/" "vite" 60
  echo
  echo "side stack up:"
  echo "  SPA     http://localhost:${VITE_PORT}   (VITE_API_URL=http://localhost:${GO_PORT}/api)"
  echo "  backend http://localhost:${GO_PORT}"
  echo "  agent   http://localhost:${AGENT_PORT}  (metrics :${AGENT_METRICS_PORT})"
  echo
  echo "run Playwright with: PLAYWRIGHT_BASE_URL=http://localhost:${VITE_PORT} <playwright cmd>"
}

cmd_stop() {
  for name in agent backend frontend; do
    pidf="$(pidfile "$name")"
    if [[ -f "$pidf" ]]; then
      pid="$(cat "$pidf")"
      # setsid made each service its own process-group leader, so a
      # negative-PID kill takes the whole tree (go-run's compiled child,
      # npx's vite process, …) and can never reach the operator's
      # same-command processes — those sit in a different PGID.
      kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true
      rm -f "$pidf"
      echo "stopped $name (pgid $pid)"
    fi
  done
}

cmd_status() {
  for port in "$GO_PORT" "$AGENT_PORT" "$AGENT_METRICS_PORT" "$VITE_PORT"; do
    printf "  :%-5s " "$port"
    ss -ltn "sport = :$port" 2>/dev/null | tail -n +2 | head -1 || echo "free"
    echo
  done
  for name in agent backend frontend; do
    pidf="$(pidfile "$name")"
    [[ -f "$pidf" ]] && echo "  $name pid $(cat "$pidf")"
  done
}

case "${1:-up}" in
  up) cmd_up ;;
  stop) cmd_stop ;;
  status) cmd_status ;;
  *) echo "usage: $0 [up|stop|status]" >&2; exit 2 ;;
esac
