# Proposal

## Why

A publicly reachable instance with open registration and an unrestricted AI chat falls inside the scope of China's generative-AI service rules, which have no filing route for an individual operator. The same codebase also serves an overseas instance that should stay fully open. The deployment therefore needs server-enforced switches that can close or narrow registration and restrict AI surfaces on one instance without touching the other, and without changing defaults for existing deployments, local development, or CI.

## What Changes

- Add `REGISTRATION_ENABLED` (default `true`) and `REGISTRATION_ALLOWLIST` (comma-separated usernames, default empty = anyone) to the backend config. Registration succeeds only when enabled and (the list is empty or the submitted username is listed). Both failure modes return one identical 403 with a static message; the check runs before bcrypt.
- Add `AI_ALLOWLIST` (comma-separated usernames, default empty = every authenticated user). When non-empty it gates `/api/chat*`, `/api/chat/approvals`, and `/api/me/llm-config*` with a 403 through chi and huma middleware that run after auth. `/api/features` is never gated.
- Add `GET /api/config` (anonymous) returning `{"registration_mode": "open" | "invite_only" | "closed"}`; it never exposes usernames or allowlist contents.
- Extend `GET /api/features` with `chat_allowed` for the authenticated caller: `LLM_ENABLED && (AI_ALLOWLIST empty || caller listed)`.
- SPA: hide the register toggle and explain when registration is closed; keep the form with an invite hint when invite-only; map a register 403 to a dedicated inline error. Hide the chat button, panel, LLM gear, config modal, and Knowledge tab when `chat_allowed` is false, and defer the chat auto-open until the features probe settles. Both probes fail open when unavailable (the server still enforces).
- Validate allowlist entries at boot (3-50 characters, no empty segments, no control characters); validate and document that changes take effect on restart.
- Support `EVAL_USERNAME` in the golden-eval provider so evals can run against a gated stack by allowlisting a fixed username.
- Docs and env templates across `backend/.env.example`, deployment docs, READMEs, `CLAUDE.md`, and `FUTURE*.md`.

## Capabilities

### New Capabilities

- `access-gates`: per-deployment registration and AI access policy — mode derivation, identical 403 semantics, route coverage, the anonymous config probe, `chat_allowed`, SPA affordances, fail-open behavior, and config validation.

## Impact

- Code: new `backend/internal/access/` (policy + public config handler), `backend/internal/middleware/ai_allowlist.go`, `backend/internal/platform/config/config.go`, `backend/internal/user/` (sentinel + service policy), `backend/internal/features/`, `backend/internal/chat/` route registration, `backend/internal/llmsettings/` middleware, `backend/cmd/api/main.go`, `backend/cmd/openapi/`.
- API: new `GET /api/config`; `GET /api/features` gains `chat_allowed`. OpenAPI spec and generated frontend types regenerated.
- Frontend: `usePublicConfig` + `publicConfigService`, `useFeatureFlags`/`useChatAllowed`, `AuthForm` affordances, `App` gating, i18n, unit + e2e tests (route-stub spec in the default suite, an opt-in live spec against a gated side stack).
- Evals: optional `EVAL_USERNAME` override.
- No database migration, no new table, no per-user persisted flag; changes are deployment env read at boot.
- Rollout: tracked in the sibling `access-gates-rollout` change (production env flips, smoke, rollback).
