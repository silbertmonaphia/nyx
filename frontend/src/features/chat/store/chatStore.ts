import { create } from "zustand";
import { logger } from "~/services/logger";
import { chatService } from "../services/chatService";
import {
  ChatApiError,
  type ApprovalDecision,
  type ChatStep,
  type ChatStepConfirm,
  type Message,
  type SessionMessage,
  type SessionSummary,
} from "../types/chat";

// Per-message `usage` payload delivered on `event: done`. Lives on
// the assistant message itself so a follow-up render can show token
// counts without holding the event out-of-band.
export interface ChatMessage {
  role: "user" | "assistant";
  content: string;
  usage?: {
    prompt_tokens: number;
    completion_tokens: number;
    total_tokens: number;
  };
  // Failover notes streamed from the backend when the primary LLM
  // errored mid-stream and the secondary is taking over. Each note
  // is a short static string (e.g. "[continued on OpenAI]"). Lives
  // on the assistant row so a follow-up render can show it without
  // keeping the event out-of-band.
  notes?: string[];
  // Agent-loop per-step trace. Populated from `plan` / `step_start`
  // / `tool_call` / `tool_result` / `step_end` SSE events when
  // AGENT_ENABLED=true; absent on the single-turn path. Ephemeral
  // — the store is not persisted (FUTURE_FRONTEND.md §10), so this
  // field stays in-memory only and never lands in localStorage.
  steps?: ChatStep[];
  // Partial-failure signal (agent-tool-dispatch-retry): set when the
  // stream carried a `note{kind: "partial_failure"}` frame — the run
  // diverged from the plan and the answer below may be incomplete.
  // Drives the <PartialFailureBanner> above the final answer.
  partialFailure?: { failedSteps: string[] };
}

interface ChatState {
  messages: ChatMessage[];
  isStreaming: boolean;
  error: string | null;
  /**
   * Whether the next `send()` should ask the server to ground the
   * answer in the authenticated user's feeds via top-K semantic
   * retrieval. Persisted to localStorage so a deliberate opt-in
   * survives a page reload. The toggle is consumed by `send` —
   * toggling mid-stream has no effect on the in-flight request.
   */
  useFeeds: boolean;
  setUseFeeds: (next: boolean) => void;
  /**
   * Send a user message and stream the assistant response. Appends
   * the user message immediately, opens the stream, and patches a
   * single assistant message in place as deltas arrive. Always
   * resolves `isStreaming=false` in `finally` so a thrown error
   * can't leave the UI stuck. Refuses while a stream is attached
   * (`isStreaming`) or while the active session has a detached
   * background run (`runningSessionIds`).
   */
  send: (text: string) => Promise<void>;
  /**
   * Drop the entire history (dialog close). Any live stream is
   * detached, not aborted — the run continues in the background and
   * surfaces through the rail.
   */
  reset: () => void;
  /**
   * Abort the attached in-flight stream, if any. Idempotent. The one
   * explicit stop gesture: navigation detaches (the run survives),
   * Cancel kills a run.
   */
  cancel: () => void;
  /**
   * Record an approve/deny decision for a paused `confirm_request`
   * and POST it to the agent through the chat proxy. On success the
   * card flips to its decided state; a failed POST keeps it pending
   * (with a short local error hint) so the user can retry — the axios
   * interceptor has already toasted the failure. A 404 is the one
   * non-retryable failure: the approval id is dead server-side, so
   * the card resolves as `expired` (design.md D2).
   */
  submitApproval: (
    runId: string,
    approvalId: string,
    decision: ApprovalDecision,
  ) => Promise<void>;

  expireApproval: (approvalId: string) => void;

  // ---- Chat history (agent-chat-history-ui) -------------------------

  /**
   * Active chat session id, or `null` when this tab isn't linked to a
   * stored session yet. The first `event: run_id` frame of a run mints
   * one and the store adopts it (see `send`), so a page reload can find
   * the conversation again. Persisted to sessionStorage — per-tab, like
   * the token store (design.md D4).
   */
  currentSessionId: string | null;
  /** Sidebar rows, newest-first; pages append via `loadMore`. */
  sessionList: SessionSummary[];
  /** Opaque cursor for the next page; `null` when the list is complete. */
  sessionListNextCursor: string | null;
  /** True while a list page (first or subsequent) is in flight. */
  isFetchingMore: boolean;
  /**
   * Fetch the next sidebar page (or the first one when nothing is
   * loaded). Single-flight: a call while one is in flight is dropped
   * (design.md D6). No-op once `sessionListNextCursor` is null.
   * Failures are silent — the sidebar shows its empty state.
   */
  loadMore: () => Promise<void>;
  /**
   * Refetch page 1 and merge it into the rail (newest-first by id),
   * preserving any lazy-loaded rows beyond page 1. Fired when a session
   * is first adopted so the new conversation appears immediately.
   * Failures are silent — the next mount/scroll retries.
   */
  refreshSessionList: () => Promise<void>;
  /**
   * Switch the active session: detach any live stream (the run
   * continues in the background), clear the panel, persist the id,
   * resume the session (`last_active_at`) and load its messages for
   * replay. A no-op when the id is already active; a 404 clears the
   * active session quietly.
   */
  setCurrentSession: (id: string) => Promise<void>;
  /**
   * Drop the active session (and its panel) without touching the
   * list. Aborts any attached stream — the destructive delete gesture
   * kills the run streaming into the deleted session.
   */
  clearActiveSession: () => void;
  /**
   * Mint a fresh session and make it active, resetting the panel. The
   * sidebar keeps its rows; the new session has none until the next
   * list fetch. Any live stream is detached, not aborted — the run
   * continues in the background.
   */
  startNewSession: () => Promise<void>;
  /**
   * Session ids with a live detached run streaming into them —
   * navigation moved the panel on but the SSE connection stayed open
   * so the run can finish (see `detachLiveStream`). Rendered as a
   * running indicator on the matching rail row; `send` refuses to
   * start a second concurrent run in a busy session, and the composer
   * shows the matching disabled state. Managed exclusively by the
   * stream that owns the run.
   */
  runningSessionIds: string[];
  /**
   * Restore the active session after a reload / dialog reopen: read the
   * persisted id, adopt it, and fetch its messages. No-op when there is
   * no persisted id, when messages are already on screen, or mid-stream.
   */
  hydrateSession: () => Promise<void>;
  /**
   * Soft-delete a session and drop it from the sidebar. Deleting the
   * active session also clears the panel.
   */
  deleteSession: (id: string) => Promise<void>;
}

// localStorage key for the useFeeds toggle. Persisted so a
// deliberate opt-in survives a page reload — a user who flipped
// the switch expects it to stay flipped.
const USE_FEEDS_KEY = "nyx.chat.useFeeds";

// Module-level singleton so the abort controller survives across
// render boundaries. The store holds only the live reference; the
// controller itself is internal to this module. Only the ATTACHED
// stream's controller is held here — a detached run has no client
// abort handle by design (it runs to its terminal state server-side).
let controller: AbortController | null = null;
// Monotonic id handed to each `send()`. A stream may write into the
// store only while its token is the attached one — see `attachedToken`.
let streamSeq = 0;
// Token of the stream that currently owns the panel — the only one
// whose frames render and whose callbacks mutate session state.
// Cleared when a stream ends, or by a detach (navigation) / abort.
let attachedToken: number | null = null;
// Poked by `detachLiveStream()` so the in-flight stream can raise its
// session's rail marker ("still working") the moment it is detached.
// Waiting for the next SSE frame instead would lag a run sitting in a
// silent tool call — heartbeat comment frames are dropped by the
// parser, so no store event fires. Set and cleared by the stream that
// owns it.
let detachHook: (() => void) | null = null;

// Read the persisted toggle at module init. localStorage access in
// the browser is synchronous and silent on failure (Safari private
// mode, SSR with no window, etc.) — a missing value collapses to
// the default (off).
function readPersistedUseFeeds(): boolean {
  if (typeof localStorage === "undefined") return false;
  try {
    return localStorage.getItem(USE_FEEDS_KEY) === "true";
  } catch {
    return false;
  }
}

// sessionStorage key for the active session id. Namespaced alongside
// the other chat persistence (`nyx.chat.useFeeds`) but per-tab: closing
// the tab forgets the conversation, a reload does not (design.md D4 —
// same posture as the token store).
const CURRENT_SESSION_KEY = "nyx.chat.currentSessionId";

function readPersistedSessionId(): string | null {
  if (typeof window === "undefined") return null;
  try {
    const id = window.sessionStorage.getItem(CURRENT_SESSION_KEY);
    return id && id !== "" ? id : null;
  } catch {
    return null;
  }
}

function persistSessionId(id: string | null): void {
  if (typeof window === "undefined") return;
  try {
    if (id === null) {
      window.sessionStorage.removeItem(CURRENT_SESSION_KEY);
    } else {
      window.sessionStorage.setItem(CURRENT_SESSION_KEY, id);
    }
  } catch {
    // Private mode / quota — the in-memory id still drives this render;
    // it just won't survive a reload.
  }
}

/**
 * Detach the panel-bound stream WITHOUT aborting it. The SSE
 * connection is the run's liveness signal: aborting it cancels the
 * agent mid-flight server-side (Go tears the upstream request down on
 * client disconnect; the Python agent's run generator is cancelled at
 * its next await and its `finally` cancels the run's pump task).
 * Navigation — `+ New chat`, a session switch, closing the dialog —
 * must leave the connection open and merely stop rendering its frames:
 * the detached stream keeps draining (and dropping) frames until it
 * reaches its terminal state, then surfaces what landed (see the
 * detached branch in `send`'s `finally`).
 *
 * No-op when nothing is attached (nothing to detach).
 */
function detachLiveStream(): void {
  if (attachedToken === null) return;
  attachedToken = null;
  controller = null;
  detachHook?.();
  detachHook = null;
}

/**
 * Abort the panel-bound stream, if any, and clear the attached token
 * so the aborted stream's `finally` skips its "drop the half-built
 * assistant row" slice — the caller (a destructive session delete
 * clearing the panel) replaces or clears that state itself. The
 * stream still observes the abort through its signal and skips its
 * post-completion duties (an aborted run is not a finished one).
 */
function abortLiveStream(): void {
  attachedToken = null;
  detachHook = null;
  if (!controller) return;
  controller.abort();
  controller = null;
}

/** True when the failure is a 404 — the session row is gone. */
function isSessionGone(err: unknown): boolean {
  return err instanceof ChatApiError && err.status === 404;
}

/**
 * True when the decision POST answered 404 — the approval id is dead
 * server-side. Design.md D2: a resumed run replays its checkpointed
 * `confirm_request`, whose approval id the decision endpoint answers
 * with a typed 404. The approvals call goes through the raw axios
 * client (not `ChatApiError`), so the status is read structurally —
 * same shape `useAuthReconciliation` uses.
 */
function isApprovalExpired(err: unknown): boolean {
  const status = (err as { response?: { status?: number } } | null)?.response
    ?.status;
  return status === 404;
}

/**
 * Convert a stored message row into the panel's `ChatMessage` shape.
 * Assistant rows carry their persisted `tool_calls` (the wire envelope
 * verbatim: `{step_id, call_id, name, args}`) which rehydrate as
 * `steps` so the replay renders the same "agent steps" disclosure the
 * live stream produces. Rows without usable tool calls render as plain
 * assistant text; unknown roles (the schema allows `tool` / `system`)
 * fold into the assistant side — the replay surface is user-facing and
 * only distinguishes "me" from "the agent". `tokens` is deliberately
 * dropped: it's a session total, not the prompt/completion split the
 * live usage row shows.
 */
function toReplayMessage(row: SessionMessage): ChatMessage {
  if (row.role === "user") {
    return { role: "user", content: row.content };
  }
  const steps: ChatStep[] = [];
  for (const raw of row.tool_calls ?? []) {
    if (!raw || typeof raw !== "object") continue;
    const call = raw as Record<string, unknown>;
    if (typeof call.step_id !== "string" || call.step_id === "") continue;
    steps.push({
      id: call.step_id,
      kind: "tool",
      status: "ok",
      toolName: typeof call.name === "string" ? call.name : undefined,
      toolArgs: call.args,
    });
  }
  return steps.length > 0
    ? { role: "assistant", content: row.content, steps }
    : { role: "assistant", content: row.content };
}

// Minimal store surface the module-level session helpers need. Typed
// structurally so zustand's own SetState signature stays the single
// source of truth for the store factory below.
type ChatGet = () => ChatState;
type ChatSet = (
  partial: Partial<ChatState> | ((state: ChatState) => Partial<ChatState>),
) => void;

/**
 * Patch the approval card carrying `approvalId`, wherever it lives.
 * Keyed by approval id (not step) because a decision can resolve after
 * the stream moved on; a non-matching id leaves the store untouched.
 */
function patchApprovalCard(
  set: ChatSet,
  approvalId: string,
  patch: Partial<ChatStepConfirm>,
  onlyIfPending = false,
): void {
  set((state) => {
    let found = false;
    const messages = state.messages.map((m) => {
      if (m.role !== "assistant" || !m.steps) return m;
      const idx = m.steps.findIndex(
        (s) => s.confirm?.approvalId === approvalId,
      );
      if (idx === -1) return m;
      const confirm = m.steps[idx].confirm;
      // Unreachable (the findIndex above matched on it) — the guard
      // exists only so the spread below type-narrows.
      if (!confirm) return m;
      if (onlyIfPending && confirm.decision !== "pending") return m;
      found = true;
      const steps = m.steps.slice();
      steps[idx] = { ...m.steps[idx], confirm: { ...confirm, ...patch } };
      return { ...m, steps };
    });
    return found ? { messages } : state;
  });
}

/**
 * Monotonic id for replay writes. Two replays of the SAME session can be
 * in flight at once — a user click and a detached run's completion both
 * target the run's session — and the fetched rows are snapshots taken at
 * request time: without ordering, the older (user-row-only) snapshot can
 * resolve last and overwrite the completed turn. Only the newest replay
 * may write.
 */
let replaySeq = 0;

/**
 * Resume one session and replay its messages into the panel. Shared by
 * `setCurrentSession`, `hydrateSession`, and the detached-run completion.
 *
 * A 404 on either call means the row is gone (deleted here, in another
 * tab, or reaped by retention): clear the active session without
 * surfacing anything — no toast, no console error (spec "Reload after
 * session deletion clears gracefully"). Any other failure is logged and
 * leaves the panel empty; the user can pick another session.
 */
async function loadSessionInto(
  get: ChatGet,
  set: ChatSet,
  id: string,
): Promise<void> {
  const replay = ++replaySeq;
  try {
    // Touch `last_active_at` so the session ranks at the top of the next
    // list fetch and retention sees it as live.
    await chatService.resumeSession(id);
  } catch (err) {
    if (isSessionGone(err)) {
      get().clearActiveSession();
      return;
    }
    logger.warn("chat.session.resume_failed", { error: String(err) });
  }
  try {
    const rows = await chatService.getMessages(id);
    // The write is guarded three ways. A switch (or `+ New chat`) may
    // have landed while the fetch was in flight — the later intent wins.
    // A NEWER replay of the same session supersedes this snapshot. And a
    // stream may have attached to the panel meanwhile — writing would
    // clobber its live turn (the send that started it snapshotted the
    // messages before this fetch resolved).
    if (replay !== replaySeq) return;
    if (get().currentSessionId !== id) return;
    if (attachedToken !== null) return;
    set({ messages: rows.map(toReplayMessage) });
  } catch (err) {
    if (isSessionGone(err)) {
      get().clearActiveSession();
      return;
    }
    logger.warn("chat.session.replay_failed", { error: String(err) });
  }
}

/**
 * Ephemeral chat history. Deliberately NOT persisted to localStorage:
 *  - The brief calls it out as out-of-scope (FUTURE_FRONTEND.md
 *    still lists chat persistence as a follow-up).
 *  - Persisted chat content would land in the XSS exfiltration
 *    surface — same class of risk as persisting tokens, and not
 *    worth the migration story yet.
 */
export const useChatStore = create<ChatState>((set, get) => ({
  messages: [],
  isStreaming: false,
  error: null,
  useFeeds: readPersistedUseFeeds(),
  currentSessionId: readPersistedSessionId(),
  sessionList: [],
  sessionListNextCursor: null,
  isFetchingMore: false,
  runningSessionIds: [],

  setUseFeeds: (next) => {
    if (typeof localStorage !== "undefined") {
      try {
        localStorage.setItem(USE_FEEDS_KEY, next ? "true" : "false");
      } catch {
        // Safari private mode, quota exceeded, etc. — the in-memory
        // value still flips, so the current session works; the
        // choice just won't survive a reload.
      }
    }
    set({ useFeeds: next });
  },

  send: async (text) => {
    const trimmed = text.trim();
    if (!trimmed) return;
    if (get().isStreaming) return;
    // Refuse a follow-up while the active session still has a detached
    // run streaming into it: a second concurrent run on one session
    // would interleave its user/assistant rows and garble the
    // conversation's replay. (The composer renders the same guard as a
    // disabled state + hint.)
    const activeSessionId = get().currentSessionId;
    if (
      activeSessionId !== null &&
      get().runningSessionIds.includes(activeSessionId)
    ) {
      return;
    }

    const userMessage: ChatMessage = { role: "user", content: trimmed };
    // Push the user message + open a placeholder assistant row that
    // deltas will append to. Index `assistantIndex` is computed AFTER
    // the user message lands so `set` is the source of truth.
    const baseMessages = [...get().messages, userMessage];
    set({
      messages: baseMessages,
      isStreaming: true,
      error: null,
    });

    const assistantIndex = baseMessages.length;
    // Seed an empty assistant message; deltas concatenate into it.
    set((state) => ({
      messages: [
        ...state.messages,
        { role: "assistant", content: "" } as ChatMessage,
      ],
    }));

    const localController = new AbortController();
    const token = ++streamSeq;
    controller = localController;
    attachedToken = token;
    // Capture the session this request is sent with — the adoption guard
    // below compares against THIS value, not the live store (review M2,
    // 2026-10-14).
    const sentSessionId = get().currentSessionId;
    // The session this run is actually writing to: the echoed id until
    // the server's `run_id` frame says otherwise (a fresh mint when the
    // echoed id was unknown / soft-deleted). Tracked even while
    // detached — the rail marker and the post-completion replay key off
    // it — but never adopted into `currentSessionId` after a detach
    // (the panel already moved on).
    let runSessionId: string | null = sentSessionId;
    // Only the stream whose token is attached may write into the store.
    const isAttached = () => attachedToken === token;
    // This stream's rail marker ("still working" on the run's session
    // row), written only from this closure so its bookkeeping — which
    // id is currently marked — stays consistent.
    let markedSession: string | null = null;
    const railMark = (id: string | null) => {
      if (id === markedSession) return;
      const prev = markedSession;
      markedSession = id;
      set((state) => {
        let next = state.runningSessionIds;
        if (prev !== null) next = next.filter((r) => r !== prev);
        if (id !== null && !next.includes(id)) next = [...next, id];
        return { runningSessionIds: next };
      });
    };
    // `detachLiveStream` pokes this the moment this stream is detached
    // so the rail marker appears immediately — waiting for the next SSE
    // frame can lag a run sitting in a silent tool call.
    const onStreamDetached = () => railMark(runSessionId);
    detachHook = onStreamDetached;
    try {
      const stream = chatService.streamMessage(
        [...baseMessages].map((m) => ({ role: m.role, content: m.content })),
        localController.signal,
        {
          rag: get().useFeeds,
          // Echo the active session so the agent continues it instead
          // of minting a new one. Null on the tab's first message.
          runId: sentSessionId,
          // The server is the authority on which session it actually
          // wrote to: adopt the frame id whenever it differs from the
          // one this request carried — a first message (null → id), or
          // a fresh mint when the echoed id was unknown / soft-deleted /
          // hit a chat-store hiccup. If the user switched sessions while
          // this stream was in flight, their pick wins (the switch
          // detaches this stream; this is the belt to that suspenders).
          // A detached stream never adopts — it just re-keys its rail
          // marker to the id the run actually landed on.
          onRunId: (id) => {
            runSessionId = id;
            if (!isAttached()) {
              railMark(id);
              // The rail may not carry this id yet — a run detached
              // before its first frame (navigation within ms of send)
              // mints its session after the tab's last list fetch. Pull
              // page 1 so the pulse has a row to sit on.
              void get().refreshSessionList();
              return;
            }
            if (id === sentSessionId) return; // request landed where sent
            if (get().currentSessionId !== sentSessionId) return; // user moved on
            set({ currentSessionId: id });
            persistSessionId(id);
            // The adopted id exists server-side (the frame follows the
            // row insert), so pull page 1 and surface the new
            // conversation in the rail without waiting for a reload.
            void get().refreshSessionList();
          },
        },
      );
      while (true) {
        // Race the next event against the abort signal so a Cancel
        // breaks the loop even when the underlying service (or its
        // test mock) doesn't honour the signal. Without this, the
        // store would hang on a never-resolving `.next()` after
        // `controller.abort()`. We close over `localController` so
        // a `cancel()` that nulls out the module-level slot can't
        // dereference null inside this closure.
        const next = await Promise.race([
          stream.next(),
          new Promise<{ done: true; value: undefined }>((resolve) => {
            if (localController.signal.aborted) {
              resolve({ done: true, value: undefined });
              return;
            }
            localController.signal.addEventListener(
              "abort",
              () => resolve({ done: true, value: undefined }),
              { once: true },
            );
          }),
        ]);
        // Cancel path: the race above resolves on abort — leave the
        // loop; the `finally` reads the signal to distinguish a kill
        // from a natural end.
        if (localController.signal.aborted) break;
        if (next.done) break;
        const event = next.value;
        // Detached (navigation moved the panel on): keep draining so
        // the run stays alive server-side, but drop the frame — it
        // belongs to a conversation the panel is no longer showing.
        if (!isAttached()) continue;
        if (event.kind === "delta") {
          set((state) => {
            const next = state.messages.slice();
            const target = next[assistantIndex];
            if (!target || target.role !== "assistant") return state;
            next[assistantIndex] = {
              ...target,
              content: target.content + event.delta,
            };
            return { messages: next };
          });
        } else if (event.kind === "done") {
          set((state) => {
            const next = state.messages.slice();
            const target = next[assistantIndex];
            if (!target || target.role !== "assistant") return state;
            next[assistantIndex] = {
              ...target,
              usage: event.usage,
            };
            return { messages: next };
          });
        } else if (event.kind === "note") {
          // Mid-stream failover note from the router: append to the
          // partial assistant reply's notes array. Do NOT truncate
          // messages — unlike the error branch, the partial assistant
          // content survives and the secondary's deltas will continue
          // appending into the same row.
          set((state) => {
            const next = state.messages.slice();
            const target = next[assistantIndex];
            if (!target || target.role !== "assistant") return state;
            next[assistantIndex] = {
              ...target,
              notes: [...(target.notes ?? []), event.text],
            };
            return { messages: next };
          });
        } else if (event.kind === "plan") {
          // Agent-loop plan event: seed the per-step trace so the
          // UI can render planned steps before `step_start` fires.
          // The planner's id+kind pairs are the canonical IDs the
          // subsequent step-scoped frames reference; storing them
          // up-front means a step_start / tool_call / step_end
          // arriving before we've rendered anything still finds a
          // row to update.
          set((state) => {
            const next = state.messages.slice();
            const target = next[assistantIndex];
            if (!target || target.role !== "assistant") return state;
            const steps: ChatStep[] = event.steps.map((s) => ({
              id: s.id,
              kind: s.kind,
              status: "running",
              toolName: s.tool,
              toolArgs: s.args,
            }));
            next[assistantIndex] = { ...target, steps };
            return { messages: next };
          });
        } else if (event.kind === "step_start") {
          // Open a step's row. If the `plan` event already seeded
          // it, mark it running and update kind (the plan's kind
          // and step_start's kind are identical today but we keep
          // them decoupled for the case where a planner revision
          // ever diverges). If no plan preceded this event (legacy
          // fallback), append a fresh step row.
          set((state) => {
            const next = state.messages.slice();
            const target = next[assistantIndex];
            if (!target || target.role !== "assistant") return state;
            const steps = (target.steps ?? []).slice();
            const idx = steps.findIndex((s) => s.id === event.step_id);
            if (idx >= 0) {
              steps[idx] = {
                ...steps[idx],
                kind: event.step_kind,
                status: "running",
              };
            } else {
              steps.push({
                id: event.step_id,
                kind: event.step_kind,
                status: "running",
              });
            }
            next[assistantIndex] = { ...target, steps };
            return { messages: next };
          });
        } else if (event.kind === "confirm_request") {
          // The run is paused on a mutating call: attach the approval
          // card to the step the frame names. A step row may not exist
          // yet (no plan / step_start seen), so one is appended — the
          // card must render even on a partial trace. The confirm's
          // tool + args mirror the frames a `tool_call` would carry,
          // so they seed the step the same way (latest wins).
          set((state) => {
            const next = state.messages.slice();
            const target = next[assistantIndex];
            if (!target || target.role !== "assistant") return state;
            const steps = (target.steps ?? []).slice();
            const confirm: ChatStepConfirm = {
              runId: event.run_id,
              approvalId: event.approval_id,
              tool: event.tool,
              args: event.args,
              expiresAt: event.expires_at,
              decision: "pending",
            };
            const idx = steps.findIndex((s) => s.id === event.step_id);
            if (idx >= 0) {
              steps[idx] = {
                ...steps[idx],
                toolName: event.tool,
                toolArgs: event.args,
                confirm,
              };
            } else {
              steps.push({
                id: event.step_id,
                kind: "tool",
                status: "running",
                toolName: event.tool,
                toolArgs: event.args,
                confirm,
              });
            }
            next[assistantIndex] = { ...target, steps };
            return { messages: next };
          });
        } else if (event.kind === "tool_call") {
          // Annotate the step with the dispatched tool's name + args.
          // Multiple tool calls per step (react sub-loop) overwrite
          // the previous — the UI shows the latest, matching the
          // ChatStep type's singular fields.
          set((state) => {
            const next = state.messages.slice();
            const target = next[assistantIndex];
            if (!target || target.role !== "assistant") return state;
            const steps = (target.steps ?? []).slice();
            const idx = steps.findIndex((s) => s.id === event.step_id);
            if (idx >= 0) {
              steps[idx] = {
                ...steps[idx],
                toolName: event.name,
                toolArgs: event.args,
              };
              next[assistantIndex] = { ...target, steps };
            }
            return { messages: next };
          });
        } else if (event.kind === "tool_result") {
          // Record the dispatch outcome on the step. Failed calls
          // carry a static safe marker (matches the wire's
          // "tool_call_failed" literal) and never the underlying
          // err.Error(); we store it verbatim so the UI can show
          // the same marker.
          set((state) => {
            const next = state.messages.slice();
            const target = next[assistantIndex];
            if (!target || target.role !== "assistant") return state;
            const steps = (target.steps ?? []).slice();
            const idx = steps.findIndex((s) => s.id === event.step_id);
            if (idx >= 0) {
              steps[idx] = {
                ...steps[idx],
                toolResult: event.ok ? event.result : event.error,
                retryCount: event.retryCount,
              };
              next[assistantIndex] = { ...target, steps };
            }
            return { messages: next };
          });
        } else if (event.kind === "partial_failure") {
          // The orchestrator's "run diverged from the plan" signal.
          // Attach it to the streaming assistant row so the final
          // answer renders with a warning banner above it. Deliberately
          // does NOT touch content — the answer itself still renders.
          set((state) => {
            const next = state.messages.slice();
            const target = next[assistantIndex];
            if (!target || target.role !== "assistant") return state;
            next[assistantIndex] = {
              ...target,
              partialFailure: { failedSteps: event.failedSteps },
            };
            return { messages: next };
          });
        } else if (event.kind === "step_end") {
          // Close the step's row with its final status + reason.
          set((state) => {
            const next = state.messages.slice();
            const target = next[assistantIndex];
            if (!target || target.role !== "assistant") return state;
            const steps = (target.steps ?? []).slice();
            const idx = steps.findIndex((s) => s.id === event.step_id);
            if (idx >= 0) {
              steps[idx] = {
                ...steps[idx],
                status: event.status === "ok" ? "ok" : "failed",
                reason: event.reason || undefined,
              };
              next[assistantIndex] = { ...target, steps };
            }
            return { messages: next };
          });
        } else if (event.kind === "error") {
          // Mid-stream error: surface the safe static error text.
          // Don't leave a stranded empty bubble in the conversation
          // history. If no deltas landed yet, drop the assistant row
          // entirely; if any content was already streamed, keep it
          // (the user saw it) and let the error toast explain the
          // truncation. Mirrors the cancel-path intent.
          set((state) => {
            const assistant = state.messages[assistantIndex];
            const hasContent =
              assistant?.role === "assistant" && assistant.content.length > 0;
            return {
              error: event.error,
              messages: hasContent
                ? state.messages.slice(0, assistantIndex + 1)
                : state.messages.slice(0, assistantIndex),
            };
          });
          break;
        } else {
          // `terminator` — clean end of stream, nothing to do.
        }
      }
    } catch (err) {
      // Distinguish user-driven aborts (Cancel button) from real
      // failures. AbortError is the WHATWG fetch name; the
      // underlying DOMException shares it. Don't surface it as a
      // chat error — the user pressed Cancel on purpose.
      const name = (err as { name?: string } | null)?.name;
      if (name === "AbortError") {
        // Drop the half-built assistant row so the UI doesn't show
        // a stranded empty bubble after a Cancel. Skipped when the
        // stream was already detached — the detach replaced or cleared
        // the panel, so slicing would touch unrelated rows.
        if (isAttached()) {
          set((state) => ({
            messages: state.messages.slice(0, assistantIndex),
          }));
        }
      } else if (isAttached()) {
        const message =
          err instanceof Error ? err.message : "Chat request failed";
        // Same empty-vs-partial rule as the mid-stream error branch:
        // a stranded empty bubble pollutes the next message's
        // history (planner sees `[user]hi, [assistant]''` and
        // re-plans for the assistant instead of the user), so drop
        // it. Partial content stays — the user saw it.
        const assistant = get().messages[assistantIndex];
        const hasContent =
          assistant?.role === "assistant" && assistant.content.length > 0;
        set({
          error: message,
          messages: hasContent
            ? get().messages.slice(0, assistantIndex + 1)
            : get().messages.slice(0, assistantIndex),
        });
      } else {
        // A detached run's failure is not this panel's error — the
        // panel is showing a different conversation. Log for
        // diagnosability; the run's session replays as user-only (the
        // server records an assistant row only on `done`).
        logger.warn("chat.send.detached_failed", { error: String(err) });
      }
    } finally {
      // Only the stream that still owns the controller may clear it —
      // a newer send may have replaced it while this one drained.
      if (controller === localController) controller = null;
      if (detachHook === onStreamDetached) detachHook = null;
      if (attachedToken === token) {
        // Ended (or was cancelled) while it was still the panel's
        // stream. Drop the half-built assistant row after a Cancel so
        // the UI doesn't show a stranded empty bubble; skipped for a
        // clean end. `signal.aborted` covers both the race-based break
        // above and a fetch that rejected with AbortError. This
        // stream's rail marker (never set while attached) retires too.
        railMark(null);
        attachedToken = null;
        if (localController.signal.aborted) {
          set((state) => ({
            messages: state.messages.slice(0, assistantIndex),
          }));
        }
        set({ isStreaming: false });
      } else if (localController.signal.aborted) {
        // Killed while detached — a Cancel that raced a navigation
        // (the detach already poked the marker) or a destructive
        // session clear. Nothing will replay; retire the marker now.
        railMark(null);
      } else {
        // Detached: navigation moved the panel on but left the SSE
        // connection open, so the run reached its terminal state
        // server-side and its writes are durable — the agent's shielded
        // finalize persists the assistant row BEFORE it yields
        // `[DONE]`, which is what let this loop exit. Surface what
        // landed: when the finished session is the one on screen, replay
        // it FIRST and keep the rail marker (the composer lock) up until
        // the replay lands — unlocking before the fetch resolves would
        // let a fast user start a new run whose panel the replay would
        // then clobber. The marker always clears, even when the session
        // is gone (the replay's own 404 path asks for exactly that).
        void (async () => {
          try {
            if (
              runSessionId !== null &&
              get().currentSessionId === runSessionId
            ) {
              await loadSessionInto(get, set, runSessionId);
            }
          } finally {
            railMark(null);
          }
          void get().refreshSessionList();
        })();
      }
    }
  },

  reset: () => {
    // Detach any in-flight stream instead of aborting it: closing the
    // dialog must not kill a run that is mid-answer (the SSE
    // connection is the run's liveness signal — see
    // `detachLiveStream`). The run finishes server-side and surfaces
    // through the rail.
    detachLiveStream();
    // A fresh dialog session starts with RAG off; the persisted
    // user preference survives but is not auto-applied until the
    // user re-toggles it. Avoids surprising the next visitor who
    // shares the browser.
    set({ messages: [], isStreaming: false, error: null, useFeeds: false });
    if (typeof localStorage !== "undefined") {
      try {
        localStorage.setItem(USE_FEEDS_KEY, "false");
      } catch {
        // Same private-mode tolerance as setUseFeeds.
      }
    }
  },

  cancel: () => {
    // The one explicit client stop gesture — the only path that kills
    // a run (navigation detaches; see `detachLiveStream`). The stream
    // observes the abort through its signal and drops its half-built
    // row from its own `finally` (the attached-token check). That drop
    // is deferred by one microtask drain: a programmatic `send()`
    // issued in the same tick would take the token first and strand
    // the row — unreachable from the UI, where the composer disables
    // synchronously and the next click is a later task.
    if (controller) {
      controller.abort();
      controller = null;
    }
    set({ isStreaming: false });
  },

  submitApproval: async (runId, approvalId, decision) => {
    try {
      await chatService.submitApproval({
        run_id: runId,
        approval_id: approvalId,
        decision,
      });
      patchApprovalCard(set, approvalId, {
        decision: decision === "approve" ? "approved" : "denied",
        error: undefined,
      });
    } catch (err) {
      // A 404 is the endpoint's typed answer for an approval id that
      // can no longer resolve (design.md D2: a resumed run replays its
      // checkpointed `confirm_request`, whose approval id is expired).
      // Resolve the card as expired — the run moved on, so a retry
      // could never succeed and a retry hint would be a lie.
      if (isApprovalExpired(err)) {
        logger.warn("chat.approval.expired", { error: String(err) });
        patchApprovalCard(set, approvalId, {
          decision: "expired",
          error: undefined,
        });
        return;
      }
      // The axios interceptor already toasted the failure (4xx bodies
      // are contract strings; 5xx stays generic). Keep the card
      // pending with a short local hint so the user can retry —
      // never echo `err.message` (SECURITY.md H7).
      logger.warn("chat.approval.submit_failed", { error: String(err) });
      patchApprovalCard(
        set,
        approvalId,
        { error: "Could not send your decision. Please try again." },
        true,
      );
    }
  },

  expireApproval: (approvalId) => {
    patchApprovalCard(set, approvalId, { decision: "expired" }, true);
  },

  // ---- Chat history actions (agent-chat-history-ui) -----------------

  loadMore: async () => {
    const { isFetchingMore, sessionList, sessionListNextCursor } = get();
    // Single-flight (design.md D6): an IntersectionObserver burst must
    // not append the same page twice.
    if (isFetchingMore) return;
    // End of list: the sidebar has no sentinel to fire, but a manual
    // call (or a remount racing a slow resolve) must not refetch page 1
    // and duplicate every row.
    if (sessionListNextCursor === null && sessionList.length > 0) return;

    set({ isFetchingMore: true });
    try {
      const page = await chatService.listSessions({
        before: sessionListNextCursor,
      });
      set((state) => ({
        sessionList: [...state.sessionList, ...page.sessions],
        sessionListNextCursor: page.nextCursor,
      }));
    } catch (err) {
      // Quiet failure: the sidebar keeps its empty state instead of a
      // toast (spec "no error toast on list failure"); the next mount
      // or scroll retries.
      logger.warn("chat.session.list_failed", { error: String(err) });
    } finally {
      set({ isFetchingMore: false });
    }
  },

  refreshSessionList: async () => {
    // Refetch page 1 and merge newest-first by id: rows the fresh page
    // carries are replaced in place (server owns their order and
    // counts), rows the user had lazy-loaded beyond page 1 are kept in
    // their existing order. Called when a session is first adopted
    // (the tab's first message minting an id) so the new conversation
    // shows up in the rail immediately. Merging (rather than
    // reset-and-replace) means a concurrently resolving mount/scroll
    // fetch cannot duplicate or reorder what is already on screen.
    try {
      const page = await chatService.listSessions({});
      set((state) => {
        const freshIds = new Set(page.sessions.map((s) => s.id));
        const tail = state.sessionList.filter((s) => !freshIds.has(s.id));
        return {
          sessionList: [...page.sessions, ...tail],
          sessionListNextCursor:
            state.sessionListNextCursor ?? page.nextCursor,
        };
      });
    } catch (err) {
      logger.warn("chat.session.refresh_failed", { error: String(err) });
    }
  },

  setCurrentSession: async (id) => {
    // Clicking the row of the active session is a no-op — no refetch,
    // no panel clear (design.md D7).
    if (get().currentSessionId === id) return;
    // Drop the live view first — clear the messages before the fetch
    // lands — but detach (not abort) any in-flight stream: the run
    // continues in the background and surfaces through the rail (see
    // `detachLiveStream`).
    detachLiveStream();
    set({ currentSessionId: id, messages: [], isStreaming: false, error: null });
    persistSessionId(id);
    await loadSessionInto(get, set, id);
  },

  clearActiveSession: () => {
    abortLiveStream();
    persistSessionId(null);
    set({
      currentSessionId: null,
      messages: [],
      isStreaming: false,
      error: null,
    });
  },

  startNewSession: async () => {
    // Clear immediately so the panel flips to the welcome state whether
    // or not the POST succeeds — spec Scenario "New chat clears the
    // panel" is a UI guarantee, not a network one. Any live stream is
    // detached, not aborted: the run finishes in the background and
    // surfaces through the rail (see `detachLiveStream`).
    detachLiveStream();
    persistSessionId(null);
    set({ currentSessionId: null, messages: [], isStreaming: false, error: null });
    try {
      const { session_id } = await chatService.createSession();
      // If a session got picked (or a message started streaming and will
      // adopt its own id from the run_id frame) while this POST was in
      // flight, the just-created session is an empty orphan — drop it so
      // the rail stays honest, and leave the live flow alone. The
      // `isStreaming` arm matters: adopting B here mid-send would point
      // the panel at B while the server writes the turn to the id the
      // stream eventually reports.
      if (get().currentSessionId !== null || get().isStreaming) {
        void chatService.deleteSession(session_id).catch((err: unknown) => {
          logger.warn("chat.session.orphan_cleanup_failed", {
            error: String(err),
          });
        });
        return;
      }
      set({ currentSessionId: session_id });
      persistSessionId(session_id);
    } catch (err) {
      // The panel is already empty; the next message mints a fresh
      // session via the run_id frame path.
      logger.warn("chat.session.create_failed", { error: String(err) });
    }
  },

  hydrateSession: async () => {
    // A reload re-imports the store, which reads sessionStorage at
    // module init; a later (re)mount re-reads it here so a store that
    // was reset — or never hydrated — still restores the tab's session.
    let id = get().currentSessionId;
    if (id === null) {
      id = readPersistedSessionId();
      if (id !== null) set({ currentSessionId: id });
    }
    if (id === null) return;
    // Hydration is a reload / reopen concern: a conversation already on
    // screen (or mid-stream) must not be refetched and replaced.
    if (get().messages.length > 0 || get().isStreaming) return;
    await loadSessionInto(get, set, id);
  },

  deleteSession: async (id) => {
    try {
      await chatService.deleteSession(id);
    } catch (err) {
      // A 404 means it is already gone (another tab, or the retention
      // cron) — drop the stale row locally anyway. Anything else is
      // transient: keep the row so the user can retry.
      if (!isSessionGone(err)) {
        logger.warn("chat.session.delete_failed", { error: String(err) });
        return;
      }
    }
    set((state) => ({
      sessionList: state.sessionList.filter((s) => s.id !== id),
    }));
    if (get().currentSessionId === id) {
      get().clearActiveSession();
    }
  },
}));

// Helper for tests: re-exported so unit tests can inspect the
// generator's request shape without reaching into module internals.
export type { Message };
