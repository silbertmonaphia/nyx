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
  type SessionStep,
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

const RUN_STREAM_URL_BASE = `${API_BASE}/chat/runs`;

const SSE_KEEPALIVE_INTERVAL_MS = 15_000;

export const READ_IDLE_TIMEOUT_MS = SSE_KEEPALIVE_INTERVAL_MS * 3;

class ReadIdleError extends Error {
  constructor() {
    super("chat read idle timeout");
    this.name = "ReadIdleError";
  }
}

async function readChunkWithWatchdog(
  reader: ReadableStreamDefaultReader<Uint8Array>,
): Promise<ReadableStreamReadResult<Uint8Array>> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const idle = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new ReadIdleError()), READ_IDLE_TIMEOUT_MS);
  });
  idle.catch(() => {});
  try {
    return await Promise.race([reader.read(), idle]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

async function* consumeEventStream(
  response: Response,
  chained: AbortController,
  signal: AbortSignal,
  onRunId: ((id: string) => void) | undefined,
): AsyncGenerator<ChatEvent> {
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
  let sawTerminal = false;
  let frameId: number | undefined;
  const withFrameId = (event: ChatEvent): ChatEvent =>
    frameId === undefined ? event : { ...event, frameId };
  try {
    while (true) {
      let read: ReadableStreamReadResult<Uint8Array>;
      try {
        read = await readChunkWithWatchdog(reader);
      } catch (err) {
        if (err instanceof ReadIdleError) {
          chained.abort();
          yield { kind: "stream_incomplete" };
          return;
        }
        if (signal.aborted) throw err;
        yield { kind: "stream_incomplete" };
        return;
      }
      if (read.done) break;
      buffer += decoder.decode(read.value, { stream: true });

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

        let currentEvent: string | null = null;
        const currentData: string[] = [];
        frameId = undefined;
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
          } else if (field === "id") {
            if (/^\d+$/.test(value)) {
              const parsedId = Number.parseInt(value, 10);
              if (Number.isSafeInteger(parsedId)) {
                frameId = parsedId;
              }
            }
          }
        }

        const dataStr = currentData.join("\n");
        if (dataStr === "[DONE]") {
          sawTerminal = true;
          yield withFrameId({ kind: "terminator" });
          return;
        }
        if (!dataStr) continue;
        if (currentEvent === "run_id") {
          try {
            const parsed = JSON.parse(dataStr) as { run_id?: unknown };
            if (typeof parsed.run_id === "string" && parsed.run_id !== "") {
              onRunId?.(parsed.run_id);
            }
          } catch {}
        } else if (currentEvent === "delta") {
          try {
            const parsed = JSON.parse(dataStr) as { delta?: unknown };
            if (typeof parsed.delta === "string") {
              yield withFrameId({ kind: "delta", delta: parsed.delta });
            }
          } catch {}
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
              yield withFrameId({
                kind: "done",
                usage: {
                  prompt_tokens: u.prompt_tokens,
                  completion_tokens: u.completion_tokens,
                  total_tokens: u.total_tokens,
                },
              });
            }
          } catch {}
          sawTerminal = true;
          yield withFrameId({ kind: "terminator" });
          return;
        } else if (currentEvent === "note") {
          try {
            const parsed = JSON.parse(dataStr) as {
              text?: unknown;
              provider?: unknown;
              kind?: unknown;
              failed_steps?: unknown;
            };
            if (parsed.kind === "replay_truncated") {
              yield withFrameId({ kind: "replay_truncated" });
            } else if (
              parsed.kind === "partial_failure" &&
              Array.isArray(parsed.failed_steps) &&
              parsed.failed_steps.every((s) => typeof s === "string")
            ) {
              yield withFrameId({
                kind: "partial_failure",
                failedSteps: parsed.failed_steps as string[],
              });
            } else if (
              typeof parsed.text === "string" &&
              typeof parsed.provider === "string"
            ) {
              yield withFrameId({
                kind: "note",
                text: parsed.text,
                provider: parsed.provider,
              });
            }
          } catch {}
        } else if (currentEvent === "error") {
          try {
            const parsed = JSON.parse(dataStr) as {
              error?: unknown;
              request_id?: unknown;
              code?: unknown;
            };
            yield withFrameId({
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
            });
          } catch {
            yield {
              kind: "error",
              error: t("chat.errors.streamError"),
              request_id: "",
            };
          }
          sawTerminal = true;
          yield withFrameId({ kind: "terminator" });
          return;
        } else if (currentEvent === "plan") {
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
              yield withFrameId({ kind: "plan", steps });
            }
          } catch {}
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
              yield withFrameId({
                kind: "step_start",
                step_id: id,
                step_kind: kind,
              });
            }
          } catch {}
        } else if (currentEvent === "confirm_request") {
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
              yield withFrameId({
                kind: "confirm_request",
                run_id: parsed.run_id,
                step_id: parsed.step_id,
                tool: parsed.tool,
                args: parsed.args,
                approval_id: parsed.approval_id,
                expires_at: parsed.expires_at,
              });
            } else {
              console.warn(
                "chat: confirm_request frame missing required fields",
                parsed,
              );
            }
          } catch {
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
              yield withFrameId({
                kind: "tool_call",
                step_id: parsed.step_id,
                call_id: parsed.call_id,
                name: parsed.name,
                args: parsed.args,
              });
            }
          } catch {}
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
              yield withFrameId({
                kind: "tool_result",
                step_id: parsed.step_id,
                call_id: parsed.call_id,
                ok: parsed.ok === true,
                result: parsed.ok === true ? parsed.result : undefined,
                error:
                  typeof parsed.error === "string"
                    ? parsed.error
                    : undefined,
                retryCount:
                  typeof parsed.retry_count === "number"
                    ? parsed.retry_count
                    : undefined,
              });
            }
          } catch {}
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
              yield withFrameId({
                kind: "step_end",
                step_id: parsed.step_id,
                status: parsed.status,
                reason:
                  typeof parsed.reason === "string" ? parsed.reason : "",
              });
            }
          } catch {}
        }
      }
      if (consumed > 0) {
        buffer = buffer.slice(consumed);
      }
    }
    if (!sawTerminal) {
      yield { kind: "stream_incomplete" };
    }
  } finally {
    try {
      reader.releaseLock();
    } catch {}
  }
}

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

function toSessionStep(raw: unknown): SessionStep | null {
  if (!raw || typeof raw !== "object") return null;
  const row = raw as Record<string, unknown>;
  if (typeof row.id !== "string" || row.id === "") return null;
  if (
    row.kind !== "rag" &&
    row.kind !== "tool" &&
    row.kind !== "llm" &&
    row.kind !== "react"
  ) {
    return null;
  }
  if (
    row.status !== "running" &&
    row.status !== "ok" &&
    row.status !== "failed"
  ) {
    return null;
  }
  return {
    id: row.id,
    kind: row.kind,
    status: row.status,
    tool_name: typeof row.tool_name === "string" ? row.tool_name : undefined,
    tool_args: row.tool_args,
    tool_result: row.tool_result,
    retry_count:
      typeof row.retry_count === "number" ? row.retry_count : undefined,
    reason: typeof row.reason === "string" ? row.reason : undefined,
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
  const steps = Array.isArray(row.steps)
    ? row.steps
        .map(toSessionStep)
        .filter((s): s is SessionStep => s !== null)
    : [];
  return {
    id: typeof row.id === "number" ? row.id : 0,
    role: row.role,
    content: row.content,
    tool_calls: Array.isArray(row.tool_calls) ? row.tool_calls : null,
    steps: steps.length > 0 ? steps : null,
    tokens: typeof row.tokens === "number" ? row.tokens : 0,
    prompt_tokens:
      typeof row.prompt_tokens === "number" ? row.prompt_tokens : null,
    completion_tokens:
      typeof row.completion_tokens === "number" ? row.completion_tokens : null,
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
    const chained = new AbortController();
    const onCallerAbort = () => chained.abort();
    if (signal.aborted) onCallerAbort();
    else signal.addEventListener("abort", onCallerAbort);
    try {
      let response = await fetch(CHAT_URL, {
        method: "POST",
        headers: buildHeaders(),
        body: JSON.stringify(body),
        signal: chained.signal,
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
            signal: chained.signal,
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

      yield* consumeEventStream(response, chained, signal, opts.onRunId);
    } finally {
      signal.removeEventListener("abort", onCallerAbort);
    }
  },

  async *reattachRun(
    runId: string,
    signal: AbortSignal,
    opts: {
      onRunId?: (id: string) => void;
      lastEventId?: number;
    } = {},
  ): AsyncGenerator<ChatEvent> {
    const url = `${RUN_STREAM_URL_BASE}/${encodeURIComponent(runId)}/stream`;
    const headers = buildHeaders();
    if (opts.lastEventId !== undefined) {
      headers["Last-Event-ID"] = String(opts.lastEventId);
    }
    const chained = new AbortController();
    const onCallerAbort = () => chained.abort();
    if (signal.aborted) onCallerAbort();
    else signal.addEventListener("abort", onCallerAbort);
    try {
      let response = await fetch(url, {
        method: "GET",
        headers,
        signal: chained.signal,
      });
      if (response.status === 401 && tokenStore.getAccessToken() !== null) {
        const ok = await refreshTokensAndReplay();
        if (ok) {
          response = await fetch(url, {
            method: "GET",
            headers,
            signal: chained.signal,
          });
        }
      }
      if (!response.ok) {
        let envelope: ApiError | null = null;
        try {
          envelope = (await response.json()) as ApiError;
        } catch {}
        throw new ChatStreamError({
          kind: "http",
          status: response.status,
          message:
            envelope?.error ??
            t('chat.errors.requestFailed', { status: response.status }),
          code: envelope?.code,
        });
      }
      yield* consumeEventStream(response, chained, signal, opts.onRunId);
    } finally {
      signal.removeEventListener("abort", onCallerAbort);
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

  async cancelRun(runId: string): Promise<void> {
    await api.post(`/chat/runs/${encodeURIComponent(runId)}/cancel`, null, {
      skipErrorToast: true,
      skipSentry: true,
    });
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
  async getMessages(
    sessionId: string,
    opts: { signal?: AbortSignal } = {},
  ): Promise<SessionMessage[]> {
    const data = await requestJson<{ messages?: unknown }>(
      `${SESSIONS_URL}/${encodeURIComponent(sessionId)}/messages`,
      { signal: opts.signal },
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
