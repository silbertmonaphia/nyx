import { create } from "zustand";
import { chatService } from "../services/chatService";
import type { ChatStep, Message } from "../types/chat";

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
   * can't leave the UI stuck.
   */
  send: (text: string) => Promise<void>;
  /** Drop the entire history (manual reset / on logout). */
  reset: () => void;
  /** Abort the in-flight stream, if any. Idempotent. */
  cancel: () => void;
}

// localStorage key for the useFeeds toggle. Persisted so a
// deliberate opt-in survives a page reload — a user who flipped
// the switch expects it to stay flipped.
const USE_FEEDS_KEY = "nyx.chat.useFeeds";

// Module-level singleton so the abort controller survives across
// render boundaries. The store holds only the live reference; the
// controller itself is internal to this module.
let controller: AbortController | null = null;
// Set to true by `reset()` so the in-flight stream's `finally`
// block can skip the "drop half-built assistant" branch — reset
// has already wiped the messages array, so slicing it again would
// resurrect a row that the UI just declared gone.
let resetting = false;

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
    controller = localController;
    let aborted = false;
    try {
      const stream = chatService.streamMessage(
        [...baseMessages].map((m) => ({ role: m.role, content: m.content })),
        localController.signal,
        { rag: get().useFeeds },
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
        if (localController.signal.aborted) {
          aborted = true;
          break;
        }
        if (next.done) break;
        const event = next.value;
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
        // a stranded empty bubble after a Cancel.
        set((state) => ({
          messages: state.messages.slice(0, assistantIndex),
        }));
      } else {
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
      }
    } finally {
      controller = null;
      // If the consumer aborted mid-stream, drop the half-built
      // assistant row so the UI doesn't show a stranded empty
      // bubble. The catch block handles the same shape when the
      // abort surfaces as AbortError from fetch; this branch covers
      // the race-based cancel path that exits the loop cleanly.
      // Skipped when reset() already cleared messages.
      if (aborted && !resetting) {
        set((state) => ({
          messages: state.messages.slice(0, assistantIndex),
        }));
      }
      resetting = false;
      set({ isStreaming: false });
    }
  },

  reset: () => {
    // Cancel any in-flight stream so the generator doesn't keep
    // patching the store after the reset.
    resetting = true;
    if (controller) {
      controller.abort();
      controller = null;
    }
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
    if (controller) {
      controller.abort();
      controller = null;
    }
    set({ isStreaming: false });
  },
}));

// Helper for tests: re-exported so unit tests can inspect the
// generator's request shape without reaching into module internals.
export type { Message };
