#!/usr/bin/env bash
set -euo pipefail
cd "${1:-/opt/nyx}"
docker compose --profile app up --build -d --remove-orphans --wait --wait-timeout 900
docker compose --profile app ps
docker image prune -f
