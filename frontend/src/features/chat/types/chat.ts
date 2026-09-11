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
});

export type Role = z.infer<typeof roleSchema>;
export type Message = z.infer<typeof messageSchema>;
export type ChatRequest = z.infer<typeof chatRequestSchema>;

// Streaming events: each `data:` line on the SSE wire is parsed into
// one of these discriminated unions. `terminator` is the trailing
// `data: [DONE]` sentinel — we model it explicitly so consumers can
// stop on it without a magic-string check.
export type ChatDelta = { kind: "delta"; delta: string };
export type ChatDone = {
  kind: "done";
  usage: { prompt_tokens: number; completion_tokens: number; total_tokens: number };
};
export type ChatError = { kind: "error"; error: string; request_id: string };
export type ChatTerminator = { kind: "terminator" };
export type ChatEvent = ChatDelta | ChatDone | ChatError | ChatTerminator;

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
