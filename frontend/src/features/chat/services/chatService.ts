import type { ApiError } from "~/api/openapi";
import { t } from "~/i18n";
import api, { tokenStore, refreshTokensAndReplay } from "~/services/api";
import { injectTraceparent } from "~/services/telemetry";
import {
  ChatApiError,
  ChatStreamError,
  type ApprovalRequest,
  type ChatEvent,
  type ChatRequest,
  type Message,
  type SessionMessage,
  type SessionSummary,
} from "../types/chat";

const API_BASE = import.meta.env.VITE_API_URL || "http://localhost:8080/api";

// POST /api/chat. The wrapped `api` axios instance can't carry SSE
// (responseType: 'stream' isn't supported and buffering would defeat
// the point), so we hit the route with raw `fetch`, manually stamp
// the Bearer + traceparent headers (axios interceptors don't run on
// fetch), and parse `text/event-stream` frames inline.
const CHAT_URL = `${API_BASE}/chat`;

// Session REST surface (agent-chat-history-ui). Same transport
// posture as the SSE call — raw `fetch` — because the wrapped axios
// instance toasts every non-2xx through the global error handler and
// the sidebar's failure contract is a quiet empty state.
const SESSIONS_URL = `${API_BASE}/chat/sessions`;

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
function buildHeaders(accept = "text/event-stream"): Record<string, string> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    Accept: accept,
  };
  const at = tokenStore.getAccessToken();
  if (at) {
    headers.Authorization = `Bearer ${at}`;
  }
  injectTraceparent(headers);
  return headers;
}

/**
 * JSON round-trip for the session endpoints. Mirrors `streamMessage`'s
 * transport (raw `fetch`, hand-stamped Bearer + traceparent, one replay
 * after the shared single-flight refresh on 401) so both halves of the
 * chat API behave identically behind an expired access token.
 *
 * Non-2xx throws `ChatApiError` carrying the status — the store tells a
 * 404 (session gone) from a transient failure off it. The message is a
 * static client-side string: the backend's `error` field is not safe to
 * echo to the UI (SECURITY.md H7), and the sidebar has no toast to fill.
 */
async function requestJson<T>(
  url: string,
  init: RequestInit = {},
): Promise<T> {
  const send = (): Promise<Response> =>
    fetch(url, { ...init, headers: buildHeaders("application/json") });

  let response = await send();
  if (response.status === 401 && tokenStore.getAccessToken() !== null) {
    const ok = await refreshTokensAndReplay();
    if (ok) {
      response = await send();
    }
  }

  if (!response.ok) {
    throw new ChatApiError(
      response.status,
      t('chat.errors.requestFailed', { status: response.status }),
    );
  }

  // DELETE answers 204 (or an empty 200) — don't feed `""` to JSON.parse.
  const text = await response.text();
  if (!text) {
    return undefined as T;
  }
  try {
    return JSON.parse(text) as T;
  } catch {
    throw new ChatApiError(response.status, t("chat.errors.invalidJson"));
  }
}

/**
 * Coerce a session-list row off the wire. Returns null for rows without
 * a usable id — a malformed entry is dropped rather than rendered as a
 * broken sidebar row. Optional fields collapse to safe defaults so the
 * component never has to null-check.
 */
function toSessionSummary(raw: unknown): SessionSummary | null {
  if (!raw || typeof raw !== "object") return null;
  const row = raw as Record<string, unknown>;
  if (typeof row.id !== "string" || row.id === "") return null;
  return {
    id: row.id,
    user_id: typeof row.user_id === "number" ? row.user_id : 0,
    title: typeof row.title === "string" ? row.title : "",
    started_at: typeof row.started_at === "string" ? row.started_at : "",
    last_active_at:
      typeof row.last_active_at === "string" ? row.last_active_at : "",
    ended_at: typeof row.ended_at === "string" ? row.ended_at : null,
    message_count:
      typeof row.message_count === "number" ? row.message_count : 0,
  };
}

/**
 * Coerce a replayed message off the wire. Rows without a role or
 * content are dropped: rendering a half-read row would misrepresent the
 * conversation. `tool_calls` is kept opaque — the store maps it to the
 * agent-step shape it renders.
 */
function toSessionMessage(raw: unknown): SessionMessage | null {
  if (!raw || typeof raw !== "object") return null;
  const row = raw as Record<string, unknown>;
  if (typeof row.role !== "string" || typeof row.content !== "string") {
    return null;
  }
  return {
    id: typeof row.id === "number" ? row.id : 0,
    role: row.role,
    content: row.content,
    tool_calls: Array.isArray(row.tool_calls) ? row.tool_calls : null,
    tokens: typeof row.tokens === "number" ? row.tokens : 0,
    created_at: typeof row.created_at === "string" ? row.created_at : "",
  };
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
 *    a line buffer and dispatching one event per SSE frame. The first
 *    frame (`event: run_id`) is routed to `opts.onRunId` instead of
 *    the yielded union — the session id is state, not stream content.
 *  - Honors `signal`: aborting the caller aborts the underlying fetch
 *    so the body reader closes and the generator returns.
 */
export const chatService = {
  async *streamMessage(
    messages: Message[],
    signal: AbortSignal,
    opts: {
      /**
       * Active session id. Sent as `run_id` so the agent resumes that
       * session (and does not re-record the replayed user messages)
       * instead of minting a fresh one. Omitted when the tab has no
       * active session yet.
       */
      runId?: string | null;
      /**
       * Called with the server-minted run id from the stream's first
       * frame (`event: run_id`). Always surfaced; the store decides
       * whether to adopt it as the active session (it only does so
       * when this tab had none).
       */
      onRunId?: (id: string) => void;
    } = {},
  ): AsyncGenerator<ChatEvent> {
    const body: ChatRequest = { messages };
    if (opts.runId) {
      body.run_id = opts.runId;
    }
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
      const message =
        envelope?.error ?? t('chat.errors.requestFailed', { status: response.status });
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
        message: t("chat.errors.noBody"),
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
          if (currentEvent === "run_id") {
            // First frame of every run. Surfaced through the callback
            // rather than the event union so the existing consumers
            // (whose for-await loops exhaustively match on `kind`) are
            // untouched — this slice adds no new ChatEvent variant.
            try {
              const parsed = JSON.parse(dataStr) as { run_id?: unknown };
              if (typeof parsed.run_id === "string" && parsed.run_id !== "") {
                opts.onRunId?.(parsed.run_id);
              }
            } catch {
              // Drop a malformed run_id frame — the stream continues
              // and the tab simply doesn't adopt the session.
            }
          } else if (currentEvent === "delta") {
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
          } else if (currentEvent === "note") {
            try {
              const parsed = JSON.parse(dataStr) as {
                text?: unknown;
                provider?: unknown;
                kind?: unknown;
                failed_steps?: unknown;
              };
              if (
                parsed.kind === "partial_failure" &&
                Array.isArray(parsed.failed_steps) &&
                parsed.failed_steps.every((s) => typeof s === "string")
              ) {
                // agent-tool-dispatch-retry: the run diverged from the
                // plan; the UI renders a banner above the final answer.
                // This variant carries `provider: null`, so it must be
                // matched before the legacy text/provider check below.
                yield {
                  kind: "partial_failure",
                  failedSteps: parsed.failed_steps as string[],
                };
              } else if (
                typeof parsed.text === "string" &&
                typeof parsed.provider === "string"
              ) {
                yield { kind: "note", text: parsed.text, provider: parsed.provider };
              }
            } catch {
              // Malformed note frame — drop it; mirrors the error-frame
              // precedent so a bad frame can't tear down the stream.
            }
          } else if (currentEvent === "error") {
            try {
              const parsed = JSON.parse(dataStr) as {
                error?: unknown;
                request_id?: unknown;
                code?: unknown;
              };
              yield {
                kind: "error",
                error:
                  typeof parsed.error === "string"
                    ? parsed.error
                    : t("chat.errors.streamError"),
                request_id:
                  typeof parsed.request_id === "string"
                    ? parsed.request_id
                    : "",
                code: typeof parsed.code === "string" ? parsed.code : undefined,
              };
            } catch {
              yield {
                kind: "error",
                error: t("chat.errors.streamError"),
                request_id: "",
              };
            }
            yield { kind: "terminator" };
            return;
          } else if (currentEvent === "plan") {
            // Agent-loop `plan` event — once per run, before any
            // step. Steps are seeded with whatever the planner
            // emitted (id + kind + kind-specific optionals). The
            // store mirrors the array verbatim so each step has a
            // row to update as `step_start` / `tool_call` / etc.
            // stream. Malformed frames drop silently — a bad plan
            // shouldn't tear the whole stream down.
            try {
              const parsed = JSON.parse(dataStr) as {
                steps?: unknown;
              };
              if (Array.isArray(parsed.steps)) {
                const steps: import("../types/chat").ChatPlanStep[] = [];
                for (const s of parsed.steps) {
                  if (!s || typeof s !== "object") continue;
                  const obj = s as Record<string, unknown>;
                  if (typeof obj.id !== "string") continue;
                  if (
                    obj.kind !== "rag" &&
                    obj.kind !== "tool" &&
                    obj.kind !== "llm" &&
                    obj.kind !== "react"
                  ) {
                    continue;
                  }
                  steps.push({
                    id: obj.id,
                    kind: obj.kind,
                    tool: typeof obj.tool === "string" ? obj.tool : undefined,
                    args: obj.args,
                    prompt:
                      typeof obj.prompt === "string" ? obj.prompt : undefined,
                    query:
                      typeof obj.query === "string" ? obj.query : undefined,
                    max_iterations:
                      typeof obj.max_iterations === "number"
                        ? obj.max_iterations
                        : undefined,
                  });
                }
                yield { kind: "plan", steps };
              }
            } catch {
              // Drop malformed plan frame; the run trace will be
              // partial but the stream stays alive.
            }
          } else if (currentEvent === "step_start") {
            try {
              const parsed = JSON.parse(dataStr) as {
                step_id?: unknown;
                kind?: unknown;
              };
              const id =
                typeof parsed.step_id === "string" ? parsed.step_id : null;
              const kind = parsed.kind;
              if (
                id !== null &&
                (kind === "rag" ||
                  kind === "tool" ||
                  kind === "llm" ||
                  kind === "react")
              ) {
                yield {
                  kind: "step_start",
                  step_id: id,
                  step_kind: kind,
                };
              }
            } catch {
              // Drop malformed step_start frame.
            }
          } else if (currentEvent === "confirm_request") {
            // Mutating-call confirmation (agent-feed-tools §6.1): the
            // run is paused inside the same stream until the decision
            // POST lands. Additive kind — the parser skips unknown
            // kinds anyway, so pre-change consumers are unaffected.
            // Required fields per the spec: run id, step id, tool,
            // args (masked like `tool_call`), approval id, expiry.
            // `expires_at` is epoch seconds — a JSON number matching
            // the producer's `float` field, not an ISO string. A frame
            // missing any required field is dropped; a bad frame must
            // not tear the stream down.
            try {
              const parsed = JSON.parse(dataStr) as {
                run_id?: unknown;
                step_id?: unknown;
                tool?: unknown;
                args?: unknown;
                approval_id?: unknown;
                expires_at?: unknown;
              };
              if (
                typeof parsed.run_id === "string" &&
                parsed.run_id !== "" &&
                typeof parsed.step_id === "string" &&
                parsed.step_id !== "" &&
                typeof parsed.tool === "string" &&
                parsed.tool !== "" &&
                typeof parsed.approval_id === "string" &&
                parsed.approval_id !== "" &&
                typeof parsed.expires_at === "number"
              ) {
                yield {
                  kind: "confirm_request",
                  run_id: parsed.run_id,
                  step_id: parsed.step_id,
                  tool: parsed.tool,
                  args: parsed.args,
                  approval_id: parsed.approval_id,
                  expires_at: parsed.expires_at,
                };
              } else {
                // A producer-side wire drift (renamed field, changed
                // type) would otherwise silently present "no card" for
                // the whole approval TTL — make it diagnosable
                // (review round 1).
                console.warn(
                  "chat: confirm_request frame missing required fields",
                  parsed,
                );
              }
            } catch {
              // Drop malformed confirm_request frame — warn so wire
              // drift is diagnosable instead of silently hiding the
              // card (review round 1).
              console.warn("chat: malformed confirm_request frame", dataStr);
            }
          } else if (currentEvent === "tool_call") {
            try {
              const parsed = JSON.parse(dataStr) as {
                step_id?: unknown;
                call_id?: unknown;
                name?: unknown;
                args?: unknown;
              };
              if (
                typeof parsed.step_id === "string" &&
                typeof parsed.call_id === "string" &&
                typeof parsed.name === "string"
              ) {
                yield {
                  kind: "tool_call",
                  step_id: parsed.step_id,
                  call_id: parsed.call_id,
                  name: parsed.name,
                  args: parsed.args,
                };
              }
            } catch {
              // Drop malformed tool_call frame.
            }
          } else if (currentEvent === "tool_result") {
            try {
              const parsed = JSON.parse(dataStr) as {
                step_id?: unknown;
                call_id?: unknown;
                ok?: unknown;
                result?: unknown;
                error?: unknown;
                retry_count?: unknown;
              };
              if (
                typeof parsed.step_id === "string" &&
                typeof parsed.call_id === "string"
              ) {
                yield {
                  kind: "tool_result",
                  step_id: parsed.step_id,
                  call_id: parsed.call_id,
                  ok: parsed.ok === true,
                  result: parsed.ok === true ? parsed.result : undefined,
                  error:
                    typeof parsed.error === "string" ? parsed.error : undefined,
                  // Additive (agent-tool-dispatch-retry §3.2): absent on
                  // frames from older servers, in which case the step row
                  // simply shows no retry badge.
                  retryCount:
                    typeof parsed.retry_count === "number"
                      ? parsed.retry_count
                      : undefined,
                };
              }
            } catch {
              // Drop malformed tool_result frame.
            }
          } else if (currentEvent === "step_end") {
            try {
              const parsed = JSON.parse(dataStr) as {
                step_id?: unknown;
                status?: unknown;
                reason?: unknown;
              };
              if (
                typeof parsed.step_id === "string" &&
                (parsed.status === "ok" || parsed.status === "failed")
              ) {
                yield {
                  kind: "step_end",
                  step_id: parsed.step_id,
                  status: parsed.status,
                  reason:
                    typeof parsed.reason === "string" ? parsed.reason : "",
                };
              }
            } catch {
              // Drop malformed step_end frame.
            }
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

  /**
   * POST /api/chat/approvals — record the user's approve/deny decision
   * for a paused `confirm_request` (agent-feed-tools §6.2).
   *
   * Deliberately the wrapped axios client (not the raw-`fetch` posture
   * the SSE call uses): the request interceptor stamps the Bearer JWT
   * and the traceparent, the response interceptor runs the shared
   * single-flight refresh on a 401, and a non-2xx surfaces through the
   * global error toast. The Go proxy forwards the body to the Python
   * agent, whose paused run resolves the decision from Redis.
   *
   * Resolves on any 2xx (the agent answers 200 for the first decision
   * and for idempotent duplicates); rejects otherwise, leaving the
   * caller to keep the card pending for a retry.
   */
  async submitApproval(body: ApprovalRequest): Promise<void> {
    await api.post("/chat/approvals", body);
  },

  /**
   * GET /api/chat/sessions — one page of the caller's sessions, ordered
   * `last_active_at DESC`. `before` is the opaque `next_cursor` from the
   * previous page (omit for the first page); `nextCursor === null` means
   * the list is complete. `limit` is left to the server default when
   * omitted so `PYTHON_AGENT_SESSIONS_PAGE_SIZE` stays the source of
   * truth for the page size.
   */
  async listSessions(
    opts: { limit?: number; before?: string | null } = {},
  ): Promise<{ sessions: SessionSummary[]; nextCursor: string | null }> {
    const params = new URLSearchParams();
    if (typeof opts.limit === "number") {
      params.set("limit", String(opts.limit));
    }
    if (opts.before) {
      params.set("before", opts.before);
    }
    const qs = params.toString();
    const data = await requestJson<{
      sessions?: unknown;
      next_cursor?: unknown;
    }>(qs ? `${SESSIONS_URL}?${qs}` : SESSIONS_URL);

    const rows = Array.isArray(data?.sessions) ? data.sessions : [];
    return {
      sessions: rows
        .map(toSessionSummary)
        .filter((s): s is SessionSummary => s !== null),
      nextCursor: typeof data?.next_cursor === "string" && data.next_cursor !== ""
        ? data.next_cursor
        : null,
    };
  },

  /**
   * GET /api/chat/sessions/{id}/messages — the session's messages in
   * chronological order. 404 (deleted / reaped / another user's
   * session) throws `ChatApiError` with `status: 404`.
   */
  async getMessages(sessionId: string): Promise<SessionMessage[]> {
    const data = await requestJson<{ messages?: unknown }>(
      `${SESSIONS_URL}/${encodeURIComponent(sessionId)}/messages`,
    );
    const rows = Array.isArray(data?.messages) ? data.messages : [];
    return rows
      .map(toSessionMessage)
      .filter((m): m is SessionMessage => m !== null);
  },

  /**
   * POST /api/chat/sessions — mint an empty session owned by the
   * caller. The returned id is the uuid4 the agent uses as the run id,
   * so it round-trips through the `run_id` request field.
   */
  async createSession(): Promise<{ session_id: string; started_at: string }> {
    const data = await requestJson<{
      session_id?: unknown;
      started_at?: unknown;
    }>(SESSIONS_URL, { method: "POST" });
    if (typeof data?.session_id !== "string" || data.session_id === "") {
      throw new ChatApiError(200, t("chat.errors.sessionNoId"));
    }
    return {
      session_id: data.session_id,
      started_at: typeof data.started_at === "string" ? data.started_at : "",
    };
  },

  /**
   * POST /api/chat/sessions/{id}/resume — refresh `last_active_at` so
   * the session ranks at the top of the next list page and the
   * retention cron treats it as live. 404 → the session is gone.
   */
  async resumeSession(sessionId: string): Promise<void> {
    await requestJson(
      `${SESSIONS_URL}/${encodeURIComponent(sessionId)}/resume`,
      { method: "POST" },
    );
  },

  /**
   * DELETE /api/chat/sessions/{id} — soft-delete. Any 2xx resolves; a
   * 404 throws `ChatApiError` (the row is already invisible to the
   * list, so the caller can drop it locally either way).
   */
  async deleteSession(sessionId: string): Promise<void> {
    await requestJson(`${SESSIONS_URL}/${encodeURIComponent(sessionId)}`, {
      method: "DELETE",
    });
  },
};
