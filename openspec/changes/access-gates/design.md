# Design

## Decisions

- **D1 — Deployment-level env, not geo detection.** The open and gated instances are separate deployments with their own `backend/.env`; no GeoIP, Host header, or Origin heuristics are involved. Defaults are the current permissive behavior, so local development, CI, and the overseas instance are unchanged.
- **D2 — One registration policy object.** `internal/access.RegistrationPolicy{Enabled, Allowlist}` owns matching (`Allows`) and mode derivation (`Mode`). `cmd/api` parses the raw env with `middleware.SplitNonEmpty`; the user service receives the typed policy via `SetRegistrationPolicy` (nil = allow all, keeping existing constructor call sites and tests intact).
- **D3 — Identical 403 for disabled and not-listed.** Both paths return `user.ErrRegistrationNotAllowed` → `api.RegisterSentinel(..., 403, "Registration is not available")`. The check runs before the 72-byte password check and bcrypt, so denied requests are cheap. The route stays mounted; only the body shape is static.
- **D4 — AI gate is middleware after auth.** `middleware.NewAIAllowlist` (chi) and `NewHumaAIAllowlist` (huma) read `reqctx.UsernameFromContext`. Empty list = pass-through; missing username = 401 (loud misordering); not listed = 403 static message. Applied to the chat group, approvals chain, and every LLM-config operation. `/api/features` stays open to authenticated users because it reports the decision.
- **D5 — Anonymous registration probe.** `/api/features` is Bearer-authenticated, so a new public `GET /api/config` is the only way the logged-out auth dialog can learn the registration mode. It exposes exactly one enum and no usernames.
- **D6 — SPA fails open, server enforces.** A missing/old endpoint or missing `chat_allowed` field renders the permissive UI; the backend remainder is the source of truth. This keeps rolling deploys safe in both directions. The chat auto-open waits for the (single, lightweight) features probe to settle so a denied user does not see the panel flash open and fire requests that 403.
- **D7 — No chatStore denial state.** The features probe is the single UI source of truth; a 403 after the probe is a race/stale-cache/manual-API case and flows through the existing chat error path. Duplicating policy in the store would risk stale hiding.
- **D8 — Eval compatibility.** The golden-eval provider gains `EVAL_USERNAME`; the existing login-first fallback means a fixed username works across runs once allowlisted. Defaults unchanged.
- **D9 — Validation at boot.** Allowlist entries must be 3-50 characters with no empty segments or control characters; a typo fails startup instead of silently never matching. Matching is exact and case-sensitive, aligned with the users table and JWT claims. Boot logs print the mode and list sizes, never usernames.

## Risks

- Registration membership is inherently observable (listed names can reach 201/409, unlisted get 403); the design keeps the 403 identical for disabled vs not-listed and never leaks the list, and the existing per-IP auth rate limit bounds probing.
- JWT usernames are login-time snapshots; removing a user from `AI_ALLOWLIST` denies on the next request after restart, while deleted users with live access tokens keep AI until expiry (pre-existing).
- With `LLM_ENABLED=false` the chat and BYOK routes are absent (404) and `chat_allowed` is false; direct callers see 404 rather than 403.
- A fresh database with `REGISTRATION_ENABLED=false` has no account-creation path; provision the first account first (or register via the allowlist), documented in the rollout sibling.
