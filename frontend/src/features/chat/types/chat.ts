import { z } from "zod";

// Chat wire schemas — zod is the source of truth for both runtime
// validation (so a malformed SSE frame never crashes the stream
// consumer) and the static types re-exported below. Field shapes
// mirror POST /api/chat's request body and the per-frame `data:`
// payloads documented in FUTURE_BACKEND.md §chat.
export const roleSchema = z.enum(["user", "assistant"]);
export const messageSchema = z.object({
  role: roleSchema,
  content: z.string().max(32768),
});
export const chatRequestSchema = z.object({
  messages: z.array(messageSchema).max(50),
  model: z.string().optional(),
  // rag:true asks the server to ground the answer in the
  // authenticated user's feeds via top-K semantic retrieval.
  // Omitted on the wire when false so today's non-RAG requests
  // look identical to before this field landed.
  rag: z.boolean().optional(),
  // run_id echoes the active chat session id (agent-chat-history-ui)
  // so the agent resumes that conversation instead of minting a new
  // one. Omitted when the tab has no active session yet — the first
  // server frame then carries the minted id and the store adopts it.
  run_id: z.string().optional(),
});

export type Role = z.infer<typeof roleSchema>;
export type Message = z.infer<typeof messageSchema>;
export type ChatRequest = z.infer<typeof chatRequestSchema>;

// Streaming events: each `data:` line on the SSE wire is parsed into
// one of these discriminated unions. `terminator` is the trailing
// `data: [DONE]` sentinel — we model it explicitly so consumers can
// stop on it without a magic-string check.
//
// Agent-loop variants (`plan` / `step_start` / `tool_call` /
// `tool_result` / `step_end`) are additive. Old clients ignore them;
// new clients render them as the agent makes progress. Field shapes
// mirror backend/internal/chat/handler.go's `writeAgentEvent`
// helpers byte-for-byte.
export type ChatDelta = { kind: "delta"; delta: string };
export type ChatDone = {
  kind: "done";
  usage: { prompt_tokens: number; completion_tokens: number; total_tokens: number };
};
export type ChatError = {
  kind: "error";
  error: string;
  request_id: string;
  // New in the agent path: a typed code ("plan_too_large",
  // "react_iterations_exceeded", "budget_exceeded",
  // "budget_unavailable", "plan_invalid", "auth_required",
  // "planner_failed"). Absent on legacy error frames; old clients
  // ignore the field, new clients can branch on it.
  code?: string;
};
export type ChatNote = { kind: "note"; text: string; provider: string };
export type ChatTerminator = { kind: "terminator" };

// New in the agent-tool-dispatch-retry slice: the orchestrator emits
// `note{kind: "partial_failure", failed_steps: [...]}` SSE frames
// just before the run's final `delta` frames when the run diverged
// from the plan. Parsed by chatService.ts into this discriminated
// shape and surfaced on the assistant message as `partialFailure`
// so `<PartialFailureBanner>` renders the warning above the final
// answer without expanding the `agent-steps` disclosure.
//
// The shape is additive — old clients that ignore unknown note
// kinds continue to render the final answer unchanged. The parser
// only emits this variant when the inner `kind === "partial_failure"`
// discriminator is present; legacy `note{text, provider}` frames
// flow through `ChatNote` exactly as before.
export type PartialFailureNote = {
  kind: "partial_failure";
  failedSteps: string[];
};
export type ChatPartialFailure = PartialFailureNote;

// Step kinds the planner may emit. Locked per agent-loop design §6;
// matching backend/internal/agent/types.go StepKind constants.
export const stepKindSchema = z.enum(["rag", "tool", "llm", "react"]);
export type ChatStepKind = z.infer<typeof stepKindSchema>;

// One entry in the planner's `plan` event. The backend allows a few
// kind-specific optional fields (tool name, args, prompt, query,
// max_iterations); all are optional here because the parser only
// needs the id + kind to drive the UI.
export interface ChatPlanStep {
  id: string;
  kind: ChatStepKind;
  tool?: string;
  args?: unknown;
  prompt?: string;
  query?: string;
  max_iterations?: number;
}

export type ChatPlan = {
  kind: "plan";
  steps: ChatPlanStep[];
};

export type ChatStepStart = {
  kind: "step_start";
  step_id: string;
  step_kind: ChatStepKind;
};

export type ChatToolCall = {
  kind: "tool_call";
  step_id: string;
  call_id: string;
  name: string;
  args: unknown;
};

export type ChatToolResult = {
  kind: "tool_result";
  step_id: string;
  call_id: string;
  ok: boolean;
  result?: unknown;
  error?: string;
  // Additive retry counter (agent-tool-dispatch-retry §3.2): retries made
  // for this dispatch — 0 on first-try success, N after N retries.
  // Absent on frames from servers that predate the field.
  retryCount?: number;
};

export type ChatStepEnd = {
  kind: "step_end";
  step_id: string;
  status: "ok" | "failed";
  reason: string;
};

// New in the agent-feed-tools slice: the orchestrator pauses a mutating
// registry call and asks the end user to confirm it. The frame carries
// the run id, the paused step, the tool name, its args (masked with the
// same policy as `tool_call` frames), the approval id the decision POST
// refers to, and the approval's expiry.
//
// `expires_at` is epoch seconds (a JSON number) — the Python producer's
// `ConfirmRequestEvent.expires_at: float` in
// backend/agent_py/agent/types.py. Not an ISO string.
//
// Additive kind: parsers that branch on the pre-change kinds only skip
// it, and every pre-change frame keeps its position + shape (spec
// "Clients that ignore the new kind are unaffected").
export type ChatConfirmRequest = {
  kind: "confirm_request";
  run_id: string;
  step_id: string;
  tool: string;
  args: unknown;
  approval_id: string;
  expires_at: number;
};

export type ChatEvent =
  | ChatDelta
  | ChatDone
  | ChatError
  | ChatNote
  | ChatTerminator
  | ChatPlan
  | ChatStepStart
  | ChatToolCall
  | ChatToolResult
  | ChatStepEnd
  | ChatPartialFailure
  | ChatConfirmRequest;

/**
 * Decision a user can record for a paused `confirm_request`.
 * Mirrors the `decision` field of `POST /api/chat/approvals`.
 */
export type ApprovalDecision = "approve" | "deny";

/**
 * Approval-card lifecycle: awaiting the user, decided + recorded, or
 * resolved dead — the decision POST answered the endpoint's typed 404
 * for an approval id that can no longer resolve (design.md D2: a
 * resumed run replays its checkpointed `confirm_request`, whose
 * approval id is expired).
 */
export type ApprovalOutcome = "pending" | "approved" | "denied" | "expired";

/**
 * UI-side record of an in-flight approval request on a step. Built from
 * a `confirm_request` SSE frame; the decision is recorded through
 * `POST /api/chat/approvals`. Ephemeral (in-memory only, like the rest
 * of the chat store) — a replayed session carries no approval cards.
 */
export interface ChatStepConfirm {
  runId: string;
  approvalId: string;
  tool: string;
  args: unknown;
  /** Epoch seconds, straight off the wire (see `ChatConfirmRequest`). */
  expiresAt: number;
  decision: ApprovalOutcome;
  // Set when the decision POST failed (the axios interceptor already
  // toasted it): a short static client-side hint, never server text.
  // The card stays pending so the user can retry. A 404 instead
  // resolves the card as `expired` and leaves this unset.
  error?: string;
}

/** `POST /api/chat/approvals` request body (agent-feed-tools §6.2). */
export interface ApprovalRequest {
  run_id: string;
  approval_id: string;
  decision: ApprovalDecision;
}

/**
 * UI-side per-step record. Lives on `ChatMessage` and is rebuilt as
 * the `step_start` / `tool_call` / `tool_result` / `step_end` events
 * stream. Deliberately a separate type from `ChatPlanStep` because
 * the UI cares about runtime state (status, latest tool call +
 * result, failure reason), not the planner's prompt / query.
 *
 * Not persisted to localStorage (FUTURE_FRONTEND.md §11 + XSS
 * surface); chat history is ephemeral and the `ChatMessage.steps`
 * array lives on the same in-memory state.
 */
export interface ChatStep {
  id: string;
  kind: ChatStepKind;
  status: "running" | "ok" | "failed";
  toolName?: string;
  toolArgs?: unknown;
  toolResult?: unknown;
  reason?: string;
  // Retries made for the step's latest tool dispatch (latest-result
  // semantics, like toolResult). Rendered as a small badge when > 0.
  retryCount?: number;
  // Present while a mutating call on this step is awaiting (or has
  // completed) an approve/deny decision. Drives the inline
  // confirmation card; absent on steps with no confirmation.
  confirm?: ChatStepConfirm;
}

/**
 * Thrown by `chatService.streamMessage` when the stream errors out —
 * either via an SSE `event: error` frame (server mid-stream error,
 * safe static `error` text + request id) or via a non-2xx HTTP
 * response before the stream opens (where we surface the envelope
 * `error` field, but never `err.Error()` — see SECURITY.md H7).
 *
 * The `event` shape lets callers distinguish the two failure modes
 * without parsing strings; both shapes carry a `kind` discriminator.
 */
export class ChatStreamError extends Error {
  constructor(
    public readonly event:
      | ChatError
      | { kind: "http"; status: number; message: string },
  ) {
    super(event.kind === "http" ? `HTTP ${event.status}: ${event.message}` : event.error);
    this.name = "ChatStreamError";
  }
}

// ---- Chat session history (openspec/changes/agent-chat-history-ui) ----
//
// Wire shapes for the session REST surface the Go chat proxy fronts
// (`GET|POST|DELETE /api/chat/sessions[...]`). Field names mirror the
// `agent.chat_sessions` / `agent.chat_messages` columns the Python
// agent reads back; no SSE frame changes ride along with this slice.

/** One sidebar row — `GET /api/chat/sessions` entry. */
export interface SessionSummary {
  id: string;
  user_id: number;
  /** Server-stored title; `""` until a summariser fills it in. */
  title: string;
  started_at: string;
  last_active_at: string;
  ended_at: string | null;
  message_count: number;
}

/** One replayed message — `GET /api/chat/sessions/{id}/messages` entry. */
export interface SessionMessage {
  id: number;
  // Slice 1 records `user` and `assistant`; `tool` / `system` are
  // allowed by the schema, so the replay mapper keeps this open and
  // renders anything non-`user` as an assistant row.
  role: string;
  content: string;
  // The wire envelope's tool invocations, byte-faithful from the SSE
  // stream (`{step_id, call_id, name, args}`). `null` when the
  // message carries none.
  tool_calls: unknown[] | null;
  tokens: number;
  created_at: string;
}

/**
 * Thrown by the JSON session endpoints in `chatService` on a non-2xx
 * response. The status rides on the error so the store can tell a 404
 * (session deleted / reaped — clear the active session quietly, no
 * toast, no console error) from a transient failure (keep the state
 * and retry later).
 */
export class ChatApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ChatApiError";
  }
}
