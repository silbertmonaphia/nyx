#!/usr/bin/env bash
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
AGENT_URL="${AGENT_URL:-http://localhost:9100}"

# shellcheck disable=SC1091
. "$ROOT/scripts/lib/agent-token.sh"
AGENT_TOKEN_ROOT="$ROOT"
export AGENT_TOKEN_ROOT
if ! TOKEN="$(resolve_agent_service_token)"; then
  echo "failed to resolve AGENT_SERVICE_TOKEN" >&2
  exit 1
fi

failures=0

check() {
  local name="$1" expected="$2" actual="$3"
  if [[ "$actual" == "$expected" ]]; then
    printf 'ok   %s (%s)\n' "$name" "$actual"
  else
    printf 'FAIL %s — want %s, got %s\n' "$name" "$expected" "$actual" >&2
    failures=$((failures + 1))
  fi
}

status() {
  curl -s -o /dev/null -w '%{http_code}' -m 5 "$@"
}

run_request=(-X POST -H 'Content-Type: application/json' -d '{"messages":[]}')
wrong_token='wrong-token-0123456789-0123456789'

health_status="$(status "$AGENT_URL/healthz")"
if [[ "$health_status" == "200" || "$health_status" == "503" ]]; then
  printf 'ok   health without token (%s)\n' "$health_status"
else
  printf 'FAIL health without token — want 200 or 503, got %s\n' "$health_status" >&2
  failures=$((failures + 1))
fi

check "metrics without token" 401 "$(status "$AGENT_URL/metrics")"
check "run without token" 401 "$(status "${run_request[@]}" "$AGENT_URL/agent/run")"
check "run with wrong token" 401 "$(status "${run_request[@]}" -H "X-Nyx-Service-Token: $wrong_token" "$AGENT_URL/agent/run")"
check "run with token but no user id" 400 "$(status "${run_request[@]}" -H "X-Nyx-Service-Token: $TOKEN" "$AGENT_URL/agent/run")"
check "metrics with valid token" 200 "$(status -H "X-Nyx-Service-Token: $TOKEN" "$AGENT_URL/metrics")"
check "unauthorized body" '{"detail":"unauthorized"}' "$(curl -s -m 5 "${run_request[@]}" "$AGENT_URL/agent/run")"

sessions_status="$(status -H "X-Nyx-Service-Token: $TOKEN" -H 'X-Nyx-User-Id: 1' "$AGENT_URL/agent/sessions")"
if [[ "$sessions_status" == "401" ]]; then
  printf 'FAIL sessions with token and user id returned 401\n' >&2
  failures=$((failures + 1))
else
  printf 'ok   sessions with token and user id (%s)\n' "$sessions_status"
fi

if (( failures > 0 )); then
  printf '%d service-token check(s) failed\n' "$failures" >&2
  exit 1
fi
printf 'agent service-token checks passed\n'
