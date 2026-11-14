# Design

## Context

The Go chat handler is a thin SSE proxy to the Python orchestrator. Go verifies the caller's JWT at the SPA edge, stamps the verified user id as `X-Nyx-User-Id`, authenticates the hop with `AGENT_SERVICE_TOKEN`, and forwards the caller's `Authorization` header as a passthrough. Python never decodes the JWT; it re-presents the bearer to Go's `/mcp` and `/internal/rag/retrieve`, which re-verify it.

That makes the run's tool and RAG authority depend on the caller's access token remaining valid — a 15-minute window — while a run's lifetime is bounded by different, mostly longer clocks.

## Decisions

### D1 — Mint once at run start, in Go, from the same secret

The delegated token is an ordinary HS256 access token with the same `iss`/`aud`/`type:"access"` and the same `user_id`/`username` claims, signed by a `TokenService` constructed with `JWT_SECRET` and `AGENT_RUN_TOKEN_TTL`. Validation is unchanged everywhere; a second `TokenService` instance with a different TTL is safe because validation only checks the secret and claims.

Eager minting (at `POST /agent/run`) is preferred over lazy minting on `401`: the run gets one deterministic credential for its whole life, including detached execution and approval pauses, with no callback endpoint and no refresh state to coordinate across replicas.

### D2 — Seam: `chat.Service`, not the transport

`Service` already owns the agent path (`proxy`, `MaxStreamDuration`) and imports `reqctx`. `AgentProxy` stays a dumb transport that forwards whatever header it is handed — its "do not re-mint" contract remains true and its transport tests stay valid. `NewServiceWithProxy` gains a required `RunTokenMinter` parameter; the misconfigured-agent constructor passes nil and never streams, so `AgentRunToken` returns a sentinel error instead of panicking.

A narrow `RunTokenMinter` interface in `chat` avoids widening `auth.TokenService` (which would break the middleware test stubs and invite arbitrary-TTL minting elsewhere).

### D3 — TTL sizing and validation

`AGENT_RUN_TOKEN_TTL` default `1h`. The credential must cover:

- `LLM_MAX_STREAM_DURATION` (default 10m) for the attached stream;
- `PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS` (default 600s) when the client disconnects and the run continues;
- `PYTHON_AGENT_APPROVAL_TTL_SECONDS` (default 300s) pauses inside the run.

1h leaves headroom over the ~20–30m envelope. Startup rejects a non-positive value, requires it to exceed `JWT_ACCESS_TTL` (a delegated token must not be weaker than the one it replaces), and — when both the LLM and agent paths are enabled — requires it to exceed `LLM_MAX_STREAM_DURATION` plus a 10m detached-run envelope constant, so raising the stream cap cannot silently reintroduce the bug. The Python detach cap is operator-tunable and not visible to Go; the constant floors the default (600s) and the docs state the full formula.

### D4 — No scope claim in this change

Enforcing a `scope:"agent_run"` claim would require every auth consumer (`NewAuth`, the huma adapter, feed/user/llmsettings/KB routes) to reject scoped tokens — a cross-cutting change with its own blast radius. An unenforced claim adds API surface with no security value. Recorded as an accepted residual risk and a follow-up. What bounds the exposure today: the token never reaches the browser, is only sent over the Go→agent hop (HTTPS-enforced for publicly routable hosts), and is never logged.

### D5 — No agent-side refresh-on-401

The agent holds no refresh token; refresh tokens are single-use and rotating with reuse detection, so a second refresher racing the SPA would revoke the family and log the user out. The executor's non-retryable classification of `ToolAuthError` is correct — retrying the same expired credential cannot succeed. Lazy on-401 minting would need a service-token-authenticated mint endpoint plus coordination across detached runs, approvals and replicas. If a run can ever outlive `AGENT_RUN_TOKEN_TTL`, that endpoint is the documented fallback.

### D6 — Rollout and rollback

Go-only. New pods mint; old pods forward the caller's token; `/mcp` and RAG accept both, so a rolling deploy never has a mixed-version gap. Rollback is reverting the handler/service/wiring hunks; the config key may remain defined but unused. In-flight runs started on old pods can still 401 after the caller's token expires — the fix applies to runs started after the new pod serves them.

## Risks

- Full-user authority for up to 1h (configurable). Same authority the agent already holds today for 15m; the delta is the window. Logout does not revoke a stateless token, exactly like existing access tokens.
- Misconfigured TTL below realistic run length reintroduces the failure; guarded by the startup check and documented.

## Testing

- Config: defaults, env override, unparseable, non-positive, below-stream-duration.
- Chat: outbound bearer is minted (not inbound), validates with the shared secret, carries the same user claims and a later `exp`; mint failure emits the typed `agent_unavailable` frames with zero upstream calls; the run token never appears in logs.
- E2E (`@live`): a side stack with `JWT_ACCESS_TTL=90s` drives create-feed through the approval card after the caller's token has expired; the feed must land (pre-fix: `tool authentication failed`, no feed).
