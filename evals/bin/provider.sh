#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PYTHON="${EVAL_AGENT_PYTHON:-$ROOT/backend/agent_py/.venv/bin/python}"

exec "$PYTHON" "$ROOT/evals/providers/agent_run.py" "$@"
