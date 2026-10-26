#!/usr/bin/env bash

AGENT_TOKEN_MIN_BYTES=32

agent_token_is_valid() {
  local LC_ALL=C
  [[ -n "${1:-}" && ${#1} -ge $AGENT_TOKEN_MIN_BYTES ]]
}

agent_token_from_env_file() {
  local file="${AGENT_TOKEN_ROOT:-.}/backend/.env"
  [[ -f "$file" ]] || return 1
  local value
  value="$(sed -n 's/^[[:space:]]*\(export[[:space:]][[:space:]]*\)\{0,1\}AGENT_SERVICE_TOKEN=//p' "$file" | tail -n 1)"
  value="${value%$'\r'}"
  value="${value%%[[:space:]]#*}"
  value="${value%"${value##*[![:space:]]}"}"
  value="${value%\"}"
  value="${value#\"}"
  value="${value%\'}"
  value="${value#\'}"
  [[ -n "$value" ]] || return 1
  printf '%s' "$value"
}

resolve_agent_service_token() {
  local candidate="${AGENT_SERVICE_TOKEN:-}"

  if [[ -n "$candidate" ]]; then
    if ! agent_token_is_valid "$candidate"; then
      echo "agent-token: exported AGENT_SERVICE_TOKEN is shorter than $AGENT_TOKEN_MIN_BYTES bytes" >&2
      return 1
    fi
    printf '%s' "$candidate"
    return 0
  fi

  if candidate="$(agent_token_from_env_file)"; then
    if ! agent_token_is_valid "$candidate"; then
      echo "agent-token: AGENT_SERVICE_TOKEN in backend/.env is shorter than $AGENT_TOKEN_MIN_BYTES bytes" >&2
      return 1
    fi
    printf '%s' "$candidate"
    return 0
  fi

  echo "agent-token: AGENT_SERVICE_TOKEN is not configured — refusing to generate one." >&2
  echo "agent-token: the Go backend and the Python agent both read it from backend/.env; set it there once:" >&2
  echo "agent-token:   printf 'AGENT_SERVICE_TOKEN=%s\n' \"\$(openssl rand -hex 32)\" >> backend/.env" >&2
  return 1
}

export_agent_service_token() {
  local token
  if ! token="$(resolve_agent_service_token)"; then
    return 1
  fi
  export AGENT_SERVICE_TOKEN="$token"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  AGENT_TOKEN_ROOT="${AGENT_TOKEN_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
  export AGENT_TOKEN_ROOT
  resolve_agent_service_token
fi
