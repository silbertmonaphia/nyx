import type { ApiError } from "~/api/openapi";
import { tokenStore, refreshTokensAndReplay } from "~/services/api";
import { injectTraceparent } from "~/services/telemetry";
import {
  ChatStreamError,
  type ChatEvent,
  type ChatRequest,
  type Message,
} from "../types/chat";

// POST /api/chat. The wrapped `api` axios instance can't carry SSE
// (responseType: 'stream' isn't supported and buffering would defeat
// the point), so we hit the route with raw `fetch`, manually stamp
// the Bearer + traceparent headers (axios interceptors don't run on
// fetch), and parse `text/event-stream` frames inline.
const CHAT_URL = `${import.meta.env.VITE_API_URL || "http://localhost:8080/api"}/chat`;

// SSE field delimiter — the spec defines `\r\n\r\n` as the frame
// terminator but most servers (and the browser's native EventSource)
// emit `\n\n`. Tolerate both so we don't lose frames if the backend
// ever switches.
const SSE_FRAME_DELIMITERS = ["\n\n", "\r\n\r\n"];

/**
 * Build the request headers for a chat call. Bearer stamping mirrors
 * the axios request interceptor (services/api.ts:165-179); traceparent
 * is stamped via the same `injectTraceparent` helper the axios path
 * uses so backend spans join the SPA's root.
 */
function buildHeaders(): Record<string, string> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    Accept: "text/event-stream",
  };
  const at = tokenStore.getAccessToken();
  if (at) {
    headers.Authorization = `Bearer ${at}`;
  }
  injectTraceparent(headers);
  return headers;
}

/**
 * POST /api/chat and yield the parsed SSE event stream as an async
 * generator. The generator:
 *  - On non-2xx, throws a `ChatStreamError` of kind `http`. The error
 *    envelope is the same `{ error, code, request_id, details }` shape
 *    axios decodes — SECURITY.md H7 means we surface `error` verbatim
 *    to the caller, but the UI layer is responsible for NOT echoing
 *    it to the toast.
 *  - On 401 with an access token present, runs the single-flight
 *    refresh via `refreshTokensAndReplay` and replays the request
 *    once. A replayed 401 throws (the refresh token is gone — only
 *    correct outcome is logout).
 *  - On 2xx, walks the `ReadableStream` chunk by chunk, accumulating
 *    a line buffer and dispatching one event per SSE frame.
 *  - Honors `signal`: aborting the caller aborts the underlying fetch
 *    so the body reader closes and the generator returns.
 */
export const chatService = {
  async *streamMessage(
    messages: Message[],
    signal: AbortSignal,
  ): AsyncGenerator<ChatEvent> {
    const body: ChatRequest = { messages };
    // `model` is intentionally omitted — the backend ignores it for
    // v1 and the brief explicitly forbids a model picker in the UI.
    let response = await fetch(CHAT_URL, {
      method: "POST",
      headers: buildHeaders(),
      body: JSON.stringify(body),
      signal,
    });

    if (response.status === 401 && tokenStore.getAccessToken() !== null) {
      // Mirror the axios interceptor's refresh-on-401 contract.
      // `refreshTokensAndReplay` is single-flight, so concurrent
      // 401s (e.g. one from this fetch + one from a parallel axios
      // call) collapse onto the same refresh.
      const ok = await refreshTokensAndReplay();
      if (ok) {
        response = await fetch(CHAT_URL, {
          method: "POST",
          headers: buildHeaders(),
          body: JSON.stringify(body),
          signal,
        });
      }
    }

    if (!response.ok) {
      // Pull the standard envelope so callers see the same shape as
      // axios errors. Don't trust `err.Error()` to be safe to ship
      // (SECURITY.md H7) — only the server-provided `error` field
      // reaches the wire.
      let envelope: ApiError | null = null;
      try {
        envelope = (await response.json()) as ApiError;
      } catch {
        // Body wasn't JSON — fall back to a generic message.
      }
      const message = envelope?.error ?? `Chat request failed (${response.status})`;
      throw new ChatStreamError({
        kind: "http",
        status: response.status,
        message,
      });
    }

    if (!response.body) {
      throw new ChatStreamError({
        kind: "http",
        status: 500,
        message: "Chat response had no body",
      });
    }

    const reader = response.body.getReader();
    const decoder = new TextDecoder("utf-8");
    let buffer = "";
    let currentEvent: string | null = null;
    let currentData: string[] = [];

    try {
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });

        // A frame may be split across chunks — split on every known
        // delimiter and keep any trailing partial in `buffer`.
        let consumed = 0;
        while (true) {
          let idx = -1;
          let delimiterLen = 0;
          for (const d of SSE_FRAME_DELIMITERS) {
            const i = buffer.indexOf(d, consumed);
            if (i !== -1 && (idx === -1 || i < idx)) {
              idx = i;
              delimiterLen = d.length;
            }
          }
          if (idx === -1) break;

          const frame = buffer.slice(consumed, idx);
          consumed = idx + delimiterLen;

          // Reset per-frame state and walk the frame's lines. SSE
          // comments start with `:`; fields are `name: value`.
          currentEvent = null;
          currentData = [];
          for (const rawLine of frame.split(/\r?\n/)) {
            if (rawLine === "" || rawLine.startsWith(":")) continue;
            const colon = rawLine.indexOf(":");
            const field = colon === -1 ? rawLine : rawLine.slice(0, colon);
            let value = colon === -1 ? "" : rawLine.slice(colon + 1);
            if (value.startsWith(" ")) value = value.slice(1);
            if (field === "event") {
              currentEvent = value;
            } else if (field === "data") {
              currentData.push(value);
            }
            // `id` / `retry` are ignored — the brief defers
            // EventSource-parser until we need them.
          }

          const dataStr = currentData.join("\n");
          if (dataStr === "[DONE]") {
            yield { kind: "terminator" };
            return;
          }
          if (!dataStr) continue;
          if (currentEvent === "delta") {
            try {
              const parsed = JSON.parse(dataStr) as { delta?: unknown };
              if (typeof parsed.delta === "string") {
                yield { kind: "delta", delta: parsed.delta };
              }
            } catch {
              // Malformed JSON — skip rather than tear the stream down.
              // The server's framing is the source of truth; a single
              // bad frame shouldn't kill the whole response.
            }
          } else if (currentEvent === "done") {
            try {
              const parsed = JSON.parse(dataStr) as {
                usage?: {
                  prompt_tokens?: unknown;
                  completion_tokens?: unknown;
                  total_tokens?: unknown;
                };
              };
              const u = parsed.usage;
              if (
                u &&
                typeof u.prompt_tokens === "number" &&
                typeof u.completion_tokens === "number" &&
                typeof u.total_tokens === "number"
              ) {
                yield {
                  kind: "done",
                  usage: {
                    prompt_tokens: u.prompt_tokens,
                    completion_tokens: u.completion_tokens,
                    total_tokens: u.total_tokens,
                  },
                };
              }
            } catch {
              // Treat malformed done as terminal — the server chose to
              // end the stream; the UI just won't show usage.
            }
            yield { kind: "terminator" };
            return;
          } else if (currentEvent === "error") {
            try {
              const parsed = JSON.parse(dataStr) as {
                error?: unknown;
                request_id?: unknown;
              };
              yield {
                kind: "error",
                error:
                  typeof parsed.error === "string"
                    ? parsed.error
                    : "Chat stream error",
                request_id:
                  typeof parsed.request_id === "string"
                    ? parsed.request_id
                    : "",
              };
            } catch {
              yield {
                kind: "error",
                error: "Chat stream error",
                request_id: "",
              };
            }
            yield { kind: "terminator" };
            return;
          }
          // Unrecognised event types are ignored — the brief defers
          // new event types until we need them.
        }
        if (consumed > 0) {
          buffer = buffer.slice(consumed);
        }
      }
    } finally {
      // The generator's `finally` runs on early return, throw, and
      // caller-driven `break`. Release the reader so the socket can
      // close — otherwise a cancelled stream leaks a pending fetch.
      try {
        reader.releaseLock();
      } catch {
        // already released — ignore
      }
    }
  },
};
