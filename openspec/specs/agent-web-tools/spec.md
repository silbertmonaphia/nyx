# Agent Web Tools Specification

## Purpose

Gives the agent read-only access to the public web — Tavily-backed search and SSRF-guarded page fetching — as agent-native tools dispatched through the executor's existing tool funnel, without extending the MCP surface.

## Requirements

### Requirement: Web tools dispatch through the existing tool funnel

When enabled, `web_search` and `web_fetch` SHALL be available to `tool` steps and to `react` steps exactly like registry tools: their schemas appear in the same tool list the model is bound to, their calls emit the same `tool_call` / `tool_result` frames, and their failures are classified into the existing retry classes (correctable argument error → model re-prompt with the validator's message; transient → backoff retry; otherwise → fail fast, no retry). Dispatch SHALL resolve agent-native tools first and fall back to the MCP registry. When an agent-native tool name collides with an MCP-exposed tool name, the agent-native tool MUST win and the collision MUST be logged once per run. Agent-native tools MUST NOT be exposed through the MCP server.

#### Scenario: ReAct loop can call web_search

- **WHEN** a `react` step runs with web tools enabled and the model emits a `web_search` tool call
- **THEN** the call dispatches to the agent-native implementation, and its result returns to the model as a `ToolMessage` preceded by the standard `tool_call` / `tool_result` SSE frames

#### Scenario: Tool step can call web_fetch by name

- **WHEN** the planner emits a `tool` step with `tool: "web_fetch"` and a public URL
- **THEN** the executor dispatches the agent-native fetch and the step's output is rendered into later steps' context blocks like any registry tool result

#### Scenario: Local failures keep the existing retry semantics

- **WHEN** a web tool call fails with a correctable argument error (e.g. malformed URL, denied address) versus a transient transport error (e.g. timeout, provider 429)
- **THEN** the correctable class routes to the model re-prompt path, the transient class routes to backoff retry, and any other error fails the step fast — identical to registry-tool failures

#### Scenario: Name collision warns and prefers the agent-native tool

- **WHEN** the merged tool list would contain two tools with the same name
- **THEN** the agent-native tool is dispatched for that name, the collision is logged once per run, and the MCP tool remains reachable only under a non-colliding name

### Requirement: `web_search` queries the Tavily Search API

`web_search` SHALL accept a `query` string (required, 1–400 characters) and return a compact ranked result list — for each result a title, URL, and description snippet — bounded by a configurable maximum (default 5, hard cap 10). The search provider base URL and API key SHALL come from the run's Tavily override when one is present, and otherwise from configuration; the API key SHALL be optional: with no key from either source the tool MUST operate against the provider's keyless tier (no account required), and with a configured key it MUST authenticate against the account. Provider throttling (HTTP 429), server errors (5xx), and timeouts SHALL be classified as transient (retryable); malformed or invalid queries SHALL be classified as correctable argument errors; account/plan-limit rejections SHALL NOT be retried. The override's base URL is user-supplied and is not filtered against private or restricted addresses; that surface is accepted for self-hosted deployments and is intentionally asymmetric with `web_fetch`, which continues to refuse private and unsafe targets.

#### Scenario: Search returns bounded compact results

- **WHEN** the model calls `web_search` with a valid query
- **THEN** the tool returns at most the configured maximum number of results, each carrying only title, URL, and description

#### Scenario: Run override selects provider endpoint and credential

- **WHEN** a run carries a Tavily override and the model calls `web_search`
- **THEN** the search request goes to the override's base URL with the override's credential, and the deployment-configured values are not consulted

#### Scenario: A per-user run without Tavily has no search tool

- **WHEN** a run carries a per-user LLM override but no Tavily override
- **THEN** `web_search` is not registered for that run, the planner catalogue does not offer it, and the deployment-configured Tavily values are NOT used as a fallback

#### Scenario: Keyless operation without an API key

- **WHEN** `web_search` runs with no API key from either the override or configuration
- **THEN** the search succeeds against the provider's keyless tier and no Authorization credential is sent

#### Scenario: Provider throttling is transient

- **WHEN** the upstream search provider responds HTTP 429 or times out
- **THEN** the failure is classified transient and dispatched on the backoff-retry path

#### Scenario: Plan-limit rejection is not retried

- **WHEN** the provider rejects the request because the account or plan limit is exhausted
- **THEN** the failure is classified non-retryable and the step fails fast

#### Scenario: Oversized query is a correctable error

- **WHEN** the model calls `web_search` with a query longer than 400 characters
- **THEN** the tool rejects it as a correctable argument error and the model is re-prompted with the constraint

### Requirement: `web_fetch` refuses private and unsafe targets

`web_fetch` SHALL accept only absolute `http` or `https` URLs on ports 80 or 443. Before any request, and again for every redirect hop, it MUST resolve the target host and refuse any address in the loopback, RFC1918 private, link-local (including the cloud metadata address 169.254.169.254), multicast, or unspecified classes, for both IPv4 and IPv6 (including IPv4-mapped and IPv6 unique-local forms). The connection MUST be pinned to the validated resolved address so a name that re-resolves between validation and connection cannot reach a denied address. Redirects SHALL be followed manually with a bounded hop count (default 5); a hop whose target fails validation MUST abort the fetch before any request is sent to the denied address. Refusals SHALL be classified as correctable argument errors (no transient retry of the same URL).

#### Scenario: Private address refused before any request

- **WHEN** the model calls `web_fetch` with a URL whose host resolves to 127.0.0.1, 10.0.0.0/8, 192.168.0.0/16, or 169.254.169.254
- **THEN** the tool refuses with a correctable error and no HTTP request is issued to that address

#### Scenario: Redirect to a private address is aborted

- **WHEN** a public URL responds with a redirect whose target resolves to a denied address
- **THEN** the fetch aborts at that hop, no request reaches the denied address, and the tool returns a correctable error

#### Scenario: Non-http scheme refused

- **WHEN** the model calls `web_fetch` with a `file:`, `ftp:`, or other non-http(s) URL
- **THEN** the tool refuses with a correctable error without attempting any connection

#### Scenario: Rebind between validation and connection cannot bypass the guard

- **WHEN** a hostname resolves to a public address during validation and to a denied address at connection time
- **THEN** the connection goes to the validated public address only; the denied address is never contacted

### Requirement: `web_fetch` responses are bounded and content-aware

`web_fetch` SHALL enforce an allowlist of textual content types (HTML, plain text, JSON, XML, Markdown); responses with other content types MUST be refused with a model-visible typed result rather than fetched into context. Responses SHALL be read under a byte cap (default 2 MiB) and a request timeout (default 10 s). HTML SHALL be reduced to readable text before entering model context, and the final text SHALL be capped (default 20 000 characters) with an explicit truncation marker when the cap is hit. Upstream non-2xx statuses — including the target's own 5xx — SHALL be returned as a normal tool result carrying the status (the model decides what to do), NOT raised as retryable failures; transport-level failures (timeout, DNS failure, connection refused — i.e. no HTTP response arrived) SHALL be classified transient.

#### Scenario: Oversized document is truncated at the cap

- **WHEN** a fetched page's extracted text exceeds the character cap
- **THEN** the tool result contains text up to the cap plus a truncation marker

#### Scenario: Non-allowlisted content type is refused as a result

- **WHEN** a URL returns `application/octet-stream` or an image content type
- **THEN** the tool returns a typed result naming the content type and no body bytes enter the model context

#### Scenario: Upstream 404 is a result, not a failure

- **WHEN** the fetched URL returns HTTP 404
- **THEN** the tool returns a normal result carrying the status code and the step does not enter the retry path

#### Scenario: Timeout is transient

- **WHEN** the fetch times out or the connection is refused
- **THEN** the failure is classified transient and dispatched on the backoff-retry path

### Requirement: Web content is framed as untrusted data

Text originating from the web — search snippets and fetched page text — SHALL be wrapped in explicit `<untrusted_web_content>` markers in every tool result frame, and the tool descriptions SHALL state that the content is data to be summarized or quoted, never instructions to follow. Downstream context renderings (the executor's prior-data block injected into the answering step) SHALL preserve the text content; the marker overhead MAY be stripped there, because on small-context models the framing alone can exhaust the answer prompt's token budget (the 2026-10-14 Qwen3-4B 4096-token incident).

#### Scenario: Fetched text carries the untrusted markers

- **WHEN** `web_fetch` succeeds
- **THEN** the model-visible tool result frame wraps the extracted text in `<untrusted_web_content>` markers and the tool description marks the content untrusted

#### Scenario: Search snippets carry the untrusted markers

- **WHEN** `web_search` succeeds
- **THEN** each result's description snippet is delivered under the same untrusted-content framing in the tool result frame

#### Scenario: Prior-data injection is bounded for small models

- **WHEN** a plan's answering step receives earlier web-tool outputs as prior data
- **THEN** the injected block is bounded (per-section and total character caps, most recent sections kept) and carries the result text without the marker overhead, so the answer prompt stays within a small model's context window

### Requirement: Disabled web tools are indistinguishable from today

With both web-tool flags disabled (the default), the merged tool list SHALL equal the MCP registry list, the planner prompt SHALL be byte-identical to its current form, and the agent SHALL issue no outbound web requests.

#### Scenario: Default configuration changes nothing

- **WHEN** the service runs with web tools disabled
- **THEN** `list_tools()` returns exactly the MCP registry's tools, the planner prompt contains no web-tool text, and fetching/searching is impossible

#### Scenario: Enabling advertises the tools to the planner

- **WHEN** web tools are enabled
- **THEN** the planner prompt lists the enabled web tools and instructs the planner to use them for requests about current events, external information, or user-supplied URLs
