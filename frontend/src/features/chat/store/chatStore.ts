import { create } from "zustand";
import { chatService } from "../services/chatService";
import type { Message } from "../types/chat";

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
}

interface ChatState {
  messages: ChatMessage[];
  isStreaming: boolean;
  error: string | null;
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

// Module-level singleton so the abort controller survives across
// render boundaries. The store holds only the live reference; the
// controller itself is internal to this module.
let controller: AbortController | null = null;
// Set to true by `reset()` so the in-flight stream's `finally`
// block can skip the "drop half-built assistant" branch — reset
// has already wiped the messages array, so slicing it again would
// resurrect a row that the UI just declared gone.
let resetting = false;

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
        } else if (event.kind === "error") {
          // Mid-stream error: surface the safe static error text.
          set((state) => ({
            error: event.error,
            messages: state.messages.slice(0, assistantIndex + 1),
          }));
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
        set({
          error: message,
          messages: get().messages.slice(0, assistantIndex + 1),
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
    set({ messages: [], isStreaming: false, error: null });
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
