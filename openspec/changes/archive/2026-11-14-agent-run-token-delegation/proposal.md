# Proposal

## Why

A chat run that outlives the SPA's access token cannot use its tools. Live incident (2026-11-14): the model called `create_feed`; the step failed with `tool authentication failed`, while the surrounding llm steps succeeded. The backend log shows every `POST /mcp` rejected `401` with `error: "token expired"`.

Mechanism: the chat handler forwards the caller's `Authorization: Bearer <access_token>` verbatim to Python, which constructs `ToolsClient`/`RagClient` once per run and keeps that snapshot for the whole run. Go's `/mcp` and `/internal/rag/retrieve` re-validate the token, and access tokens have a 15-minute TTL (`JWT_ACCESS_TTL`). Runs routinely outlive 15 minutes: planner + executor latency on a small model, a 300-second approval pause, detached runs after an SSE disconnect (600-second cap), and re-attach. The run in the incident started at 18:23 and made its first tool call at 18:30 — the token was dead.

The executor classifies `ToolAuthError` non-retryable (correct: retrying the same expired credential cannot succeed), so the failure is terminal for the step and surfaces to the user.

## What Changes

- The Go chat handler mints a **per-run delegated access token** (same `JWT_SECRET`, same verified user id/username claims, TTL default `1h`, configurable via `AGENT_RUN_TOKEN_TTL`) and forwards that token to the Python agent instead of the caller's short-lived token.
- `/mcp` and `/internal/rag/retrieve` keep validating the same way — no auth middleware, Python client, or wire-frame change.
- `AGENT_RUN_TOKEN_TTL` is validated at startup and must exceed `LLM_MAX_STREAM_DURATION` when both the LLM and agent paths are enabled.
- Documentation records the sizing rule (`> stream cap + detached-run cap`), the containment of the delegated token, and the accepted residual risk (stateless token survives logout until `exp`, as existing access tokens do).

**Non-change:** no scope-claim enforcement (deferred, recorded as a follow-up); no agent-side refresh-on-401 (the agent holds no refresh token, and sharing the rotating refresh token with a second client would trip reuse detection and log the user out); no browser-visible credential change.

## Capabilities

### New Capabilities

- None.

### Modified Capabilities

- `chat-agent-proxy`: the outbound bearer to the Python service is a freshly minted per-run delegated token rather than the caller's token (the service token and logging rules are unchanged).
- `agent-python-service`: the passthrough bearer carries the delegated per-run token that the MCP/RAG bridges re-verify.

## Impact

- **Code**: `backend/internal/platform/config/config.go` (new config key), `backend/cmd/api/main.go` (mint service construction), `backend/internal/chat/{service,handler,run_token,errors}.go`, tests under `backend/internal/chat/` and `backend/internal/platform/config/`.
- **Not touched**: Python agent code/tests, MCP server, RAG bridge, migrations, sqlc, OpenAPI schema, frontend runtime code.
- **Security surface**: a full-user access token with a longer TTL exists for the duration of a run. It travels only Go→agent (HTTPS-enforced for public hosts), is never returned to the browser and never logged. Scope-claim restriction to `/mcp` + RAG is a documented follow-up.
- **Rollout gates**: none. The change is Go-only, has no migration, and both new and old pods interoperate (old pods forward the caller's token; MCP/RAG accept both). Rollback is a handler/service/wiring revert.
