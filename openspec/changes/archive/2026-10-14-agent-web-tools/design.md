# Design

## Context

See proposal.md — Why. Shape of the current code that constrains the approach:

- The executor has **one tool funnel**: `_dispatch_tool_with_retries` (`backend/agent_py/agent/executor.py:542`) wraps every tool call — `tool` steps and react-loop iterations alike — around `tools.call(name, args)` (line 624), and classifies failures into three classes (`arg` → model re-prompt, `transient` → backoff retry, else fail fast) from the exception type raised.
- Tool schemas reach the model via two call sites, `tools.list_tools()` at `executor.py:311` (react bind) and `:744` (retry re-prompt). Both consume `ToolsClient` (`agent/tools_client.py`), a single-endpoint MCP JSON-RPC client.
- The planner prompt is **static** (`PLANNER_SYSTEM_PROMPT`, `agent/planner.py:59`) — it names step kinds but no tool catalog; the planner emits tool names from model prior, and an unknown name only surfaces when the registry rejects it at execute time.
- The Go backend already has the SSRF-guard pattern this design mirrors: `checkHostNotPrivate` (`internal/platform/config/config.go:751`) + a per-`DialContext` re-check in the vLLM client, with `LLM_ALLOW_PRIVATE_URL` as the dev opt-out.
- Config is pydantic-settings with `env_prefix="PYTHON_AGENT_"` (`agent/config.py:15`), validators refuse startup on bad values, and feature flags default off when they introduce external dependencies (langfuse).

## Goals / Non-Goals

**Goals:**

- Two tools — `web_search` (Tavily), `web_fetch` (guarded) — usable from `tool` steps and react loops with zero changes to the retry machinery, wire contract, or step kinds.
- Fetch safety at least as strong as the Go backend's guard: deny-class IP check, per-hop re-validation, dial-address pinning (rebind defense), bounded everything.
- Byte-identical behavior when the flags are off.

**Non-Goals:**

- No MCP exposure — the tools live and die inside the Python agent; the Go registry surface is untouched.
- No caching layer for fetch/search results (the Go Redis cache is not reachable from these tools by design; can be added later behind the same tool contract).
- No JS rendering / browser automation — `web_fetch` retrieves and extracts static HTML only; JS-heavy SPAs may return little text (the model sees the truncation honestly, not an error).
- No robots.txt/crawl policy engine — these are user-triggered single-page fetches, not a crawler.
- No PDF/binary parsing — non-textual content types are refused per spec.

## Decisions

**D1 — Local tools are a second source inside a `ToolDispatcher`, not a fork of the executor.** New module `agent/tool_dispatch.py` exposing `ToolDispatcher(local: LocalToolRegistry, mcp: ToolsClient)` with the identical `list_tools()` / `call(name, args)` surface. `main.py` builds the local registry once from settings at startup and pairs it with the per-request `ToolsClient`; `executor.py` / `graph.py` change only type hints. The executor's two schema call sites and the single dispatch call site stay structurally untouched.
*Alternatives:* teach the executor about two tool sources (bigger diff, two dispatch paths — rejected); put the merge in `ToolsClient` (conflates transport with composition — rejected).

**D2 — Local tools raise the *existing* exception types.** `ToolError` (correctable/arg class), `ToolUnavailableError` (transient), `ToolRejectedError` (non-retryable). `_classify_tool_failure` then works byte-for-byte unchanged, and the retry-tests continue to cover the web tools for free. The spec's classification scenarios are satisfied by construction.
*Alternatives:* a parallel `WebToolError` hierarchy + extended classifier (more churn, no benefit — rejected).

**D3 — Merge semantics: local first, MCP failure degrades to local-only.** `list_tools()` returns local schemas then MCP schemas. Today an MCP `tools/list` failure produces an empty tool list (react runs tool-less); with local tools present the failure degrades to local-only and is logged — strictly more capable, and `call()` for an MCP name still fails through the normal transient path. Name collision (local vs MCP): local wins, warn once per run (spec: agent-web-tools » funnel requirement). Collision is impossible with today's registry names; the rule exists so a future registry tool named `web_fetch` cannot silently route to the wrong implementation.

**D4 — SSRF guard in `agent/net_guard.py`, dial-pinned.** Validate: absolute http/https URL, no userinfo, port 80/443 (defaulted), host resolves → **every** returned address must pass the deny-list (loopback / private / link-local / multicast / unspecified / reserved, IPv4 **and** IPv6 including `ipv4_mapped` and ULA — `ipaddress` stdlib). Connect: a custom transport pins the TCP dial to one validated resolved address while Host/SNI stay the origin hostname — the Python equivalent of the Go `DialContext` re-check, defeating DNS rebinding. Redirects are followed manually (httpx `follow_redirects=False`), each hop re-parsed + re-validated, capped by `max_redirects`. `WEB_FETCH_ALLOW_PRIVATE=true` short-circuits the deny-list **and** lifts the port restriction (fixture servers don't run on 80/443); it is logged loudly at startup and is for dev/e2e only.
*Alternatives:* resolve-then-compare without pinning (TOCTOU rebind window — rejected, the Go side doesn't accept it either); a deny-list of hostnames (trivially bypassed — rejected); egress proxy (infrastructure the repo doesn't have — out of scope).

**D5 — Error mapping for fetch targets vs fetch failures.** Upstream non-2xx (404, 403, 500-from-target…) is a **normal tool result** carrying the status — the tool executed; the model decides. Transport failures (timeout, connect refused, DNS failure, upstream 5xx at the edge? — no: an HTTP 5xx *from the target* is a result too; "transport" means the client could not complete the exchange) raise `ToolUnavailableError` → transient retry. Guard refusals (denied address, bad scheme, oversized query) raise `ToolError` → model re-prompt, because the model can correct the URL.
*Alternative:* treat 404 as an error (rejected — the model, not the retry loop, should choose the next URL).

**D6 — HTML→text via stdlib `html.parser`.** A small `HTMLParser` subclass: drop `script/style/noscript/template/svg` subtrees, block tags → newline, inline tags → space, `convert_charrefs=True`, collapse whitespace, then cap at `WEB_FETCH_MAX_CHARS` with a truncation marker. No new dependencies.
*Alternatives:* `trafilatura`/`readability-lxml`/`selectolax` (better extraction, new deps + heavier installs — deferred; revisit only if observed quality is poor — Open Question 2).

**D7 — Tavily stays concrete; the seam is the configurable base URL + the `search_web()` function.** Provider swap (2026-10-14, post-landing): Brave was the original pick but requires binding a credit card; Tavily's keyless tier (no account, rate-limited) with an optional card-free free key (1k searches/mo) satisfies "no card" without self-hosting. One provider behind a function boundary is enough indirection for now. The base URL override is what makes e2e stubs and unit tests possible without mocking at the HTTP level (they still can via `httpx.MockTransport`, matching `test_tools_client.py`).
*Alternatives:* SearXNG self-hosted (no accounts at all, but adds a compose service + `settings.yml` JSON-format enablement, engine CAPTCHA/rate-limit fragility, and container proxy config on proxy-only hosts — heavier ops for the same outcome; revisit if vendor independence becomes a requirement); DuckDuckGo scraping (no account but fragile HTML/ToS-gray — rejected); a `SearchProvider` protocol mirroring `internal/llm.Provider` (premature with one implementation — rejected; revisit when a second provider is real).

**D8 — Untrusted framing is applied inside the tool implementation.** Search snippets and fetched text are wrapped in `<untrusted_web_content>` markers at the point of construction, so both the SSE `tool_result` frame and `_render_prior_data`'s "Data gathered by earlier steps" block carry the markers without renderer changes. Tool descriptions state the content is data, never instructions.

**D9 — Planner learns about the tools via a conditional prompt block.** `plan_node` composes `PLANNER_SYSTEM_PROMPT + web_tools_block(settings)`; the block is `""` when both tools are off (byte-identical requirement) and lists the enabled tool names + the "use for current events / external info / user URLs" rule when on. This is the minimal fix for the planner being catalog-blind; a full dynamic-catalog injection (fetch `list_tools` at plan time) is deliberately not done here.

**D10 — Deterministic e2e via pinned SSE fixtures; live smoke is optional.** New `frontend/tests/e2e/chat-web-tools.spec.ts` replays `web-tools-*.sse` fixtures through the app's own parser (the `chat-agent-retry.spec.ts` pattern), with the fixture bytes pinned by Python producer tests in `test_executor.py`. A live-stack describe running a real chat through Tavily is documented as a manual smoke (keyless needs no key; it is not a CI gate).

## Risks / Trade-offs

- **Prompt injection through fetched content** → framing + caps reduce the attack surface but cannot eliminate it; a 4B-parameter model is credulous. Accepted: the tools are read-only, per-owner data tools remain per-owner scoped, and the blast radius is the model's own plan. Tool descriptions and the untrusted markers are the mitigation; note it in README's trust model.
- **Keyless-tier throttling** → concurrent runs can 429; the existing transient-retry path absorbs it with backoff, but a burst of runs may surface `tool_execution_transient` failures. Acceptable for dev/first shipping; a free key raises the limit, and a token bucket can be added behind `web_search` later without spec change (Open Question 1).
- **Dial-pinning transport is the most intricate piece** → if the custom transport proves too invasive on the pinned httpx/httpcore version, the fallback is resolve→validate→connect-immediately with a narrow TOCTOU window, explicitly noted in code. The spec's rebind scenario (agent-web-tools » refuses private targets) is the contract either way; the pinned dial is the strong form.
- **JS-heavy pages return little text** → honest truncation/low-content results beat fabricated content; model may retry with a different URL. Documented in the tool description.
- **Small-model tool selection** (Qwen3-4B may over-use web tools or mis-format URLs) → planner rules + correctable-error re-prompts; `agent_tool_calls_total{name}` metric distinguishes web-tool usage; observe before tuning.
- **Sequencing with the in-flight `agent-langfuse-tracing` change** → it currently touches the same files uncommitted (`config.py`, `executor.py`, `planner.py`, `main.py`, `observability.py`). Implement this change on top only after langfuse lands, or in a worktree reset to the post-langfuse base — see memory note on worktree drift.
- **Spec overlap: canonical `agent-loop`** → its requirement text ("`tool` (registry dispatch)", scenario "tools dispatched match the registry") predates the Python service and is now incomplete for the Python path; this change amends `agent-python-service` (the live normative spec) and deliberately leaves `agent-loop` untouched. Flagged for a follow-up spec cleanup rather than silently amended here.

## Migration Plan

Default-off, so deploying this change is inert: no env flip, no behavior change, no schema/data migration. Enablement is `PYTHON_AGENT_WEB_*` flags (+ optional `PYTHON_AGENT_TAVILY_API_KEY`; keyless works with none). Rollback is flipping the flags back to `false` — no state to unwind. Production enablement rides the existing PM-owned `agent-production-rollout`; no rollout sibling is created for this change.

## Open Questions

1. Per-user rate limit on `web_search`/`web_fetch` (beyond model-budget gating) — deferred; caps + bounded retries first, add if provider throttling or observed abuse justify it. Does not change specs or approach.
2. HTML extraction quality on structured pages (docs sites, articles) — deferred; swap the stdlib extractor for a readability library later behind the same tool contract if quality is poor in practice.
