#!/usr/bin/env bash
# Run the backend (Go) and/or frontend (Vite) against the local
# docker-compose infra (db / redis / jaeger).
#
# Usage: ./scripts/dev.sh [backend|frontend|both] [--detach]
#
# Default is foreground — the started process attaches to your
# terminal so output streams live and Ctrl+C kills it cleanly. Pass
# --detach to run in background: stdout/stderr tee to the terminal
# AND to /tmp/nyx-{backend,frontend}.log; the script waits on each
# child, so Ctrl+C tears them down together. PIDs land in
# /tmp/nyx-*.pid for `make stop`.
#
# `both` requires --detach — two foreground processes can't share
# one terminal.
#
# Assumes the infra containers are already up for the backend —
# bring them with `make up` or `docker compose up -d db redis
# jaeger`. The frontend needs no infra.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

LOG_DIR="${TMPDIR:-/tmp}"
BACKEND_LOG="$LOG_DIR/nyx-backend.log"
FRONTEND_LOG="$LOG_DIR/nyx-frontend.log"
BACKEND_PID="$LOG_DIR/nyx-backend.pid"
FRONTEND_PID="$LOG_DIR/nyx-frontend.pid"

mode="${1:-both}"
detach=false

case "$#" in
  1) ;;
  2) case "${2}" in
       --detach) detach=true ;;
       *) echo "usage: $0 [backend|frontend|both] [--detach]" >&2; exit 2 ;;
     esac ;;
  *) echo "usage: $0 [backend|frontend|both] [--detach]" >&2; exit 2 ;;
esac

case "$mode" in
  backend|frontend|both) ;;
  *) echo "usage: $0 [backend|frontend|both] [--detach]" >&2; exit 2 ;;
esac

if [[ "$mode" == "both" && "$detach" != "true" ]]; then
  echo "'both' requires --detach — two foreground processes can't share one terminal" >&2
  echo "  run './scripts/dev.sh both --detach' or pick one of backend / frontend" >&2
  exit 2
fi

needs_backend()  { [[ "$mode" == backend  || "$mode" == both ]]; }
needs_frontend() { [[ "$mode" == frontend || "$mode" == both ]]; }

# --- backend preflight -------------------------------------------------
if needs_backend; then
  command -v go >/dev/null || { echo "go not on PATH" >&2; exit 1; }

  if [[ ! -f backend/.env ]]; then
    echo "backend/.env missing — copy backend/.env.example and set JWT_SECRET (32+ bytes)" >&2
    exit 1
  fi

  # config.Load() refuses the default placeholder AND an empty
  # JWT_SECRET (auth.MinSecretBytes = 32, RFC 7518 §3.2). Catch the
  # common "I copied backend/.env.example but forgot to fill the
  # secret" mistake here so the backend doesn't crash a few hundred
  # ms after spawning. Grep for an uncommented, non-empty assignment.
  if ! grep -qE '^JWT_SECRET=[^[:space:]#]+' backend/.env; then
    echo "JWT_SECRET missing or empty in backend/.env — set it to a 32+ byte random string" >&2
    echo "  generate with: openssl rand -base64 48" >&2
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

  # Three .env files live in this repo; only `backend/.env` is
  # sourced here. Why each of the other two is NOT:
  #
  #   - `backend/.env` (SOURCED): viper reads .env relative to the
  #     backend's CWD. `go run` is invoked from `backend/`, so
  #     viper would look for `backend/.env` — the file exists,
  #     but viper only reads it if `viper.SetConfigFile` is
  #     pointed at it explicitly (it is, in config.go) AND the
  #     process's effective CWD matches. Sourcing dumps every
  #     KEY=value into the spawned process's env, so viper's
  #     `BindEnv` + `AutomaticEnv` pick them up regardless of
  #     how the binary is invoked (cd backend || exec from
  #     elsewhere). Without this, viper silently falls back to
  #     the placeholder JWT_SECRET that auth.NewTokenService
  #     then rejects — the backend crashes ~200 ms after spawn.
  #
  #   - `frontend/.env` (NOT SOURCED): Vite reads .env natively
  #     by its own convention — `frontend/.env` flows straight
  #     into `import.meta.env.VITE_*` at dev time. Re-sourcing
  #     it in bash would be redundant AND would inject bare
  #     (un-prefixed) vars into the npm process's env that
  #     aren't supposed to be there.
  #
  #   - root `.env` (NOT SOURCED): consumed only by `docker
  #     compose` for the `db` / `vllm` services. Neither the
  #     backend nor the frontend process reads it; propagating
  #     it would leak infra tokens (HUGGING_FACE_HUB_TOKEN)
  #     into the backend's process env for no reason.
  #
  # `set -a` + `.` dumps every KEY=value into the spawned
  # process's environment; bash's normal parsing handles quoted
  # values.
  set -a
  # shellcheck disable=SC1091
  . ./backend/.env
  set +a

  # `backend/.env.example` ships DB_URL with the in-network hostname
  # `db:5432`, which only resolves from inside the compose bridge.
  # Override AFTER the source above so the locally-run backend reaches
  # the host-mapped port instead. Same story for REDIS_URL —
  # `redis:6379` only resolves on the compose network; the
  # host-mapped port is 6380 (see docker-compose.yml).
  export DB_URL='postgres://postgres:postgres@127.0.0.1:5433/nyx?sslmode=disable'
  export REDIS_URL='redis://127.0.0.1:6380'
fi

# --- frontend preflight ------------------------------------------------
if needs_frontend; then
  command -v npm >/dev/null || { echo "npm not on PATH" >&2; exit 1; }
fi

# --- start -------------------------------------------------------------
if $detach; then
  # Detached mode: each child runs as a backgrounded pipeline so the
  # script can wait on both simultaneously. Output tees to terminal +
  # per-process log file; PIDs land in /tmp/nyx-*.pid for `make stop`.
  cleanup() {
    local code=$?
    [[ -f "$BACKEND_PID"  ]] && kill "$(cat "$BACKEND_PID")"  2>/dev/null || true
    [[ -f "$FRONTEND_PID" ]] && kill "$(cat "$FRONTEND_PID")" 2>/dev/null || true
    rm -f "$BACKEND_PID" "$FRONTEND_PID"
    exit "$code"
  }
  trap cleanup INT TERM EXIT

  if needs_backend; then
    echo ">> backend  → $BACKEND_LOG"
    # Capture the subshell's PID (the one that exec's into the process
    # we actually want to control) before tee steals the rightmost
    # slot of the pipeline. $BASHPID is the current subshell's PID; $!
    # would report tee's PID, which is downstream and dies naturally
    # when its stdin EOFs.
    ( cd backend && echo $BASHPID > "$BACKEND_PID" && exec go run ./cmd/api ) \
      2>&1 | tee "$BACKEND_LOG" &
  fi

  if needs_frontend; then
    echo ">> frontend → $FRONTEND_LOG"
    ( cd frontend && echo $BASHPID > "$FRONTEND_PID" && exec npm run dev ) \
      2>&1 | tee "$FRONTEND_LOG" &
  fi

  cat <<EOF

    backend  : http://localhost:8080/api
    frontend : http://localhost:5173
    jaeger   : http://localhost:16686

    tail backend  : tail -f $BACKEND_LOG
    tail frontend : tail -f $FRONTEND_LOG

Ctrl+C to stop$( [[ "$mode" != "both" ]] && echo " $mode" ).
EOF

  # Wait for whichever children were started. Capture each exit code so
  # the script can report which one died. Errors stream live to this
  # terminal AND to the per-process log file via tee.
  backend_exit=0
  frontend_exit=0
  [[ -f "$BACKEND_PID"  ]] && wait "$(cat "$BACKEND_PID")"  2>/dev/null || backend_exit=$?
  [[ -f "$FRONTEND_PID" ]] && wait "$(cat "$FRONTEND_PID")" 2>/dev/null || frontend_exit=$?

  if (( backend_exit != 0 )); then
    echo "backend exited with status $backend_exit" >&2
  fi
  if (( frontend_exit != 0 )); then
    echo "frontend exited with status $frontend_exit" >&2
  fi
  exit $(( backend_exit != 0 ? backend_exit : frontend_exit ))
else
  # Foreground mode: replace the script with the single child process
  # so it inherits the terminal's tty + job-control directly. Ctrl+C
  # in the terminal kills it; no PID file or cleanup trap needed.
  if needs_backend; then
    echo ">> backend  starting in foreground (Ctrl+C to stop)"
    cd backend || exit 1
    exec go run ./cmd/api
  fi

  if needs_frontend; then
    echo ">> frontend starting in foreground (Ctrl+C to stop)"
    cd frontend || exit 1
    exec npm run dev
  fi
fi
