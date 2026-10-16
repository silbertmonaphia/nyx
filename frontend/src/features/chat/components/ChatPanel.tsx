import React, { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
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
  const { t } = useTranslation();
  const argsPreview = formatJsonPreview(step.toolArgs);
  const resultPreview = formatJsonPreview(step.toolResult);
  const statusLabel =
    step.status === "running"
      ? t("chat.status.running")
      : step.status === "ok"
        ? t("chat.status.ok")
        : t("chat.status.failed");
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
            title={t("chat.retryTitle", { count: step.retryCount })}
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
          <summary className="cursor-pointer select-none">
            {t("chat.stepArgs")}
          </summary>
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
          {t("chat.stepResult", { preview: resultPreview })}
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
  const { t } = useTranslation();
  const messages = useChat((s) => s.messages);
  const isStreaming = useChat((s) => s.isStreaming);
  const error = useChat((s) => s.error);
  const send = useChat((s) => s.send);
  const cancel = useChat((s) => s.cancel);
  const submitApproval = useChat((s) => s.submitApproval);
  const expireApproval = useChat((s) => s.expireApproval);
  const reset = useChat((s) => s.reset);
  const hydrateSession = useChat((s) => s.hydrateSession);
  const startNewSession = useChat((s) => s.startNewSession);
  // The active session still has a detached background run streaming
  // into it (navigation moved this panel on, the run kept going).
  // The composer locks — a second concurrent run on one session would
  // interleave its user/assistant rows — and a hint explains why.
  const sessionBusy = useChat(
    (s) =>
      s.currentSessionId !== null &&
      s.runningSessionIds.includes(s.currentSessionId),
  );
  const composerLocked = isStreaming || sessionBusy;

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
    // Mirror the store's own guards (`isStreaming` + busy-session lock):
    // an implicit submit from a nested control (Enter on the checkbox)
    // can reach here even with the textarea disabled, and clearing the
    // draft first would silently eat it.
    if (!text || composerLocked) return;
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
                aria-label={
                  sidebarOpen ? t("chat.hideHistory") : t("chat.showHistory")
                }
                aria-expanded={sidebarOpen}
                data-testid="chat-sidebar-toggle"
                onClick={() => setSidebarOpen((v) => !v)}
              >
                <Menu className="h-4 w-4" />
              </Button>
            )}
            <DialogTitle>{t("chat.title")}</DialogTitle>
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
                {t("chat.newChat")}
              </Button>
            )}
          </div>
          <DialogDescription>{t("chat.description")}</DialogDescription>
        </DialogHeader>

        {/* `relative` scopes the mobile drawer to the content area, so
            the hamburger in the header stays reachable while it is
            open. The `max-h` bounds the row so the rail's own scroll
            container engages instead of the dialog growing with the
            session count. */}
        <div className="relative flex gap-3 min-h-0 max-h-[70vh]">
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
                  {t("chat.emptyState")}
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
                            onExpire={() => expireApproval(confirm.approvalId)}
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
                            {t("chat.steps", { count: m.steps.length })}
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
                        {t("chat.tokens", {
                          total: m.usage.total_tokens,
                          prompt: m.usage.prompt_tokens,
                          completion: m.usage.completion_tokens,
                        })}
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
              {sessionBusy && (
                <p
                  className="text-[10px] text-muted-foreground"
                  data-testid="chat-session-busy-hint"
                  role="status"
                >
                  {t("chat.busyHint")}
                </p>
              )}
              <Textarea
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={handleKeyDown}
                placeholder={t("chat.placeholder")}
                rows={2}
                disabled={composerLocked}
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
                    {t("common.cancel")}
                  </Button>
                )}
                <Button
                  type="submit"
                  disabled={composerLocked || draft.trim().length === 0}
                  data-testid="chat-send"
                >
                  <Send className="h-4 w-4" />
                  {t("chat.send")}
                </Button>
              </div>
            </form>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
};
