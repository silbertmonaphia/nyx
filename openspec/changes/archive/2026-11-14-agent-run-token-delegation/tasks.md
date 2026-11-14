# Tasks

## 1. Config

- [x] 1.1 Add `AgentRunTokenTTL` (`AGENT_RUN_TOKEN_TTL`) to `backend/internal/platform/config/config.go`: struct field, `setDefaults` (`1h`), `bindEnvVars` list, and `validateDurations` (positive; greater than `LLM_MAX_STREAM_DURATION` when LLM + agent are enabled).
- [x] 1.2 Tests in `config_test.go`: default `1h`, env honoured, unparseable rejected, non-positive rejected, below stream duration rejected.

## 2. Chat service and handler

- [x] 2.1 New `backend/internal/chat/run_token.go`: `RunTokenMinter` interface and `(*Service).AgentRunToken(ctx)` minting from `reqctx` user id/username; `ErrAgentRunToken` sentinel in `errors.go` for nil minter, zero user id, and mint failures.
- [x] 2.2 `NewServiceWithProxy` takes a required minter; `NewServiceWithMisconfiguredAgent` passes nil. Update all call sites.
- [x] 2.3 `streamProxy` forwards `"Bearer "+minted` instead of the inbound header; on mint failure calls `api.ClassifyAndLog` and emits the existing `agent_unavailable` envelope with no upstream call. Update the proxy/approvals stale passthrough comments.

## 3. Wiring

- [x] 3.1 `mountChatRoute` parses `AGENT_RUN_TOKEN_TTL`, constructs `auth.NewTokenService([]byte(cfg.JWTSecret), runTTL)`, passes it to `NewServiceWithProxy`, logs the TTL only.

## 4. Backend tests

- [x] 4.1 `run_token_test.go`: claims from context, nil minter, zero user id.
- [x] 4.2 Invert the handler-level verbatim assertion in `agent_proxy_integration_test.go`: outbound bearer is minted, validates, same user claims, later `exp`, body still byte-for-byte; harnesses pass a real 1h `TokenService`.
- [x] 4.3 Mint failure → typed `agent_unavailable` frames + zero upstream calls; run token absent from captured logs.
- [x] 4.4 Update harness signature call sites (`handler_llm_resolution_test.go`, `sessions_proxy_test.go`, `approvals_proxy_test.go`, `runs_proxy_test.go`); keep proxy-level transport tests unchanged.

## 5. Docs

- [x] 5.1 `backend/.env.example`: document `AGENT_RUN_TOKEN_TTL` with the sizing rule; correct the "forwards the user's Authorization" wording.
- [x] 5.2 `docs/agent-deployment.md`: optional env-table row + "tool authentication failed = expired run token" gotcha + Go-only rollout/rollback note.
- [x] 5.3 `SECURITY.md`: delegated-token containment, stateless-logout window, deferred scope follow-up.

## 6. E2E

- [x] 6.1 `scripts/e2e-side-stack.sh`: conditional `JWT_ACCESS_TTL` export from `E2E_JWT_ACCESS_TTL`; header docs.
- [x] 6.2 New `frontend/tests/e2e/chat-run-token-expiry-live.spec.ts` (`@live`): capture the `POST /api/chat` bearer, wait past its `exp`, approve the card, assert the feed lands; explicit skip unless the stack's access TTL is ≤120s.
- [x] 6.3 Red-first run against pre-fix code, then green after the fix; re-run `chat-feed-approval-live.spec.ts` on the same side stack as the happy-path gate. Evidence (2026-11-14, host vLLM Qwen3-4B + side stack with `E2E_JWT_ACCESS_TTL=90s`): new spec green in 1.7m; `chat-feed-approval-live` green in 6.5s; the same spec against the pre-fix HEAD backend (:8181, same agent/MCP) failed at the `feed-list` assertion with `POST /mcp 401 token expired` in the backend log.

## 7. Gates

- [x] 7.1 `go build ./...`, `SKIP_CONTAINERS=true go test ./...`, `gofmt -l`, `golangci-lint run --timeout=5m`.
- [x] 7.2 `npm run lint` for the new spec.
