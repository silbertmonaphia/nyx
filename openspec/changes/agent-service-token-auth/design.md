# Design

## Context

See `proposal.md` — Why. The current Python service has no inbound authentication: `X-Nyx-User-Id` is parsed but never authenticated, and the docs state the compose/k8s network is the trust boundary. The agent is now a public Render web service, so that boundary is gone. The Go backend is the only intended caller: it already forwards the verified user id and the user's `Authorization` bearer as a passthrough, and it serves the SPA; the browser never calls the agent directly.

Two Go request builders cover every agent hop: `AgentProxy.Run` (`/agent/run`, SSE) and `SessionsProxy.Forward` (all `/agent/sessions*` routes and `/agent/approvals`). On the Python side all 11 routes are included into one FastAPI app; `/healthz` is probed by Render, Docker, compose, and the side-stack harness, and `/metrics` is also served by a separate Prometheus exporter on port 9001.

## Goals / Non-Goals

**Goals:**
- A public agent rejects every caller except the Go backend, with no insecure local default.
- One credential shared by both runtimes, simple enough to operate and rotate on Render.
- Fail closed where each side needs the credential: Python refuses to boot without it; Go keeps the API up but fails every agent route closed.
- Keep the existing user-JWT passthrough unchanged and unlogged.
- Dev, eval, side-stack, and e2e harnesses keep working without a committed secret.

**Non-Goals:**
- HMAC request signing, mTLS, or a custom identity provider.
- Per-caller identities or scoped tokens (one Go deployment, one credential).
- Changing the SSE wire envelope or the agent's handler-level 400 semantics.
- Provisioning the agent service in `render.yaml` (dashboard-managed deployment).
- Zero-downtime dual-token rotation (documented ordered rotation instead).

## Decisions

### Static shared secret in a dedicated header

`X-Nyx-Service-Token` carries the value of `AGENT_SERVICE_TOKEN` on every Go-to-Python request. The Python middleware compares it with `hmac.compare_digest` on encoded bytes — the `str` overload raises `TypeError` on non-ASCII values, which would turn a hostile header into a 500. Chosen over HMAC signing and mTLS because TLS already protects transport, the threat is unauthenticated public callers rather than captured-request replay, and both alternatives add substantial cross-runtime complexity and certificate/clock operations for a two-service system. The header is separate from `Authorization`, which continues to carry the end-user JWT to downstream MCP/RAG bridges.

### One unprefixed env-var name for both runtimes

Go reads `AGENT_SERVICE_TOKEN` via viper. Python normally prefixes settings with `PYTHON_AGENT_`, but a single shared credential is clearer as one name; the field uses `validation_alias="AGENT_SERVICE_TOKEN"` with `populate_by_name=True`, verified against the project venv (the alias wins over any prefixed name). This also lets compose pass one `backend/.env` value to both services and avoids two names drifting.

### One global middleware, with an exact health exemption

A `ServiceTokenMiddleware` wraps the whole app in `create_app`, added after the rate-limit middleware so it is outermost: unauthenticated traffic is rejected before it consumes rate-limit state. Only the exact path `/healthz` is exempt, because Render's health check cannot attach custom headers; `/metrics` on the FastAPI port is gated (the 9001 exporter is outside FastAPI). A global middleware, rather than a per-route dependency, makes new routes fail closed by default. Startup validation lives in the pydantic settings model, so `agent.main` fails at import when the token is absent — no insecure window.

### Fail-closed on both sides

Python rejects a missing or short token unconditionally — a public service must never run unauthenticated. Go does not take the whole API down for an agent-only misconfiguration: `cmd/api/main.go` validates the token before wiring the proxy and, when the token is missing/short (or `PYTHON_AGENT_URL` is unset), builds a Service that answers every agent-backed route 503 with a static detail while auth/feeds/MCP keep running. It never falls back to the single-turn path, because that would hide the misconfiguration. The key must be added to the explicit `bindEnvVars()` allowlist — `viper.AutomaticEnv()` alone ignores keys viper has not registered. `auth.MinSecretBytes` (32) is reused for the Go minimum so the rule matches `JWT_SECRET`.

### Token threading through the two Go builders

`AgentProxy` and `SessionsProxy` each gain a `serviceToken` field and constructor parameter; `newSessionsProxyFromService` copies it from the agent proxy, so approvals get it through the same `Forward` path. An empty token is rejected in both constructors as defense in depth. This keeps one configuration surface (`AGENT_ENABLED`, `PYTHON_AGENT_URL`, `AGENT_SERVICE_TOKEN`) and makes a half-configured agent path impossible.

### Persisted local token for dev, eval, and e2e

`scripts/lib/agent-token.sh` resolves the token in this order: exported env, an `AGENT_SERVICE_TOKEN=` assignment parsed from `backend/.env`, a gitignored `backend/agent_py/.service-token`, then generates one with `openssl rand -hex 32` (mode 600). Persisting the generated value keeps separately started backend and agent processes in agreement across runs. Compose needs no new `environment:` entry because both services already read `backend/.env`; Python consumes the unprefixed name.

### Keep the SSE 401 behavior, document it

The Go SSE handler writes HTTP 200 and SSE headers before contacting the agent. If the service token mismatches, Python's 401 JSON is copied into the already-open stream rather than surfaced as an HTTP 401. This is fail-closed and already implied by the byte-for-byte forwarding contract; the change pins the behavior with an integration test and documents the symptom instead of altering the SSE envelope. Sessions and approvals are plain JSON and surface 401 normally.

## Risks / Trade-offs

- **Render health path** → Confirm the agent service health check is exactly `/healthz` before deploying the enforcing Python image; a `/` health path would receive 401 and the deploy would never become healthy.
- **Mixed-token window** → Deploy Go first (old Python ignores the header), then Python. Rotation follows the same order; rollback is Python first, then Go. Recorded in the rollout change.
- **Token leakage** → Never log either credential; middleware logs nothing; scripts print statuses only; docs use placeholders; gitleaks protects commits.
- **Local dev breakage** → Existing `backend/.env` files lack the token. The resolver generates and persists one; agent-only mode must not source all of `backend/.env` (compose hostnames would leak).
- **Import-time failure in tests** → `agent/main.py` builds the app at import; `tests/conftest.py` sets a test token before collection. The eval suite is separate: `evals/tests/test_stub.py` constructs `Settings(...)` directly and the CI `Eval unit gate` sets no token, so it needs an explicit value or env.
- **Byte vs character length** → Go counts bytes, Python counts characters; document ASCII hex/base64 generation (`openssl rand -hex 32`) so the two never disagree.
- **`/healthz` remains public** → It pings Redis and the LLM provider per request; required by Render's probe, accepted as a minor public load/information surface.
- **Single-token rotation** → No overlap window; the ordered rotation briefly rejects the side still on the old value. Acceptable for a two-service system; dual-token support is future work.

## Migration Plan

1. Generate one token (`openssl rand -hex 32`); configure `AGENT_SERVICE_TOKEN` with the same value on the Go service and the agent service.
2. Confirm the agent's health-check path is `/healthz`.
3. Deploy Go; verify chat still works against the unchanged Python image.
4. Deploy Python; verify non-token callers get 401, `/healthz` returns 200, and chat/sessions/approvals still work through Go.
5. Rollback: redeploy the previous Python image (ignores the header) first, then revert Go; or restore the previous token on both. Rollout evidence and the PM-owned archive live in `openspec/changes/agent-service-token-auth-rollout/`.
