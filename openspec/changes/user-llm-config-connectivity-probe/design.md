# Design

## Context

The probe reuses machinery that already exists end to end: the resolver builds per-slot OpenAI-compatible clients from user values under SSRF rules (`llmsettings/resolver.go:144-165`), the embedding width probe exists as a save-time hook (`llmsettings/service.go:117-128`), and the Python agent defines the authoritative Tavily wire shape (`agent_py/agent/web_tools.py:350-371`). The gap is purely an affordance: none of this is reachable on demand, and nothing surfaces a classified outcome to the user.

## Decisions

### D1 — Typed upstream-status carrier, not string parsing

`openai.Client.mapError` collapses all non-429 statuses into `fmt.Errorf("upstream %d: %w", code, llm.ErrProviderUnavailable)` — the code survives only as message text. The validate service needs it structurally. Add an `llm.UpstreamStatus` carrier (an error type wrapping a sentinel plus the HTTP status) and have `mapError`/`mapEmbedError` attach the code to it. Existing behavior is preserved bit for bit: `errors.Is` against the sentinels still matches (the carrier wraps them), `api.RegisterSentinel` mappings and the safe-detail funnel are untouched. Classification in llmsettings switches on the carrier; nothing greps strings.

### D2 — Slot clients come from the resolver, not a parallel builder

The probe builds clients through `Resolver.embedSlot` / `Resolver.buildClient` so SSRF transport, keyless placeholder key, and `BYOK_ALLOW_PRIVATE_URL` semantics are literally the same code path the runtime uses — a probe result cannot diverge from runtime behavior by construction. The Tavily slot has no Go client today; add a minimal one inside llmsettings that mirrors the agent's shape exactly: `POST {base_url}/search`, `Authorization: Bearer <key>` header, `X-Tavily-Access-Mode: keyless` when no key, body `{"query": "<tiny>", "max_results": 1}`. A wire-contract test pins the request bytes against the agent's documented shape (the cross-runtime consumer rule: the probe's request must satisfy the *agent-side* consumer, or probe-ok would not imply runtime-ok).

### D3 — Blank-key resolution mirrors PUT

PUT's contract is "empty key field = keep the stored key". Validate adopts it verbatim: configured user + blank key → probe with the decrypted stored key (server-side only, never in the response). Unconfigured user + blank key → Tavily probes keyless (the provider supports it); OpenAI-shaped slots report `invalid_config`, because a remote endpoint without a credential is exactly what PUT would refuse at save time.

### D4 — Timeout is its own knob

`LLM_PROBE_TIMEOUT` (2s default) budgets the router's failover health check — too tight for a cold 1-token completion on a small provider. Add `LLM_CONFIG_PROBE_TIMEOUT` (default `10s`), read once in main.go, passed to the validate service. Each slot runs under its own `context.WithTimeout` derived from the request context, so one slow slot cannot extend another's budget beyond its own.

### D5 — Rate limit: reuse the chat limiter pattern

A validate call triggers up to four outbound requests, which makes the endpoint an amplification primitive if unthrottled. Wire a dedicated `chat.NewUserRateLimiter` instance (rate ~6/min, burst 2 — generous for a human clicking "test", hostile to scripted probing; a per-section button click filters to one slot, so one call). Rejected calls return 429 before any upstream dial.

### D6 — Request scopes to the clicked section; response discloses what it sent

The endpoint accepts an optional `slots` filter (`chat` | `embedding` | `fallback` | `tavily`, repeatable). Absent → probe every filled slot; present → probe only the named slots, so a per-section button never spends another provider's quota or probes another section's mid-edit values. Each probed slot carries its result **plus the exact outbound request** — method, URL, headers, body — so the user can see, and replay, precisely what was sent:

```json
{ "slots": {
    "chat":      { "probed": true, "status": "ok",           "http_status": 200, "detail": "",
                   "request": { "method": "POST", "url": "https://…/v1/chat/completions",
                                "headers": { "Authorization": "Bearer ••••ab12", "Content-Type": "application/json" },
                                "body": { "model": "…", "max_tokens": 1, "messages": "…" } } },
    "embedding": { "probed": true, "status": "auth_failed", "http_status": 401, "detail": "",
                   "request": { "method": "POST", "url": "https://…/v1/embeddings", "headers": "…", "body": "…" } },
    "fallback":  { "probed": false },
    "tavily":    { "probed": false }
} }
```

The disclosure is captured from the actual outgoing request at the transport seam, not reconstructed from inputs — what the user sees is what the upstream received, including the keyless access-mode header on a keyless Tavily probe. Credentials in `headers` are masked to the established tail-4 convention (`Bearer ••••ab12`); the plaintext key never appears in the response, the DOM, or the copy output. `status` is the fixed eight-value enum from the spec; `detail` carries a provider-specific fragment only when it is safe (width numbers, model name echo) and never key material or response bodies. The frontend consumes this through `llmConfigService.ts`; the vitest suite pins the shape with a fixture captured from the Go handler's actual JSON output (wire-contract rule: parse with the consumer, don't assert the producer's in-process shape).

### D7 — Observability and the key-absence test

One OTel span per slot probe (`nyx.llmsettings.validate.<slot>`) with status and duration attributes; one structured log line per slot at Info on `ok`, Warn otherwise — slot name, status, duration. No URL query, no headers, no bodies. The key-never-logged test embeds a sentinel key value in the probe request against an httptest upstream, triggers a full probe, and asserts the sentinel appears nowhere in captured log output — with an assertion that the probe demonstrably ran (the upstream saw the request), so the absence claim cannot pass vacuously.

### D8 — Frontend: one button per section, result inline in that section

Each visible section in `LlmConfigModal.tsx` (main LLM, embedding, Tavily) carries its own test-connection button; `useLlmConfig.ts` gains a `useLlmConfigValidate` mutation parameterized by slot, calling the endpoint with that single slot in the `slots` filter. The result renders inside its own section: green success, red credential/config failure, amber network failure (retryable), nothing when the section is empty. Below the status line, a collapsible 「查看请求」 disclosure shows the exact outbound request from the response's `request` field (headers already tail-4 masked server-side), with a copy action that emits a replayable curl command in which the credential is an explicit placeholder (`<你的API key>`) — generated client-side, never the plaintext. In-flight state is per section — one section probing does not disable the others. Status-to-label mapping lives in `i18n/en.ts` + `zh.ts`. No auto-probe on save — the buttons are explicit; auto-probe would double save latency and probe mid-edit values for no diagnosed pain.

Rationale for per-section over one global button: sections are independent providers with independent keys and failure modes (the capability's own guidance requirement already mandates treating them so), users edit one section at a time, a global button would spend other providers' quota on every edit and report cross-section noise from half-edited values, and one slow slot's timeout would delay the section the user actually cares about.

### D9 — Observed spec drift, noted not silently fixed

`user-llm-config`'s in-modal guidance requirement says a wrong-width embedding model is "refused at query time rather than at save time", while the PUT requirement probes width at save and returns 400. The drift predates this change; the probe makes width visible on demand either way. The docs task corrects the guidance sentence to match the PUT behavior rather than leaving both statements live.

## Testing

- **Backend unit** (httptest upstreams, no DB — probing stores nothing): per-slot matrix `200 / 401 / 403 / 429 / 404-model / 500 / dial-refused / hang-past-timeout / SSRF-refused`; blank-key resolution (configured + unconfigured); slot independence (upstream that fails slot A answers slot B); rate limiter; key-absence-in-logs with the anti-vacuity marker from D7; disclosure fidelity — the response's `request` field compared field-by-field against what the httptest upstream actually received, plus a plaintext-key absence assertion over the full response JSON.
- **Wire contract**: Tavily probe request bytes asserted against the agent's shape; validate-response fixture fed to the frontend service in vitest.
- **Frontend vitest**: per-section button states (idle/probing/other-section-still-usable), per-slot rendering per status class, empty-section-no-result, localized labels.
- **Playwright e2e** (agent-free set): stub upstream on loopback started by the harness (BYOK allows loopback by deployment config in the side stack), wrong-key fixture → inline red credential message in the embedding section only, correct fixture → green; probing one section sends no request for the others (asserted via the stub's request log); probe-then-close-leaves-config-unchanged assertion.

## Risks

- Provider variance: some OpenAI-compatible servers reject `max_tokens: 1` or empty prompts; the probe sends a minimal but well-formed 1-token completion with a non-empty prompt. Documented in code via the test matrix, not comments.
- Probe-vs-runtime drift for exotic providers (header quirks, path prefixes) is bounded by D2's shared construction; residual drift is inherent to any probe.
