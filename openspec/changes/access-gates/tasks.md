# Tasks

## 1. Config

- [x] 1.1 `Config` fields `RegistrationEnabled` (default true), `RegistrationAllowlist`, `AIAllowlist` (defaults empty) + explicit `bindEnvVars` entries.
- [x] 1.2 `validateAccessConfigs`: entries 3-50 characters, no empty segments or control characters; runs even when LLM/KB/MCP are off.
- [x] 1.3 Config tests: defaults, env overrides, malformed-list matrix, valid lists.

## 2. Access policy + public probe

- [x] 2.1 `internal/access`: `RegistrationPolicy` with `Allows`/`Mode`, shared `Allows(list, username)`.
- [x] 2.2 `GET /api/config` (anonymous, huma) returning `registration_mode`; no usernames or list contents.
- [x] 2.3 Tests: policy table, three modes, no allowlist leak.

## 3. Registration gate

- [x] 3.1 `user.ErrRegistrationNotAllowed` → 403 sentinel; `Service.SetRegistrationPolicy`; check before password/bcrypt.
- [x] 3.2 `cmd/api` wiring: `SplitNonEmpty`, policy setter, boot log with mode + counts only.
- [x] 3.3 Tests: service matrix (nil/disabled/listed/unlisted/empty list, before bcrypt), handler 403/201/409, identical 403 bodies, no username echo.

## 4. AI gate

- [x] 4.1 `middleware.NewAIAllowlist` + `NewHumaAIAllowlist` (empty = pass-through, missing username = 401, not listed = 403).
- [x] 4.2 Wire chat group, approvals chain, and LLM-config huma ops; `/api/features` excluded.
- [x] 4.3 `features` response gains `chat_allowed` = `LLM_ENABLED && Allows(AI_ALLOWLIST, caller)`.
- [x] 4.4 Tests: middleware envelopes/ordering, chat route matrix, approvals, llm-config verbs, features matrix.

## 5. Contracts

- [x] 5.1 `cmd/openapi` registers the new handler and threads the new signatures; path assertions include `/api/config` and `/api/features`.
- [x] 5.2 Regenerate `api/openapi.json` and `frontend/src/api/openapi/schema.d.ts`.

## 6. Frontend

- [x] 6.1 `featuresService` parses `chat_allowed` (missing/non-boolean → undefined); `useFeatureFlags`/`useAgentEnabled`/`useChatAllowed` share one query.
- [x] 6.2 `publicConfigService` + `usePublicConfig` (silent, no retry, 5-minute stale, unknown → null).
- [x] 6.3 `AuthForm`: closed hides the toggle behind a hint; invite-only keeps the form with a hint; register 403 maps to a dedicated inline error; computed register mode avoids setState-in-effect.
- [x] 6.4 `App`: defer chat auto-open until the features probe settles; `chatAllowed=false` hides the chat button/panel/gear/modal and the Knowledge tab; LLM gate derives from `chatAllowed`; fail-open on probe error/missing field.
- [x] 6.5 i18n keys in `en.ts` + `zh.ts`; unit tests for services, hooks, AuthForm, App gating, i18n parity.

## 7. Evals

- [x] 7.1 `EVAL_USERNAME` override in `evals/providers/agent_run.py` (documented env list) with a provider test.

## 8. E2E

- [x] 8.1 `tests/e2e/access-gates.spec.ts` (route stubs): closed/invite-only/fail-open, register 403 inline, `chat_allowed=false` hides AI surfaces, missing field fails open.
- [x] 8.2 `tests/e2e/access-gates-live.spec.ts` (`@live`, opt-in) + side-stack env passthrough: invite-only anonymous UI, unlisted registration 403, API matrix (features/chat/llm-config/config), SPA AI surface gating for listed vs unlisted users.
- [x] 8.3 Manual closed-mode check (`REGISTRATION_ENABLED=false` side backend): `/api/config` = `closed`, `POST /api/register` = 403.

## 9. Verification

- [x] 9.1 Backend: gofmt, golangci-lint, `go test -race` (all non-temp packages, containers included), coverage gate, sqlc-diff empty, openapi-diff empty.
- [x] 9.2 Frontend: typecheck, lint, lint:arch, test:coverage (657 tests), openapi:check, build.
- [x] 9.3 E2E: route-stub spec 6/6, live gated spec 5/5 against a side stack, theme spec 9/9 on the same stack.

## 10. Docs / roadmap

- [x] 10.1 `backend/.env.example` access-gates section; `docs/deployment.md` env table + stale `/api/register` Bearer claim fixed; `docs/deployment-selfhost.md`; `docs/agent-deployment.md`; README/README.zh/README_FULL.
- [x] 10.2 `CLAUDE.md` auth bullet; `FUTURE.md`, `FUTURE_BACKEND.md`, `FUTURE_FRONTEND.md` entries ticked.
- [x] 10.3 Rollout gates split into the sibling `access-gates-rollout` change.
