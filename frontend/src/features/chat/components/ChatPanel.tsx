import React, { useEffect, useRef, useState } from "react";
import { Send, Square } from "lucide-react";
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
  const reset = useChat((s) => s.reset);

  const [draft, setDraft] = useState("");
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
      queueMicrotask(() => setDraft(""));
    }
    wasOpenRef.current = open;
  }, [open, reset]);

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
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Chat</DialogTitle>
          <DialogDescription>
            Ask anything — responses stream as the model generates them.
          </DialogDescription>
        </DialogHeader>

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
                {m.content || (isStreaming && m.role === "assistant" ? "…" : "")}
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
      </DialogContent>
    </Dialog>
  );
};
