# Design

## Context

The dispatch funnel (`executor._dispatch_with_retries`) already orders every tool call through: confirmation gate (name-level, registry tools only, fail-closed on registry outage) → dispatch under two retry paths. Mutating-call confirmation is complete end-to-end: `ApprovalGate` → `confirm_request` frame → SPA card → decision store → closed denial via `denial_text_for`. Agent-native tools deliberately never gate. `net_guard` owns SSRF for `web_fetch` (URL validation + pinned-IP dialing); the GDPR audit table is a different concern (data-subject operations, not dispatch decisions). The funnel already canonicalizes `(tool, args)` into a string key for approval binding (`_canonical_args`) — policy matching can consume the identical serialization.

See proposal.md for motivation; behavior contract in specs/agent-tool-policy.

## Goals / Non-Goals

**Goals:**

- Deny evaluation ahead of the confirmation gate, uniform across agent-native and registry tools, including registry-outage runs.
- Fail-closed lifecycle: invalid configured policy refuses the boot; unset policy is fully inert.
- Audit trail (log + counter) without call content.
- Zero change to wire frames, retry taxonomy, confirmation semantics, Go side, SPA.

**Non-Goals:**

- Policy-driven confirmation or ask rules — the confirmation gate stays annotations-driven (`readOnlyHint`); this layer only denies.
- Hot reload of policy (mid-run edits inert by spec; restart to apply).
- Per-user or per-role policies (single global policy v1).
- Regex/JSONPath argument matching — glob on the canonical serialization only.
- Registry-side (Go) enforcement — the registry remains the data-authorization boundary (owner scoping); this is an agent-side intent filter.
- Content-side injection filtering of fetched web pages or feed items — deliberate rejection, see D7.

## Decisions

**D1 — Policy check lives in the dispatch funnel, before the confirmation-gate block; not inside `ToolDispatcher.call`.**
A denial must be a *closed outcome* (fixed text, no retry consumption, no re-prompt). Raising from the dispatcher would surface as `ToolError` and route into the arg-correction re-prompt path — the model would be invited to "fix" a call policy forbids. The funnel already owns closed-outcome denial plumbing for confirmations; policy reuses it. *Alternative considered*: dispatcher-level guard — rejected for retry-taxonomy misrouting.

**D2 — JSON policy file, pydantic-validated, snapshotted at boot; `AGENT_TOOL_POLICY_PATH` (default unset).**
Unset → the policy layer is inert (byte-identical dispatch). Set-but-invalid/unreadable → startup raises, process exits (same posture as `JWT_SECRET` validation: a misconfigured safety control must be loud, never a silent allow-all). JSON over YAML: stdlib `json` only — no new dependency, and the config is operator-authored and small. Snapshot: file is read once during wiring; mid-run edits are inert until restart (spec requirement).

**D3 — Rule schema: ordered list of `{id, action, tool, args?}`; first match wins; `action ∈ {allow, deny}`.**
`tool` and `args` are glob patterns (`fnmatch.fnmatchcase`); omitted `args` matches any arguments. `allow` short-circuits (later rules unseen; existing gates proceed unchanged) — this is what makes narrow surgical denies possible without blanket blocks. Args match against the funnel's canonical serialization so approval keying and policy matching see byte-identical strings; the tests must pin that the two consumers share one serializer (spec-drift guard: verify the matcher reads the same producer the confirmation gate keys on, not a parallel re-serialization). `id` is a short operator-chosen label surfaced in audit only.

**D4 — Denial reuses the confirmation-denial shape: one fixed constant string, emitted via the existing closed-outcome path.**
Same text for every rule and call (spec: static across rules), feeding `tool_result{ok:false}` and the react-loop `ToolMessage` exactly as confirmation denials do today. No new SSE kind.

**D5 — Audit = structured log line + `agent_tool_policy_decisions_total{tool, decision, rule}` counter.**
No new table: confirmation decisions are logged (not tabulated) today, policy denials are far lower-volume than the GDPR audit's compliance rows, and a DB row per decision creates retention surface for zero added value at this volume. Counts and identifiers only — never argument content (same stance as `audit.py` payload summaries).

**D6 — Coverage by construction: the funnel check evaluates before any tool-source branch.**
The check depends only on (name, args) — no registry I/O — so it applies identically to agent-native tools (which never reach confirmation) and to registry tools during a `tools/list` outage.

**D7 — The gate is the enforcement point; content-side injection filtering is rejected as an alternative.**
The threat is a delete intent the user never expressed, injected via untrusted content (fetched pages, feed items in RAG context, chat history). Inspecting that content for "injection" cannot be the control: prompt injection has no signature — it is natural language, so detectors (regex, classifier, LLM judge) are probabilistic in both directions (rephrased or non-imperative persuasion slips past; pages legitimately discussing feed management get flagged), and content inspection covers one ingress path while every path converges on the single choke point where the model emits a tool call. Vendor practice reflects this: tool-level guardrails are the industry standard precisely because content filtering is a treadmill. Delimiting fetched content ("data, not instructions") and prompt hardening remain welcome *complementary* probability-reducers in other changes — they lower the chance a bad call is emitted; only this gate guarantees the outcome when one is.

## Risks / Trade-offs

- [Glob over serialized JSON args is brittle (key order, whitespace)] → Matching consumes the funnel's canonical serializer; unit tests pin identical serialization for policy and approval keying; the policy file documents that `args` globs operate on the canonical form.
- [Over-broad denies silently break tool UX] → Fail-boot validation catches schema errors, not semantic over-blocking; mitigations are `allow` rules with first-match-wins, the audit counter making per-rule deny volume visible, and default-off rollout (unset = inert).
- [Policy file tamper disables the guard] → Same trust boundary as every operator setting on the host; the data-authorization layer (registry owner scoping) does not depend on this file, so tampering widens exposure to ask-only protection, not to cross-user data access.
- [Denied calls still cost a model turn] → Policy runs before dispatch but after the model emits the call; execution is blocked, not the attempt. Eval cases pin the observable outcome (blocked result + delivery marker), not prompt behavior.

## Migration Plan

Default-off: merging is behavior-neutral everywhere (`AGENT_TOOL_POLICY_PATH` unset). Enabling = set the env var + restart the agent process; rollback = unset + restart. No migration, no wire change, no coordinated rollout windows.

## Open Questions

None.
