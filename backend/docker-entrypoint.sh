#!/bin/sh
# docker-entrypoint.sh — generic `_FILE` env bridge for the backend.
#
# The backend reads `DB_URL` and `JWT_SECRET` directly from the
# environment (it has no _FILE support yet — tracked as a follow-up).
# Some orchestrators mount secrets as files (Docker Compose `secrets:`,
# k8s `secretKeyRef` with a `volumes:` mount instead of `env:`, certain
# sidecar patterns) and then point an env var at the file path. This
# shim closes that gap: for each known secret, if `/run/secrets/<name>`
# exists and is non-empty it is exported into the named env var before
# the backend starts; otherwise the env var is left untouched so the
# config loader's fail-closed check (empty / default / short) fires
# normally.
#
# Today this shim is a no-op in every deployment path we ship:
#   - dev compose (`docker-compose.yml`) does not mount the secrets
#     block; operators supply DB_URL / JWT_SECRET via `.env`.
#   - production runs on Kubernetes (`k8s/`); secrets are inlined into
#     pod env via `secretKeyRef` (see `k8s/backend-deployment.yaml`),
#     and `/run/secrets/*` is not mounted.
# It is wired as the ENTRYPOINT in `backend/Dockerfile` so any future
# orchestrator (or a return to compose-file secrets) gets the bridge
# for free, and so SECURITY.md M9's evidence — that secret values never
# appear as env literals in the image or in `docker inspect` — still
# holds whenever the file-mount pattern is used.
#
# SECURITY.md M9: the bridge this script provides. Kept as the
# authoritative answer to "where do mounted secrets get consumed?"
# even while every current deployment skips it.
#
# Behaviour:
#   - If /run/secrets/<name> exists and is non-empty, export
#     <env>=$(cat ...).
#   - If /run/secrets/<name> exists but is empty, leave the env
#     alone (the config loader rejects empty DB_URL and short /
#     default JWT_SECRET, so the container fails closed at boot).
#   - If /run/secrets/<name> does NOT exist, leave the env alone —
#     the orchestrator is expected to have set it.
#   - The final `exec "$@"` replaces the shell process with the
#     backend binary so it receives PID 1 and signals (SIGTERM,
#     SIGINT) propagate correctly.
#
# `set -e` is intentionally omitted at the top level — we want to
# keep going through unset-env checks rather than abort on the
# first missing file. The only fatal exit is the final `exec`,
# which is the standard "binary not found" / "binary not
# executable" path.

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