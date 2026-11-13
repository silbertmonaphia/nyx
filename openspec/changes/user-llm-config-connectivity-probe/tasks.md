# Tasks

## 1. Typed upstream-status carrier

- [x] 1.1 Add the status-carrying error type in `backend/internal/llm/errors.go` wrapping the existing sentinel plus the upstream HTTP status; `errors.Is` against the sentinels keeps matching. Verify `api.RegisterSentinel` mappings produce byte-identical HTTP responses before and after (existing handler tests must pass unchanged).
- [x] 1.2 Attach the carrier in `openai/client.go` `mapError` / `mapEmbedError` (`client.go:404-424`) for every branch that knows a status. Verify no branch changes its sentinel classification — only the carrier is added.

## 2. Tavily probe client

- [x] 2.1 Implement the minimal Tavily probe in `backend/internal/llmsettings/`: `POST {base}/search`, `Authorization: Bearer`, `X-Tavily-Access-Mode: keyless` when keyless, body `{"query": ..., "max_results": 1}`, through the same SSRF transport the resolver builds. Verify the request bytes match `backend/agent_py/agent/web_tools.py:350-371` in a wire-contract test.
- [x] 2.2 Map Tavily outcomes into the shared status enum (2xx ok, 401/403 auth_failed, 429 rate_limited, network unreachable, timeout) — Tavily has no width/model semantics, so `invalid_model`/`width_mismatch` are unreachable for this slot.

## 3. Validate service + handler

- [x] 3.1 Add `Validate(ctx, userID, UpdateInput) -> SlotResults` to `backend/internal/llmsettings/service.go`: skip resolution for empty base URLs, blank-key resolution per D3, per-slot `context.WithTimeout` from a new `LLM_CONFIG_PROBE_TIMEOUT` config (default `10s`) in `backend/internal/platform/config/config.go`.
- [x] 3.2 Probe the main-LLM and fallback slots with a minimal 1-token non-streaming completion through the resolver-built client; probe embedding with a 1-input call reusing the existing width check (`service.go:117-128` extraction, not duplication); classify via the D1 carrier into the eight-value enum.
- [x] 3.3 Add the huma operation `POST /api/me/llm-config/validate` in `backend/internal/llmsettings/huma_handler.go` on the same gate and auth middleware as the existing three routes; request takes an optional repeatable `slots` filter, response carries per-slot results plus the exact outbound request (method/URL/headers/body, credentials tail-4 masked, captured at the transport seam not reconstructed), and no plaintext key material anywhere. Wire a dedicated `chat.NewUserRateLimiter` (≈6/min, burst 2) in `backend/cmd/api/main.go`.
- [x] 3.4 Unit-test the disclosure: the response's `request` field matches what the httptest upstream actually received, the keyless Tavily header appears verbatim, and a search of the full response JSON finds no plaintext key.
- [x] 3.5 Emit one span + one log line per slot (name, status, duration) with no key, header, or body content. Verify with the sentinel-key test: a distinctive key value never appears in captured logs while the upstream demonstrably received the request.
- [x] 3.6 Unit-test the full matrix per slot (200 / 401 / 403 / 429 / 404-model / 500 / dial-refused / hang-past-timeout / SSRF-refusal) plus slot independence and blank-key resolution for configured and unconfigured users. Verify the stored row is untouched after any probe (read-back equality).

## 4. Frontend

- [x] 4.1 Extend `frontend/src/features/chat/services/llmConfigService.ts` with the validate call (slot filter) and the closed status union; pin the shape with a fixture captured from the Go handler's real JSON output, parsed by the TS consumer in vitest.
- [x] 4.2 Add `useLlmConfigValidate` in `frontend/src/features/chat/hooks/useLlmConfig.ts` with per-section in-flight state suppressing that section's re-click while leaving others usable.
- [x] 4.3 Add per-section test-connection controls and inline result rows to `frontend/src/features/chat/components/LlmConfigModal.tsx`: success / credential / network / config-problem classes, empty section shows no result, saving independent of probing. Add the collapsible 「查看请求」 disclosure rendering the response's `request` field with a copy action that emits a replayable curl command with the credential as an explicit placeholder. Localize all labels in `frontend/src/i18n/en.ts` + `zh.ts`.
- [x] 4.4 Vitest: per-section button states (other sections stay usable during a probe), per-slot rendering per status class, probe-then-close leaves the form and stored config unchanged, copy output contains the placeholder and no plaintext key, disclosure renders the masked headers verbatim.

## 5. E2E

- [x] 5.1 Add an agent-free Playwright spec against the side stack with a loopback stub upstream started by the harness: wrong-key fixture → inline red credential message with the upstream status in the embedding section only (stub's request log proves no other slot was probed); valid fixture → green; disclosure visible with tail-4 masked key; probe-then-close leaves the stored config unchanged. Register it in the standard `npm run test:e2e` set (not `@live`).

## 6. Docs

- [x] 6.1 Grep the `llm-config` identifier to enumerate every doc surface; document the endpoint, the status enum, and `LLM_CONFIG_PROBE_TIMEOUT` in the README What-it-does / Endpoints / Key-env-vars sections where the config endpoints are listed. Enumerated surfaces: `README_FULL.md` (BYOK paragraph, endpoints list, env table) and `FUTURE_BACKEND.md` §10 BYOK item are updated; `README.md` / `README.zh.md` carry no LLM-config section at all (verified by grep), so there is nothing to mirror there.
- [x] 6.2 Correct the guidance-drift sentence in the config modal text (width is refused at save time per PUT — `user-llm-config` PUT requirement) and tick `FUTURE_BACKEND.md` / `FUTURE_FRONTEND.md` items when the work lands.
