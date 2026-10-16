# Tasks

## 1. Logging setup and verification

- [x] 1.1 Configure process logging at agent startup (`backend/agent_py/agent/main.py`, before uvicorn starts): root handler at WARNING with timestamp/level/logger format; `agent` logger at INFO; idempotent on repeated setup. Placed at module import (the compose entrypoint `python -m uvicorn agent.main:app` never executes `__main__`; import still precedes uvicorn in both entrypaths). Verify: unit tests via `caplog` — startup INFO names the host when tracing is enabled, zero Langfuse lines when disabled, root/`agent` levels and format as designed (`cd backend/agent_py && .venv/bin/python -m pytest -q`).
- [x] 1.2 Commit a live smoke (new `tests/test_langfuse_failure_visibility.py`, following the existing live-smoke pattern): boot the agent app with `LANGFUSE_ENABLED=true`, dummy keys, and an unreachable host (e.g. `127.0.0.1:9`); drive one run; assert the startup line and a first WARN-or-above export diagnostic naming the host appear within a bounded timeout; assert disabled produces no Langfuse lines; assert uvicorn lines are not duplicated (covered by two real subprocess boots: `python -m agent.main` and `python -m uvicorn agent.main:app`). Verify: the smoke runs green locally (5 passed); full agent suite green with and without `TEST_ASYNC_PG_DSN` (bare 395 passed/141 skipped; DSN 535 passed/1 skipped).

## 2. Documentation

- [x] 2.1 Update `backend/agent_py/README.md`: tracing state/failures are visible by default; `LANGFUSE_DEBUG=true` is a process-environment lever (explicitly noting the agent's `.env` does not populate `os.environ`, which the SDK reads). Integrated into existing sections, not EOF; `README.zh.md` mirrored for doc parity. Verify: diff reviewed; the caveat sentence is present.
