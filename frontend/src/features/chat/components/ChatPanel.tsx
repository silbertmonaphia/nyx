import React, { useEffect, useRef, useState } from "react";
import {
  ChevronDown,
  Loader2,
  Menu,
  Plus,
  Send,
  Square,
} from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/Dialog";
import { Button } from "~/components/ui/Button";
import { Textarea } from "~/components/ui/Textarea";
import { cn } from "~/utils/cn";
import { useChat } from "../hooks/useChat";
import { ChatSidebar } from "./ChatSidebar";
import { ConfirmationCard } from "./ConfirmationCard";
import { PartialFailureBanner } from "./PartialFailureBanner";
import type { ChatStep } from "../types/chat";

// Rollback switch from the change design ("Rollback: SPA flag
// VITE_CHAT_HISTORY_SIDEBAR_ENABLED=false hides the sidebar and reverts
// to the current behaviour"). Enabled unless explicitly disabled;
// `+ New chat` goes with the list — without the rail there is no way
// back to the session it would mint.
const SIDEBAR_ENABLED =
  import.meta.env.VITE_CHAT_HISTORY_SIDEBAR_ENABLED !== "false";

/**
 * Format an unknown JSON value as a one-line preview. Used in the
 * collapsed "args" row of each agent step — the UI hides the full
 * payload by default to keep the bubble readable, but the user can
 * pop the <details> to see the raw JSON.
 */
function formatJsonPreview(value: unknown): string {
  if (value === undefined) return "";
  try {
    const text = JSON.stringify(value);
    if (text.length <= 80) return text;
    return `${text.slice(0, 77)}…`;
  } catch {
    return String(value);
  }
}

/**
 * Single row in the "agent steps" trace. Renders the step's kind +
 * status + (for tool steps) the latest tool name and a collapsible
 * args preview. Pure presentational; the store owns the data.
 */
const StepRow: React.FC<{ step: ChatStep }> = ({ step }) => {
  const argsPreview = formatJsonPreview(step.toolArgs);
  const resultPreview = formatJsonPreview(step.toolResult);
  const statusLabel =
    step.status === "running"
      ? "running"
      : step.status === "ok"
        ? "ok"
        : "failed";
  return (
    <li
      className="text-xs space-y-1"
      data-testid="chat-agent-step"
      data-step-id={step.id}
      data-step-status={step.status}
    >
      <div className="flex items-center gap-2">
        {step.status === "running" ? (
          <Loader2
            className="h-3 w-3 animate-spin text-muted-foreground"
            aria-hidden="true"
          />
        ) : (
          <span
            className={cn(
              "h-2 w-2 rounded-full",
              step.status === "ok" ? "bg-emerald-500" : "bg-destructive",
            )}
            aria-hidden="true"
          />
        )}
        <span className="font-mono text-muted-foreground">{step.kind}</span>
        {step.toolName && (
          <span className="font-medium" data-testid="chat-agent-step-tool">
            {step.toolName}
          </span>
        )}
        {typeof step.retryCount === "number" && step.retryCount > 0 && (
          <span
            className="text-[10px] text-amber-600 dark:text-amber-400"
            data-testid="chat-agent-step-retry"
            title={`succeeded after ${step.retryCount} retr${step.retryCount === 1 ? "y" : "ies"}`}
          >
            ↻ {step.retryCount}
          </span>
        )}
        <span className="ml-auto text-[10px] uppercase tracking-wide text-muted-foreground">
          {statusLabel}
        </span>
      </div>
      {step.toolName && argsPreview && (
        <details
          className="ml-5 text-[10px] text-muted-foreground"
          data-testid="chat-agent-step-args"
        >
          <summary className="cursor-pointer select-none">args</summary>
          <pre className="mt-1 whitespace-pre-wrap break-words font-mono">
            {JSON.stringify(step.toolArgs, null, 2)}
          </pre>
        </details>
      )}
      {step.status !== "running" && resultPreview && (
        <div
          className="ml-5 text-[10px] text-muted-foreground"
          data-testid="chat-agent-step-result"
        >
          result: {resultPreview}
        </div>
      )}
      {step.status === "failed" && step.reason && (
        <div
          className="ml-5 text-[10px] text-destructive"
          data-testid="chat-agent-step-reason"
        >
          {step.reason}
        </div>
      )}
    </li>
  );
};

interface ChatPanelProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

/**
 * Streaming chat dialog. Reuses the Radix Dialog primitive, the
 * shadcn-style Textarea/Button, and the cn() helper — no new UI
 * library. Auth gating lives one layer up (App.tsx only mounts this
 * when `isAuthenticated`), so the panel assumes a valid session.
 *
 * Hosts the conversation-history rail: a static left column at `md:` and
 * up, an overlay drawer below it (toggled from the header). Session
 * switches replay their stored messages through the same `messages`
 * array the live stream writes to, so the render path — bubbles, agent
 * steps, partial-failure banner — is shared (openspec change
 * `agent-chat-history-ui`).
 *
 * Auto-scroll to the bottom of the message list whenever the
 * assistant grows. We coalesce via `requestAnimationFrame` so a
 * burst of SSE deltas doesn't queue up dozens of `scrollTop` writes
 * per second.
 */
export const ChatPanel: React.FC<ChatPanelProps> = ({ open, onOpenChange }) => {
  const messages = useChat((s) => s.messages);
  const isStreaming = useChat((s) => s.isStreaming);
  const error = useChat((s) => s.error);
  const send = useChat((s) => s.send);
  const cancel = useChat((s) => s.cancel);
  const submitApproval = useChat((s) => s.submitApproval);
  const reset = useChat((s) => s.reset);
  const useFeeds = useChat((s) => s.useFeeds);
  const setUseFeeds = useChat((s) => s.setUseFeeds);
  const hydrateSession = useChat((s) => s.hydrateSession);
  const startNewSession = useChat((s) => s.startNewSession);

  const [draft, setDraft] = useState("");
  // Mobile-only drawer state. At `md:` and up the rail is always
  // visible and this flag only adds overlay classes below the
  // breakpoint — no JS media query, no duplicate DOM.
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const listRef = useRef<HTMLDivElement | null>(null);
  const rafRef = useRef<number | null>(null);
  // Track the previous `open` value so the close → reset transition
  // fires exactly once per close, without setState-inside-effect.
  const wasOpenRef = useRef(open);

  useEffect(() => {
    if (wasOpenRef.current && !open) {
      // Closing — clear ephemeral state. `reset` lives in the store
      // (synchronous); the draft clear is a direct event handler
      // concern so we route it through a microtask rather than a
      // cascading setState in this effect.
      reset();
      queueMicrotask(() => {
        setDraft("");
        setSidebarOpen(false);
      });
    }
    wasOpenRef.current = open;
  }, [open, reset]);

  // Restore the tab's active session on open: after a reload the id
  // comes from sessionStorage, and reopening the dialog refetches the
  // messages `reset()` dropped on close. No-op when there is no
  // persisted id, when the conversation is already on screen, or when
  // the session no longer exists (the store clears it quietly).
  useEffect(() => {
    if (open) {
      void hydrateSession();
    }
  }, [open, hydrateSession]);

  // Auto-scroll on new content. Skip the first paint of a fresh
  // open — the empty list shouldn't snap to a non-existent bottom.
  useEffect(() => {
    if (!listRef.current) return;
    if (rafRef.current !== null) cancelAnimationFrame(rafRef.current);
    rafRef.current = requestAnimationFrame(() => {
      if (listRef.current) {
        listRef.current.scrollTop = listRef.current.scrollHeight;
      }
      rafRef.current = null;
    });
    return () => {
      if (rafRef.current !== null) {
        cancelAnimationFrame(rafRef.current);
        rafRef.current = null;
      }
    };
  }, [messages]);

  const handleSubmit = (e?: React.FormEvent) => {
    e?.preventDefault();
    const text = draft.trim();
    if (!text || isStreaming) return;
    setDraft("");
    void send(text);
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    // Enter submits, Shift+Enter inserts a newline (the standard
    // chat-UX convention).
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      handleSubmit();
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-4xl" data-testid="chat-panel">
        <DialogHeader>
          <div className="flex items-center gap-2 pr-6">
            {SIDEBAR_ENABLED && (
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="md:hidden"
                aria-label={sidebarOpen ? "Hide chat history" : "Show chat history"}
                aria-expanded={sidebarOpen}
                data-testid="chat-sidebar-toggle"
                onClick={() => setSidebarOpen((v) => !v)}
              >
                <Menu className="h-4 w-4" />
              </Button>
            )}
            <DialogTitle>Chat</DialogTitle>
            {SIDEBAR_ENABLED && (
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="ml-auto"
                data-testid="chat-new-session"
                onClick={() => void startNewSession()}
              >
                <Plus className="h-4 w-4" />
                New chat
              </Button>
            )}
          </div>
          <DialogDescription>
            Ask anything — responses stream as the model generates them.
          </DialogDescription>
        </DialogHeader>

        {/* `relative` scopes the mobile drawer to the content area, so
            the hamburger in the header stays reachable while it is
            open. */}
        <div className="relative flex gap-3 min-h-0">
          {SIDEBAR_ENABLED && (
            <ChatSidebar
              onSessionClick={() => setSidebarOpen(false)}
              className={cn(
                "w-52 shrink-0",
                sidebarOpen
                  ? "absolute inset-y-0 left-0 z-10 w-64 bg-background shadow-lg md:static md:w-52 md:shadow-none"
                  : "hidden md:flex",
              )}
            />
          )}

          <div className="flex-1 min-w-0 flex flex-col gap-3">
            <div
              ref={listRef}
              className="max-h-[60vh] overflow-y-auto rounded-md border bg-muted/30 p-3 space-y-3"
              data-testid="chat-message-list"
            >
              {messages.length === 0 && (
                <p className="text-sm text-muted-foreground text-center py-8">
                  No messages yet — type below to start.
                </p>
              )}
              {messages.map((m, i) => (
                <div
                  key={i}
                  className={cn(
                    "flex",
                    m.role === "user" ? "justify-end" : "justify-start",
                  )}
                  data-testid={`chat-message-${m.role}`}
                >
                  <div
                    className={cn(
                      "max-w-[80%] rounded-lg px-3 py-2 text-sm whitespace-pre-wrap break-words",
                      m.role === "user"
                        ? "bg-primary text-primary-foreground"
                        : "bg-background border",
                    )}
                  >
                    {m.role === "assistant" && m.partialFailure && (
                      <PartialFailureBanner
                        failedSteps={m.partialFailure.failedSteps}
                      />
                    )}
                    {m.role === "assistant" &&
                      (m.steps ?? []).map((s) => {
                        const confirm = s.confirm;
                        if (!confirm) return null;
                        // Approval cards sit at the bubble level, not
                        // inside the "agent steps" disclosure: a paused
                        // run must be visibly actionable without the
                        // user expanding anything.
                        return (
                          <ConfirmationCard
                            key={`confirm-${s.id}`}
                            confirm={confirm}
                            onDecide={(decision) =>
                              submitApproval(
                                confirm.runId,
                                confirm.approvalId,
                                decision,
                              )
                            }
                          />
                        );
                      })}
                    {m.content ||
                      (isStreaming && m.role === "assistant" ? "…" : "")}
                    {m.role === "assistant" &&
                      m.notes?.map((n, ni) => (
                        <div
                          key={`note-${ni}`}
                          className="mt-1 text-[10px] italic text-muted-foreground"
                          data-testid="chat-note"
                        >
                          {n}
                        </div>
                      ))}
                    {m.role === "assistant" &&
                      m.steps &&
                      m.steps.length > 0 && (
                        <details
                          className="mt-2 text-xs"
                          data-testid="chat-agent-steps"
                          open={m.steps.some((s) => s.status === "running")}
                        >
                          <summary className="flex items-center gap-1 cursor-pointer select-none text-muted-foreground">
                            <ChevronDown
                              className="h-3 w-3"
                              aria-hidden="true"
                            />
                            {m.steps.length} step
                            {m.steps.length === 1 ? "" : "s"}
                          </summary>
                          <ul className="mt-1 space-y-2 border-l border-muted pl-2">
                            {m.steps.map((s) => (
                              <StepRow key={s.id} step={s} />
                            ))}
                          </ul>
                        </details>
                      )}
                    {m.role === "assistant" && m.usage && (
                      <div className="mt-1 text-[10px] text-muted-foreground">
                        tokens: {m.usage.total_tokens} (
                        prompt {m.usage.prompt_tokens} · completion{" "}
                        {m.usage.completion_tokens})
                      </div>
                    )}
                  </div>
                </div>
              ))}
              {error && (
                <div
                  className="text-xs text-destructive text-center"
                  data-testid="chat-error"
                  role="alert"
                >
                  {error}
                </div>
              )}
            </div>

            <form
              onSubmit={handleSubmit}
              className="flex flex-col gap-2"
              data-testid="chat-form"
            >
              <div className="flex items-center gap-2">
                <input
                  type="checkbox"
                  id="chat-use-feeds"
                  checked={useFeeds}
                  onChange={(e) => setUseFeeds(e.target.checked)}
                  disabled={isStreaming}
                  data-testid="chat-use-feeds"
                  className="h-4 w-4 rounded border-input accent-primary"
                  aria-label="Use my feeds"
                />
                <label
                  htmlFor="chat-use-feeds"
                  className="text-xs text-muted-foreground cursor-pointer"
                >
                  Use my feeds
                </label>
                {useFeeds && (
                  <span className="text-[10px] text-muted-foreground/70 ml-auto">
                    Answers grounded in your feeds
                  </span>
                )}
              </div>
              <Textarea
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={handleKeyDown}
                placeholder="Type a message…"
                rows={2}
                disabled={isStreaming}
                data-testid="chat-input"
              />
              <div className="flex gap-2 justify-end">
                {isStreaming && (
                  <Button
                    type="button"
                    variant="outline"
                    onClick={() => cancel()}
                    data-testid="chat-cancel"
                  >
                    <Square className="h-4 w-4" />
                    Cancel
                  </Button>
                )}
                <Button
                  type="submit"
                  disabled={isStreaming || draft.trim().length === 0}
                  data-testid="chat-send"
                >
                  <Send className="h-4 w-4" />
                  Send
                </Button>
              </div>
            </form>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
};
