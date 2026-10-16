# Proposal

## Why

With `PYTHON_AGENT_LANGFUSE_ENABLED=true` and an unreachable `PYTHON_AGENT_LANGFUSE_HOST`, the agent gives the operator zero feedback — no startup confirmation that tracing is on, no export-failure diagnostics (bugs.md 部署①). The export path is intentionally best-effort (spec: warn and continue), but the process configures no root log handler (uvicorn configures only `uvicorn.*`), so the agent's own INFO line is dropped and only bare, unprefixed lines from the OpenTelemetry exporter ever surface via `logging.lastResort`. The defect is observability of a working-as-designed degradation, not the degradation itself.

## What Changes

- **Process logging at agent startup**: a root handler with timestamp/level/logger formatting at WARNING, and the `agent` logger at INFO — making the existing "langfuse tracing enabled (host=…)" line and the exporter's retry/drop diagnostics visible in `/tmp/nyx-agent.log` under the default configuration. Third-party INFO noise (httpx etc.) stays suppressed.
- **Documentation**: `LANGFUSE_DEBUG=true` documented as the SDK-level verbosity lever, explicitly as a process-environment variable (the agent's `.env` does not populate `os.environ`, which the SDK reads).
- No startup network probe, no `/healthz` change, no request-path behavior change, no new dependency.

No breaking changes; the SSE stream and run outcomes are untouched (best-effort contract unchanged).

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `agent-llm-tracing`: adds an operator-visibility requirement — the enabled state is announced at startup and export-failure diagnostics are visible under the default logging configuration, without changing the best-effort delivery contract.

## Impact

- `backend/agent_py/agent/main.py` (logging setup before uvicorn starts); `backend/agent_py/README.md`.
- Tests: unit tests via `caplog` for the logging configuration; a committed live smoke booting the agent with an unreachable host asserting the startup line and a first export diagnostic appear within a bounded window (and that disabled tracing stays quiet).
- Rollout: none foreseen — every task completes in the dev/CI environment.
