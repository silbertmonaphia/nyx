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

read_persisted_token() {
  local persisted="$1"
  local value
  value="$(tr -d '\r\n' < "$persisted")"
  if ! agent_token_is_valid "$value"; then
    echo "agent-token: $persisted is shorter than $AGENT_TOKEN_MIN_BYTES bytes" >&2
    return 1
  fi
  printf '%s' "$value"
}

resolve_agent_service_token() {
  local root="${AGENT_TOKEN_ROOT:-.}"
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

  local persisted="$root/backend/agent_py/.service-token"
  if [[ -f "$persisted" ]]; then
    read_persisted_token "$persisted"
    return $?
  fi

  if ! candidate="$(openssl rand -hex 32 2>/dev/null)" || ! agent_token_is_valid "$candidate"; then
    echo "agent-token: failed to generate a token with openssl" >&2
    return 1
  fi
  if ( umask 077; set -C; printf '%s\n' "$candidate" > "$persisted" ) 2>/dev/null; then
    chmod 600 "$persisted" 2>/dev/null || true
    printf '%s' "$candidate"
    return 0
  fi
  if [[ ! -f "$persisted" ]]; then
    echo "agent-token: failed to persist $persisted" >&2
    return 1
  fi
  read_persisted_token "$persisted"
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
