# Design

## Context

See `proposal.md` — Why. Constraints that shape the approach:

- The Go MCP server (`internal/mcp`, mcp-go v1.1.0) already serves the five feed tools and marshals `annotations` in `tools/list`; `WithReadOnlyHintAnnotation` / `WithDestructiveHintAnnotation` exist in the pinned version. The Python `ToolsClient` receives the MCP-native shape (`{name, description, inputSchema, annotations}`).
- The Python executor already merges MCP `tools/list` into react-loop binding and dispatches plan `tool` steps by name through one shared funnel (`_dispatch_tool_with_retries` in `executor.py`) — the natural single choke point for a confirmation gate.
- The Python service already has: per-run heartbeat (`_heartbeat_loop` refreshing `agent.runs.last_heartbeat_at`), a stale-run sweeper that reaps runs whose heartbeat is stale (900 s default), Redis (budget guard, `redis-py`), run metadata with per-user ownership, and resumable runs via the langGraph checkpointer.
- The Go chat proxy byte-forwards the Python SSE stream (no frame transformation), and the frontend SSE parser skips unknown event kinds (`chatService.ts` branches on known kinds only).
- Verified defect: `langchain_core.convert_to_openai_tool` **accepts** the MCP-native dict but drops `inputSchema` — bound MCP tools reach the model schemaless today (checked in `backend/agent_py/.venv`; conversion output has no `parameters`). `_tool_schema_text` likewise reads a `parameters` key that the native shape never had.

## Goals / Non-Goals

**Goals:**

- One normalization point so every consumer (react binding, retry re-prompt schema text, planner catalog, confirmation policy) sees the same function-tool shape, with MCP annotations preserved on a side channel.
- A planner catalog that is a truthful mirror of the live bearer-scoped registry — no flag, no drift, degrade-to-today on failure.
- A confirmation gate that is (a) data-driven (annotations, fail-safe), (b) universal (plan `tool` steps and react loops alike), (c) cross-replica correct, (d) fail-closed in every failure mode, and (e) built on the existing held-stream + heartbeat model rather than a new run lifecycle.

**Non-Goals:**

- No change to the external MCP wire shape beyond the additive `annotations` field (external MCP clients see a superset they can ignore).
- No Go-side run/ownership store: the agent's run metadata stays the single authority for run↔user.
- No per-user / per-run approval policy surface; one global TTL env key.
- No styling effort beyond a functional approve/deny affordance in the chat (visual polish is a frontend-task detail).
- No confirmation for read-only tools, and no "remember my choice" caching.

## Decisions

### D1 — Normalize MCP-native schemas at the dispatcher boundary

`ToolDispatcher` normalizes registry-sourced entries (native → OpenAI function format, `inputSchema` → `parameters`) once per `list_tools()` call, and exposes both a merged view (for binding) and a registry-only view (for the planner catalog). Annotations travel on a per-tool record alongside the schema, never inside the bound function object (LangChain would drop them — the same silent-drop trap this change exists to fix).

Alternatives: normalize in each consumer (three copies that will drift — the current bug class); normalize in the Go handler (changes the wire for external MCP clients, who expect MCP-native); accept LangChain's silent drop and special-case later (that is today's bug). Also fix `_tool_schema_text` to read from the one normalized shape.

### D2 — Confirmation transports over the held stream, decisions brokered via Redis

When the funnel reaches a mutating call it emits `confirm_request`, then **waits inside the same SSE response** for a decision brokered through Redis: the approval endpoint writes `approval:{run_id}:{approval_id}` with `SET NX` (first decision wins, duplicates no-op) and the run's waiter polls `GET` (≈0.5 s) until decision or TTL. While waiting, the stream emits SSE comment keep-alives every ~15 s (protocol-legal, skipped by the existing frontend parser) so intermediaries don't cut the idle connection; the existing DB heartbeat keeps the sweeper from reaping the run.

Alternatives: **langGraph `interrupt()` + `Command(resume=…)`** — idiomatic, but splits one chat turn across two HTTP streams, needs a new run status (`awaiting_approval`) with sweeper ties, and interacts with the pure-resume `None`-input subtlety documented in `graph.py`. Go-side interception — impossible: the Python agent calls `/mcp` directly with the user's bearer. MCP elicitation — asks the tool client (the agent), not the end user. Pub/Sub instead of poll — works, but adds subscription lifecycle handling for no observable benefit at a 0.5 s poll cadence.

Cross-replica note: the approval POST may land on any agent replica; `SET NX` in Redis makes first-wins atomic across replicas, and the waiter resolves regardless of which replica received the decision.

### D3 — The mutating signal is the MCP annotation, fail-safe on absence

A registry tool requires confirmation unless its annotations assert `readOnlyHint=true`. `create_feed` / `update_feed` / `delete_feed` are annotated `readOnlyHint=false` (+ `destructiveHint=true`); `list_feeds` / `get_feed` are annotated `readOnlyHint=true`. Absent/empty annotations ⇒ confirmation required — a future mutating MCP tool is gated by default, and a new tool author must make a deliberate read-only claim to skip the gate.

Alternatives: a hardcoded Python name list (drifts from the Go registry; new tools silently ungated — exactly the class of coupling this repo's wire-contract rule warns about); `destructiveHint` only (too narrow: create/update mutate without being destructive); a second Go-side config channel (more machinery, same drift).

### D4 — The planner catalog is derived per run, and its absence is the "off" state

The planner prompt gains a "Registry tools available" block built from the dispatcher's registry-only list at plan time; no block when the list fails or is empty (prompt identical to today). Local agent-native tools are excluded — web tools keep their own catalog with untrusted-content framing. The planner node factory gains the dispatcher (graph wiring change, mirroring the executor factory).

Alternatives: static block (lies whenever the registry changes or MCP is off); a `PYTHON_AGENT_FEED_TOOLS_ENABLED` flag gating a static block (duplicated gate — `MCP_ENABLED` already is the deliberate gate — and a half-gate: react binding is unaffected by it, so "off" would only blind the planner, not close the capability). Cost: one `tools/list` call per run — memoized per dispatcher instance (per run, per bearer), so the planner catalog, react binding, retry-schema text, and the gate's annotation map all read the same fetch; the confirmation gate adds no fetch of its own. If the registry hangs, planning is delayed by the existing 15 s MCP client timeout at worst, and the degrade path is the block being omitted.

### D5 — The gate sits in the shared funnel, once per tool call

The check runs inside `_dispatch_tool_with_retries` before the first dispatch attempt, after normalization, consulting the registry-only annotation map. Consequences by construction: plan `tool` steps and react loops (including `_run_react`'s per-call dispatch and the re-prompt path's eventual re-dispatch) are all gated; transient retries of an already-approved call do NOT re-confirm (the approval covers the intended mutation, and re-prompting on each backoff would be unusable); each mutating call gets its own approval id; batches of parallel mutating calls in one assistant turn confirm sequentially (the funnel is sequential — accepted, documented, and visible in the stream as consecutive `confirm_request` frames).

### D6 — Deny / expiry / broker outage are denials, funneled as tool results

A denial (explicit, TTL expiry, or decision-store unavailable) closes the call with a model-visible result: the tool is not dispatched, the model receives a static, safe denial text naming the tool and reason (an `ok:false` tool result, not an `error` frame), the run continues, and no retry counter increments. No failure mode auto-approves. The TTL is `PYTHON_AGENT_APPROVAL_TTL_SECONDS` (default 300 s); the Redis key TTL is the approval TTL plus a margin so an expiry-then-decision race resolves as "expired" (404 at the endpoint) rather than a late approval.

### D7 — New endpoints mirror the existing trust boundaries

- Python `POST /agent/approvals`: `X-Nyx-User-Id` header contract identical to `/agent/run` (no JWT verification — the network boundary is the trust boundary); ownership checked via run metadata; single-sentinel 404 for non-owned/unknown runs; typed 404 for unknown/expired approval ids; duplicates idempotent.
- Go `POST /api/chat/approvals`: lives next to the existing chat routes under the standard Bearer JWT middleware; handler forwards body to the agent with `X-Nyx-User-Id` stamped, passes status/body through, never logs the token, fails closed (never auto-grants) when the proxy is disabled/unreachable. Rate limiting: reuse the per-user limiter pattern already applied to chat/MCP surfaces.

### D8 — Wire tests round-trip through the consumer

- Python: normalization tests consume a **captured mcp-go `tools/list` fixture** (native shape incl. annotations), not hand-written OpenAI-format dicts — closing the self-consistent-wrongness gap that hid the schema drop.
- Frame tests: the Go proxy's byte-forward regression covers `confirm_request`; the frontend parser test consumes a captured Python `confirm_request` frame.
- E2E (Playwright, live stack): create-with-approve lands the feed and renders the confirmation affordance; deny leaves the feeds page untouched; `list_feeds` completes without any confirmation UI.

## Risks / Trade-offs

- [Held stream during human think-time: proxies cut idle connections despite keep-alives] → SSE comments every ~15 s keep bytes flowing; e2e runs through the real compose topology (nginx + Go proxy) so a cut shows up as a failing test, not a prod surprise.
- [An MCP tool mis-annotating itself read-only bypasses the gate] → the registry is first-party (our Go server) and bearer-scoped; fail-safe covers absent annotations; a lying annotation is a server-side bug with server-side review, not an agent-side risk.
- [Planner prompt grows with registry size (token cost)] → 5 tools today; the block is one line per tool. If the registry grows large, the block can be truncated/summarized later without wire changes.
- [Approval endpoint abuse (decision spam)] → per-user rate limiting (same pattern as chat/MCP); approval ids are run-scoped and single-use, so spam cannot flip a decided outcome.
- [Wait loop polling Redis at 0.5 s per paused run] → bounded by concurrent runs holding approvals (low in practice); poll interval is a constant, not config.
- [Sequential confirmations for parallel mutating calls slow the stream] → accepted; parallel mutating calls in one turn are rare, and each confirmation is individually observable in the stream.

## Migration Plan

- No schema migrations, no wire breakage. New env key is optional (default 300 s). Deploy order: Go (annotations + route) → Python (normalization + gate + endpoint) → frontend (confirm card). Mixed versions are safe in both directions: an old Python ignores annotations (no gate, no catalog) while the new Go route is unused; a new Python against an old Go sees no annotations ⇒ confirm-all for registry tools (fail-safe) — call this out in the release note so the rollback path is understood.
- Rollback: set `MCP_ENABLED=false` to remove the feed tool surface entirely (the pre-change behavior for the agent), or roll back the release; there is no partial state to clean up.

## Open Questions

- Frontend affordance shape (inline card vs modal within the turn) — a frontend-task detail that does not affect specs or the backend approach.
