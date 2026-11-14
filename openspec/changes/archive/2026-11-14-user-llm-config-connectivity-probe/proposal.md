# Proposal

## Why

A wrong BYOK credential produces zero user-visible signal. The feed embedding path is best-effort by contract (`backend/internal/feed/service.go:159-166` — "never fail the user's write because embedding failed"), so a bad key surfaces only as a server-side Warn. The OpenAI-compatible client collapses every non-429 upstream status into `ErrProviderUnavailable` (`backend/internal/llm/openai/client.go:416-421`), so even the log line reads `upstream 401: llm provider unavailable` — a hint, not a diagnosis. The save-time probe covers one failure class only: embedding **width**, and it deliberately saves anyway when the endpoint cannot be reached (`internal/llmsettings/service.go:117-128`), which swallows 401 into "couldn't probe". The main-LLM, fallback, and Tavily slots are never probed at all — the config modal's own guidance says so ("the deployment does not probe at save time").

Observed incident (2026-11-13): a user saved a SiliconFlow embedding endpoint with a wrong key, updated feeds, saw the UI report success throughout, and only found the root cause by grepping the backend log. Nothing in the product told them the key was invalid.

## What Changes

- **A new on-demand probe endpoint**, `POST /api/me/llm-config/validate`, authenticated and gated like the existing three config endpoints. It accepts the same eleven-field shape PUT takes (blank key field = use the stored key, mirroring the PUT contract), and probes **each slot the user actually filled**: main LLM (1-token chat completion), embedding (1-input `/v1/embeddings`, reusing the existing width check), fallback (1-token chat completion), Tavily (`max_results=1` search). Empty slots are reported as skipped and never error.
- **Per-slot classified results, with the request disclosed.** Each slot returns `ok` / `auth_failed` / `rate_limited` / `invalid_model` / `invalid_config` / `width_mismatch` / `unreachable` / `timeout` plus the upstream HTTP status where one exists, so a wrong key reads as "401 — authentication failed", not as a generic failure. Each result also carries the exact outbound request — method, URL, headers (credentials tail-4 masked), body — rendered collapsibly in the modal with a copy-as-curl action whose credential is an explicit placeholder, so the user can see and replay what was sent. One slot's failure does not affect the others' probes.
- **A typed upstream-status carrier in the LLM client error mapping**, so classification reads a status code structurally instead of parsing the `upstream 401:` message string. The HTTP-facing sentinel semantics (`api.RegisterSentinel`, safe-detail funnel) are unchanged.
- **A minimal Tavily probe client in Go** that speaks the exact wire shape the Python agent uses at runtime (`Authorization: Bearer` header, `X-Tavily-Access-Mode: keyless` when no key — `backend/agent_py/agent/web_tools.py:350-371`), so "probe ok" implies "runtime will authenticate". The Go backend has no Tavily client today (search runs in the agent), so this is new but tiny.
- **A per-section test-connection affordance in the config modal**: each visible section (main LLM, embedding, Tavily) carries its own button probing only that section's values, with the result rendered inline in that section, localized, independent of the save action. Sections stay independent end to end — separate providers, separate endpoints, separate failure modes — so the probe affordance follows the same shape.

**Non-change worth stating:** PUT save semantics are untouched — the width probe still blocks only on width mismatch, unreachable still saves, no slot becomes save-blocking. The probe is purely additive: it informs, it does not gate.

## Capabilities

### New Capabilities

- None.

### Modified Capabilities

- `user-llm-config`: a configured-or-submitting user can request an on-demand, per-slot connectivity probe of their BYOK values, receiving a classified result per filled slot; the config modal surfaces the probe inline before saving.

## Impact

- **Code**: `backend/internal/llm/openai/client.go` (typed status carrier), `backend/internal/llm/errors.go` (carrier type), `backend/internal/llmsettings/` (validate service + huma handler + Tavily probe client), `backend/internal/llmsettings/resolver.go` (slot-client construction reused by the probe), `backend/internal/platform/config/config.go` (`LLM_CONFIG_PROBE_TIMEOUT`, default 10s), `backend/cmd/api/main.go` (route wiring + per-user rate limiter), `frontend/src/features/chat/components/LlmConfigModal.tsx`, `frontend/src/features/chat/services/llmConfigService.ts`, `frontend/src/i18n/{en,zh}.ts`.
- **Not touched**: PUT semantics, the resolver's runtime behavior, feed indexing, migrations (no schema change — probing stores nothing), `queries/*.sql`.
- **Security surface introduced**: an authenticated endpoint that triggers up to four server-side outbound requests from user-supplied URLs. Mitigations are structural: the same SSRF transport and `BYOK_ALLOW_PRIVATE_URL` semantics as the runtime path, a bounded per-slot timeout, a per-user rate limiter (probe amplification guard), and keys never appearing in logs or responses.
- **Rollout gates**: none. The endpoint is routed on the same `LLM_ENABLED` gate as its siblings and is inert until called; no production flip, no deployed-stack smoke, no multi-day window — so no `-rollout` sibling per the classification test.
