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
};

export type ChatStepEnd = {
  kind: "step_end";
  step_id: string;
  status: "ok" | "failed";
  reason: string;
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
  | ChatStepEnd;

/**
 * UI-side per-step record. Lives on `ChatMessage` and is rebuilt as
 * the `step_start` / `tool_call` / `tool_result` / `step_end` events
 * stream. Deliberately a separate type from `ChatPlanStep` because
 * the UI cares about runtime state (status, latest tool call +
 * result, failure reason), not the planner's prompt / query.
 *
 * Not persisted to localStorage (FUTURE_FRONTEND.md §10 + XSS
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