# Design

## Context

See proposal.md — Why. Verified state (2026-10-16): `agent/main.py:1146` runs uvicorn with `log_level="info"`, and uvicorn's `LOGGING_CONFIG` configures only `uvicorn.*` loggers — no root handler exists, so `agent.*` INFO lines are dropped and unprefixed otel-exporter lines surface via `logging.lastResort`. The exporter retries then drops with WARN/ERROR inside the OTel batch processor (suppressed-return contract); the agent wrappers are log-and-continue. `LANGFUSE_DEBUG` is read by the Langfuse SDK directly from `os.environ` (`langfuse/_client/client.py:374-381`), and pydantic-settings does not mirror the agent's `.env` into `os.environ` — a `.env` entry alone would not enable it.

## Goals / Non-Goals

**Goals:** the existing degradation signals become visible under the default configuration, with the operator able to identify the host; zero behavior change to runs or the SSE path.

**Non-Goals:** a startup connectivity probe (rejected below), healthz changes, alerting/monitoring integration, changing the best-effort contract, log shipping.

## Decisions

### D1 — Root handler at WARNING + package logger at INFO

At agent startup (before `uvicorn.run`), configure `logging.basicConfig(level=WARNING, format="%(asctime)s %(levelname)s %(name)s: %(message)s")` and raise the `agent` package logger to INFO. Rationale: root at WARNING keeps third-party INFO chatter (httpx request logging) suppressed while letting the otel exporter's WARN/ERROR retry/drop diagnostics render with full formatting; the `agent` logger at INFO surfaces the existing startup line plus future operational lines without touching the many WARN logs already in place.

- Alternative rejected: root at INFO — floods dev logs with httpx/urllib3 request lines.
- Alternative rejected: per-logger configuration only — leaves the otel exporter records on the bare `lastResort` path, which is precisely the unreadable output being fixed.
- Uvicorn interplay: uvicorn's loggers carry `propagate=False` with their own handlers, so the root addition does not duplicate access/startup lines — pinned by the smoke test in 1.2.

### D2 — No startup probe, no healthz change

Rejected: calling `client.auth_check()` at boot or exposing it on `/healthz`. A boot-time network dependency contradicts the existing posture (host deliberately unvalidated — `config.py:410-411`), adds latency to every start, and dilutes `/healthz`'s Redis-only contract. Visibility of the first export cycle (D1) is sufficient for the operator to diagnose; probing remains possible manually.

### D3 — `LANGFUSE_DEBUG` documented, not enabled

The SDK-level verbose mode stays off by default; the README documents it as a process-environment lever (with the `.env` caveat from Context). No agent-side flag is added — the default configuration is now informative enough.

### D4 — Verification

Unit (`caplog`): configuration applied (root WARNING + `agent` INFO + format), startup INFO names the host when enabled, zero Langfuse lines when disabled, repeated setup is idempotent. Live smoke (committed, bounded): boot the agent with enabled + dummy keys + unreachable host, drive one run, assert the startup line and a first WARN-or-above export diagnostic naming the host within a bounded window; assert disabled produces none; assert no duplicated uvicorn lines.

## Risks / Trade-offs

- [Duplicate lines if a future deploy configures root logging again] → `basicConfig` is a no-op when root already has handlers; the chosen placement is documented in the README section this change adds.
- [Exporter retry storms on a long outage flood the log] → OTel's batch cadence plus its `DuplicateFilter` bound the repetition; `LANGFUSE_DEBUG` stays opt-in for deeper detail.
- [Test brittleness asserting log text] → assertions target level + host substring + presence, not full message text; the live smoke polls with a timeout instead of sleeping fixed durations.

## Migration Plan

Logging-only change; no data, no migration. Rollback = revert (log lines disappear; behavior identical). No rollout gates.
