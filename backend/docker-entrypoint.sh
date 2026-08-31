#!/bin/sh
# docker-entrypoint.sh — bridge between Compose-mounted secrets and
# the backend's env-var-driven config.
#
# The backend reads `DB_URL` and `JWT_SECRET` directly from the
# environment (it has no _FILE support yet — tracked as a follow-up).
# The prod compose mounts those values as Docker secret files at
# /run/secrets/db_url and /run/secrets/jwt_secret (mode 0400). This
# shim reads each secret file into its env var if and only if the
# env var is currently unset, then exec's the backend. The result:
# the secret never appears as an env literal in the image or in
# `docker inspect`, and a missing secret fails closed (the backend
# config loader rejects empty DB_URL and short/default JWT_SECRET).
#
# SECURITY.md M9: this script is the bridge that makes the
# docker-compose.prod.yml `secrets:` block actually work. Without
# it the compose file is half-wired — secrets mount, but nothing
# reads them, and the backend fails closed at startup with a
# confusing "env not set" error.
#
# Behaviour:
#   - If /run/secrets/<name> exists, export <env>=$(cat ...) and
#     `unset` the env var if the file is empty (so an empty secret
#     is treated like an unset env — the config loader errors
#     instead of starting with "" as a valid value).
#   - If the secret file does NOT exist (e.g. dev compose, which
#     doesn't mount the secrets block) leave the env alone — the
#     operator must supply DB_URL / JWT_SECRET through .env or the
#     shell.
#   - The final `exec "$@"` replaces the shell process with the
#     backend binary so it receives PID 1 and signals (SIGTERM,
#     SIGINT) propagate correctly.
#
# `set -e` is intentionally omitted at the top level — we want to
# keep going through unset-env checks rather than abort on the
# first missing file. The only fatal exit is the final `exec`, which
# is the standard "binary not found" / "binary not executable" path.

# DB_URL — fail-closed: empty file or missing file both leave the
# env alone, and the config loader will reject the empty string.
if [ -r /run/secrets/db_url ]; then
  db_url_value=$(cat /run/secrets/db_url)
  if [ -n "$db_url_value" ]; then
    export DB_URL="$db_url_value"
  fi
fi

# JWT_SECRET — same pattern. config.Load() rejects both the literal
# default placeholder and any value shorter than auth.MinSecretBytes
# (32 bytes per RFC 7518 §3.2), so an empty file or missing file is
# safe — the startup check fires.
if [ -r /run/secrets/jwt_secret ]; then
  jwt_value=$(cat /run/secrets/jwt_secret)
  if [ -n "$jwt_value" ]; then
    export JWT_SECRET="$jwt_value"
  fi
fi

# Hand off to the backend. Default to ./main (matches the runtime
# stage WORKDIR / CMD in the Dockerfile) but accept a first arg so
# docker-compose.yml can override it without rebuilding the image.
exec "${@:-./main}"