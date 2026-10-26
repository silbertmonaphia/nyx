# Proposal

## Why

The Python agent is now reachable from the public internet as a Render web service, but it performs no inbound authentication: every route trusts `X-Nyx-User-Id`, so anyone who knows the URL can run agent turns against the deployment's LLM credentials, read or delete arbitrary users' chat history, and export or erase GDPR data. The compose-network boundary the service was designed around no longer exists.

## What Changes

- Add a shared secret `AGENT_SERVICE_TOKEN` (at least 32 bytes) to both runtimes. The Go backend stamps `X-Nyx-Service-Token` on every agent hop: `/agent/run`, all `/agent/sessions*` routes, and `/agent/approvals`.
- Add a Python middleware that rejects any request without the correct token (constant-time comparison, HTTP 401 `{"detail":"unauthorized"}`), except the exact `/healthz` probe path.
- Fail closed at startup: Go refuses to boot when `AGENT_ENABLED=true` without a valid token; Python always refuses to boot without one. **BREAKING** for existing deployments and local `.env` files — the same token must be set on both services before rollout.
- Gate `/metrics` on the FastAPI port. The separate 9001 Prometheus exporter is outside FastAPI and stays unchanged.
- Local dev, eval, and e2e tooling resolve one persisted token so `make dev`, the side stack, and eval runs agree without a committed default secret.
- Update the affected specs, operator docs, security notes, and deployment runbook.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `chat-agent-proxy`: the proxy now stamps a service token on every Go-to-Python hop; the requirement forbidding a service-to-service token is replaced, and the 401-forwarding scenario is redefined as service-token rejection.
- `agent-python-service`: adds the service-token gate requirement (all routes except `/healthz`), modifies the verified-user-header and environment requirements, and updates the approvals trust contract.

## Impact

- Code: Go `backend/internal/platform/config/config.go`, `backend/internal/chat/{agent_proxy,sessions_proxy,approvals_proxy,handler}.go`, `backend/cmd/api/main.go`; Python `backend/agent_py/agent/{config,main,service_auth}.py` plus route trust docstrings; tests in both runtimes; `scripts/{dev.sh,e2e-side-stack.sh}`, `scripts/lib/agent-token.sh`, `scripts/e2e/agent-service-auth.sh`, `evals/providers/agent_run.py`, `Makefile`, and the CI agent job.
- Config: new `AGENT_SERVICE_TOKEN` (required when the agent path is enabled, same value on both services). No change when `AGENT_ENABLED=false`.
- Docs: agent README/README.zh/README_ALL trust models and env tables, deployment docs, `SECURITY.md`/`SECURITY.zh.md`, root READMEs, `docs_agent/AgentArchitecture.md`, `CLAUDE.md`.
- Rollout gates tracked in `openspec/changes/agent-service-token-auth-rollout/`.
