# Design

## Context

See proposal.md — Why.

Current executor data flow: `_render_prior_data(state, step_id)` renders only `state["step_outputs"]` entries for earlier `rag` / `kb` / `tool` steps. A failed step never writes `step_outputs`, so later `llm` and `react` steps receive no failure signal. Failures exist only as SSE events in `state["events_so_far"]`: `tool_result{ok: false, error}` and `step_end{status: "failed", reason}`.

Current planner data flow: `_enforce_write_routing(steps, latest_user_message)` is a lexical guard over the most recent raw user message. It can reject a plan that omits a write for an explicit state-change message; it cannot force a write. The planner prompt receives the flattened conversation, so the previous assistant text is visible to the model but not to the deterministic guard.

Constraints:
- Small default model (Qwen3-4B, ~4096-token context). Prompt additions must stay bounded and small.
- `PRIOR_DATA_MAX_CHARS` / `PRIOR_DATA_PER_SECTION_MAX_CHARS` caps already exist and must keep holding.
- Clean-run prompts are pinned byte-identical by existing tests; the new context must be additive only on failure.
- The SPA request carries only `{role, content}`; the only durable next-turn carrier of failure information is the assistant answer text persisted in `agent.chat_messages.content`.

## Goals / Non-Goals

**Goals:**
- Make a failed step's safe error text and a no-false-success instruction reach every later model-facing step in the same run.
- Make a bare retry instruction after a reported not-executed write re-plan that write, subject to a fresh confirmation.
- Keep the failure description bounded, injection-safe, and derived from text already published on the wire.
- Leave the SPA request shape, SSE frame shapes, and history schema untouched.

**Non-Goals:**
- Sending structured tool-call history (`steps` / `tool_calls`) on the next request, or loading it from `chat_store` at run start. Deferred; the text route is sufficient for the reported-write case.
- Auto-retrying a failed write without a user instruction, or reusing an approval decision.
- Changing `_collect_failed_steps`, the partial-failure banner, or its trigger semantics.
- English retry cue coverage in the deterministic guard (prompt-only, matching the existing Chinese cue scope).

## Decisions

### D1. Derive failure context from terminal `step_end` events, not a new state channel

`step_end{status: "failed"}` is emitted only from `executor_node`'s exception path — a retry that recovers inside a step never reaches it. Event data is already checkpointed, so resumed runs carry it without new compatibility work. A dedicated `state["step_failures"]` channel was considered and rejected: it duplicates event data, adds a checkpoint surface that must be maintained on resume, and risks being mistaken for the `step_outputs` completion marker used by the resume-skip check.

A small helper renders sections from `state["plan"]` and `state["events_so_far"]`, limited to steps ordered before the current step. It is separate from `_collect_failed_steps` (which stays the banner's single source of truth and additionally applies the retry-allowance fallback); the two answer different questions and must not be merged.

### D2. Use only already-wire-safe error text

For `tool` steps the section carries the `error` field from that step's terminal `tool_result{ok: false}`. That text is produced by `_safe_tool_error_text` and is already delivered to the client and to react-loop `ToolMessage`s; the approval denial text is explicitly designed to be model-facing. For non-tool failures the section carries a static sentence and never `step_end.reason` (which can contain provider exception class names) or raw exception text.

Error text is rendered as *data* inside the prior-data block. The "report the failure, do not claim success" instruction is a separate static system message, so text inside an error cannot act as an instruction.

### D3. Failure data joins the existing prior-data block; the instruction is a separate static message

The failure sections are built alongside the existing data sections, in plan order, and pass through the same per-section cap, total cap, and marker stripping. The static instruction is appended as its own `SystemMessage` whenever at least one earlier step failed, regardless of whether the detail sections survived the total cap — so even a crowded prompt cannot tell the model the run succeeded. Clean runs add neither, preserving the byte-identical invariant.

The instruction also requires quoting a "tool was not executed" error verbatim. That verbatim quote is what makes the planner's deterministic retry guard (D4) work: the next turn's history is plain text, so the guard needs a stable phrase to key on.

Alternative considered: always reserve a fixed portion of the total cap for failures. Rejected as unnecessary complexity — the failing write is normally the most recent step before the answer, so the existing most-recent-first retention keeps it.

### D4. Retry-intent guard requires both a cue and a preceding not-executed write report

`_enforce_write_routing` gains a retry branch. It fires only when all of the following hold:
- the most recent user message contains a retry cue from a fixed Chinese cue set (再试试 / 再试一次 / 重试 / 重新试 / 再试下 / 再来一次 / 继续);
- the immediately preceding assistant message names a write tool (`create_feed` / `update_feed` / `delete_feed`) and carries a not-executed marker (未执行 / 没有执行 / not executed);
- no draft or negation qualifier is present in the latest message (reusing the existing negation markers plus retry negations such as 别重试 / 不要重试 / 先不要重试).

When it fires, the plan must contain that named write tool before the answering step, and the existing compose-before-write and content-limit rules apply. When any condition is absent, planning is unchanged. The blast radius of a false positive is bounded: the plan only schedules a write, and dispatch still requires a fresh confirmation.

Structured history (reading `chat_store` steps at run start) was considered as the deterministic alternative and deferred: it adds a run-start read, tenant/session authorization, and a new prompt channel for a case the text route already covers when the answer step follows the instruction.

### D5. No wire or persistence changes

The answer text is persisted exactly as today (`assistant_text_parts` → `chat_messages.content`), so the next turn's SPA resend carries the failure description with no backend change. No SSE frame, request field, or schema changes; the existing `note{kind:"partial_failure"}` banner remains the structured UI signal.

## Risks / Trade-offs

- [The model ignores the instruction and still claims success] → Static instruction plus verbatim-quote requirement plus a live/golden eval scenario; the banner remains the UI backstop.
- [A paraphrasing model omits the verbatim not-executed phrase, so the next-turn guard does not fire] → Prompt-level retry rule in the planner still applies; the deterministic guard is deliberately conservative (no blind write).
- [A huge earlier data section crowds the failure detail out of the total cap] → The static instruction is added independently of the detail sections, so the model is still told not to claim success; the failing write is normally the most recent section.
- [Retry cue false positive in a long conversation] → Only the immediately preceding assistant message is inspected, negations suppress the guard, and the retried write still requires a fresh confirmation.
- [Tool error text used as a prompt-injection vector] → Errors are server-generated, already wire-safe, rendered as data, and the instruction is static and separate.

## Migration Plan

No migration. Code-only change for the Python agent, with no schema or wire contract change. Rollback is a revert; no persisted state depends on the new prompt fields.
