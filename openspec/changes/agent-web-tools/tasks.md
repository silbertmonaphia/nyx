# Tasks

Sequencing note: implement on top of the landed `agent-langfuse-tracing` change (or a worktree reset to its base) — it currently touches `config.py`, `executor.py`, `planner.py`, `main.py` uncommitted.

All work is dev/CI completable; no rollout gates are foreseen (feature is default-off; production enablement rides the PM-owned `agent-production-rollout`).

## 1. SSRF guard foundation (`agent/net_guard.py`)

- [x] 1.1 URL pre-validation: absolute `http`/`https` only, no userinfo, port 80/443 (defaulted); `PYTHON_AGENT_WEB_FETCH_ALLOW_PRIVATE=true` lifts the address deny-list and the port rule. Verify: unit tests covering scheme, userinfo, explicit-port, and allow-private cases.
- [x] 1.2 Address deny-list: resolve the host and refuse any returned address in loopback / RFC1918 / link-local (incl. `169.254.169.254`) / multicast / unspecified / reserved, IPv4 **and** IPv6 (`ipv4_mapped`, ULA). Verify: unit tests per denied class with a resolver double, plus a mixed-result case (one public + one private address → refused).
- [x] 1.3 Dial-pinned transport: TCP connect goes to a validated resolved address while Host/SNI stay the origin hostname. Verify: test where the resolver double returns a public address at validation and a private address at connect — the connection must target the validated public IP (rebind scenario in the spec).

## 2. `web_fetch` tool (`agent/web_tools.py`)

- [x] 2.1 HTML→text extraction via stdlib `html.parser`: drop `script`/`style`/`noscript`/`template`/`svg` subtrees, block tags → newline, entity handling, whitespace collapse, char cap with truncation marker. Verify: extractor unit tests incl. cap-hit marker case.
- [x] 2.2 Fetch core: content-type allowlist, byte cap, timeout, manual redirect following with per-hop re-validation and hop cap. Verify: tests via `httpx.MockTransport` — oversized body truncated, non-allowlisted content type → typed result, redirect chain over the hop cap → correctable error, redirect hop to a denied address aborted before any request.
- [x] 2.3 Result/error contract: non-2xx from the target → normal result carrying the status; transport failure (timeout/DNS/connect refused) → `ToolUnavailableError`; guard refusal → `ToolError`; text wrapped in `<untrusted_web_content>` markers; description states data-not-instructions. Verify: tests asserting each case's class reaches the right `_classify_tool_failure` bucket, and markers present in the result string.

## 3. `web_search` tool (`agent/web_tools.py`)

- [x] 3.1 Brave client: GET to the configured base URL with `X-Subscription-Token`, bounded `count`, response parsed to `{title, url, description}` list. Verify: `httpx.MockTransport` tests asserting request shape (URL, header, count param) and parsed output.
- [x] 3.2 Error mapping and bounds: HTTP 429 / 5xx / timeout → `ToolUnavailableError` (transient), query longer than 400 chars → `ToolError` (correctable), `max_results` clamped to the hard cap of 10, snippets untrusted-framed. Verify: unit tests per case, and `max_results=50` yields at most 10 results.

## 4. Dispatcher integration

- [x] 4.1 `agent/tool_dispatch.py`: `LocalToolRegistry` + `ToolDispatcher` with the `ToolsClient` surface — merged `list_tools()` (local first), `call()` routing local-first with MCP fallback, collision warn-once, MCP list failure degrades to local-only. Verify: unit tests for merge order, routing, collision warning, and degrade-on-MCP-outage.
- [x] 4.2 Wire into the executor graph: dispatcher type swap in `executor.py`/`graph.py`, local registry built from settings at startup in `main.py`, per-request pairing with the request's `ToolsClient`. Verify: updated existing executor tests still pass; new tests show (a) flags off → merged list equals the MCP list, (b) a web-tool `ToolError` drives the re-prompt path and a `ToolUnavailableError` drives the backoff path through `_dispatch_tool_with_retries`.

## 5. Config and gating

- [x] 5.1 Settings in `config.py`: `web_fetch_enabled`, `web_search_enabled` (default false), `brave_api_key` (`SecretStr`), `brave_base_url`, `web_search_max_results`, `web_fetch_max_bytes`, `web_fetch_timeout_seconds`, `web_fetch_max_chars`, `web_fetch_max_redirects`, `web_fetch_allow_private`; validators refuse startup on search-without-key and negative numerics; allow-private logs at startup. Verify: `test_config.py` cases per refusal and the default-off scenario.
- [x] 5.2 Registry-level gating: enabled flags register the tools, disabled flags leave them absent. Verify: tests showing the merged tool list is unchanged from the MCP-only list when both flags are off.

## 6. Planner awareness

- [x] 6.1 Conditional web-tools block appended to `PLANNER_SYSTEM_PROMPT` in `plan_node` (names + "use for current events / external info / user-supplied URLs" rule; empty string when off). Verify: `test_planner.py` asserts the prompt is byte-identical to the constant when off and contains the enabled tool names when on.

## 7. Wire fixtures and e2e

- [x] 7.1 Python producer tests pinning the SSE bytes for a web_search + web_fetch run (`tool_call`/`tool_result` frames) into `frontend/tests/e2e/fixtures/web-tools-*.sse` — same pattern as the existing tool-retry wire fixtures (`test_executor.py`). Verify: tests fail if producer output drifts from the fixture bytes.
- [x] 7.2 `frontend/tests/e2e/chat-web-tools.spec.ts`: replay the fixtures through the app's own SSE parser (no live model or network) — step rendering for search and fetch, fetched text displayed as plain text (not HTML), final answer streams. Verify: `npm run test:e2e` green headless with the documented dev-stack prerequisites (`make up` + `make dev`). Live-stack smoke against the real search provider is documented in the spec header as a manual dev check, not a CI gate (keyless — no API key needed; needs network egress).

## 8. Documentation and full-suite gate

- [x] 8.1 Docs and surface: `backend/agent_py/README.md` section (tools, env vars, trust-model delta for untrusted web content), `backend/agent_py/.env.example` entries, `docker-compose.yml` agent service env passthrough, `FUTURE_AGENT.md` roadmap entry. Verify: `docker compose config` parses; README env table matches `config.py` field names.
- [ ] 8.2 Broader integration gate: `cd backend/agent_py && pytest tests/ -v` fully green with no live network (all web calls mocked); frontend `npm test` and `npm run test:e2e` green. Verify: the three commands pass in the dev environment.

> 8.2 status (2026-10-14): plain `pytest tests/ -q` → 264 passed / 34 skipped in ~8s, no env overrides (the earlier langfuse-teardown hang was fixed upstream in 8a80b17 — `fix(observability)`; this change needs no override). `npm test` 207 passed. e2e: chat-web-tools (re-run against the regenerated fixture after the review fixes), chat-rag, chat-agent-planner, chat-agent-retry all green; `feed-crud.spec.ts` fails independently of this change (no feeds files in the diff; backend CRUD verified healthy via direct API probe) — unresolved, owner decision pending. Review round (test-reviewer, 2026-10-14): "request changes" with 5 required fixes (malformed-URL ValueError escape, bogus-charset LookupError, UnicodeError/odd provider-payload escapes, internal-IP disclosure, 5xx spec wording) + cheap should-fixes — all addressed; 21 new tests pin them.

## 9. Provider swap: Brave → Tavily (post-landing, 2026-10-14)

Brave Search API requires binding a credit card at signup (user constraint: refused). Swapped to Tavily — keyless tier (no account, no card, rate-limited) with an optional free key (1k searches/month, also no card). Commit 4a1fdd1 landed the Brave implementation; this section supersedes §3.1's provider and §5.1's `brave_*` field names.

- [x] 9.1 `config.py`: `brave_api_key`/`brave_base_url` → `tavily_api_key` (optional) / `tavily_base_url`; the search-without-key refuse-start validator is removed (keyless is a supported mode); negative-numeric checks unchanged. Verify: `test_config.py` — keyless-boots scenario, updated defaults, keyed-accepted case.
- [x] 9.2 `web_tools.py` `search_web`: POST `{query, max_results}` to the Tavily endpoint; `Authorization: Bearer <key>` when configured, else `X-Tavily-Access-Mode: keyless`; parse `results[].content` → description; 429/5xx/timeout transient, 432/433 plan-limit and other 4xx non-retryable. Verify: request-shape tests (keyed + keyless header cases), classification tests, payload-shape guard tests.
- [x] 9.3 Spec deltas: `agent-web-tools` search requirement retitled to Tavily + keyless/plan-limit scenarios added; `agent-python-service` env requirement renamed keys, refuse-start rule replaced with keyless-boots. Verify: `openspec validate agent-web-tools`.
- [x] 9.4 Docs: README (prose + env table), `.env.example`, docker-compose comment, design D7/risks/migration, proposal updates. Verify: grep shows no live `BRAVE` references outside historical task records (§3.1/§5.1).
- [x] 9.5 Gates: full pytest green; ruff/mypy clean; **live keyless search smoke through the dev box's proxy** (real provider call, no key) — 5 ranked results returned, untrusted-framed.

## 10. Fix: small-model context overflow on web-tool plans (2026-10-14)

Live incident: `web_search` + answering step on Qwen3-4B (4096-token context) failed with `OpenAIInvalidRequestError 400 — 4097 input tokens`; the run ended `empty_answer`. Root cause: the answering step's prior-data block re-injected 5 framed search results; with JSON escaping + Chinese tokenization that alone exceeded the model's ceiling. Fix layering: (1) bounded prior-data injection in `_render_prior_data` (per-section + total char caps, most recent sections win), (2) untrusted-content markers stripped from the prior-data rendering (kept on the wire tool_result frame), (3) the agent-web-tools spec's framing requirement amended to match.

- [x] 10.1 `executor.py`: `PRIOR_DATA_MAX_CHARS=2800` / `PRIOR_DATA_PER_SECTION_MAX_CHARS=1500`, most-recent-kept drop policy; `_strip_untrusted_markers` + `_strip_markers_recursive` applied to prior-data sections. Verify: 4 new executor tests (per-section cap, total cap + drop policy, single-huge-section, marker stripping incl. JSON-escaped values).
- [x] 10.2 Spec delta updated (framing requirement + new "Prior-data injection is bounded" scenario). Verify: `openspec validate agent-web-tools`.
- [x] 10.3 Live verification: side instance on :9003 with the fix → same failing prompt (`今天天气如何上网搜索看看？`) now produces `web_search ok → final_answer ok` with a streamed answer. Verify: no `empty_answer`, no provider 400.
