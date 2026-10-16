import React, { useEffect, useRef } from "react";
import { useTranslation } from "react-i18next";
import { History, Trash2 } from "lucide-react";
import { cn } from "~/utils/cn";
import { DEFAULT_LOCALE, i18n } from "~/i18n";
import { useChat } from "../hooks/useChat";

/**
 * Format a timestamp as a short relative label ("5 min ago",
 * "3 days ago"). The label is orientation, not precision: buckets
 * collapse upward (45 s → "just now", 25 h → "1 day ago"), and a
 * future `last_active_at` (clock skew between the browser and the
 * server) collapses to "just now" rather than a negative duration.
 */
function relativeTime(iso: string): string {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "";

  const seconds = Math.floor((Date.now() - then) / 1000);
  if (seconds < 60) return i18n.t("time.justNow");
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return i18n.t("time.minutesAgo", { count: minutes });
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return i18n.t("time.hoursAgo", { count: hours });
  const days = Math.floor(hours / 24);
  if (days < 7) return i18n.t("time.daysAgo", { count: days });
  if (days < 30) {
    return i18n.t("time.weeksAgo", { count: Math.floor(days / 7) });
  }
  if (days < 365) {
    return i18n.t("time.monthsAgo", { count: Math.floor(days / 30) });
  }
  return i18n.t("time.yearsAgo", { count: Math.floor(days / 365) });
}

const absoluteFormatters = new Map<string, Intl.DateTimeFormat>();

function absoluteFormatter(locale: string): Intl.DateTimeFormat {
  let formatter = absoluteFormatters.get(locale);
  if (!formatter) {
    formatter = new Intl.DateTimeFormat(locale, {
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    });
    absoluteFormatters.set(locale, formatter);
  }
  return formatter;
}

function absoluteTime(iso: string): string {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "";
  const parts = absoluteFormatter(i18n.language || DEFAULT_LOCALE).formatToParts(
    new Date(then),
  );
  const value = (type: Intl.DateTimeFormatPartTypes) =>
    parts.find((part) => part.type === type)?.value ?? "";
  return `${value("month")}-${value("day")} ${value("hour")}:${value("minute")}`;
}

function timeLabel(iso: string): string {
  return [absoluteTime(iso), relativeTime(iso)].filter(Boolean).join(" · ");
}

interface ChatSidebarProps {
  /**
   * Fired on every row click, before the store call — the mobile drawer
   * uses it to close itself (spec Scenario "Narrow viewport shows the
   * drawer": clicking a session in the drawer closes it).
   */
  onSessionClick?: () => void;
  className?: string;
}

/**
 * Conversation history rail. Renders the store's `sessionList`
 * newest-first, lazy-loads the next page when a sentinel at the bottom
 * comes within 200px, and drives replay through `setCurrentSession`.
 *
 * Visibility is the caller's concern: `ChatPanel` shows this as a
 * static left column at `md:` and up, and as an overlay drawer below it
 * — the same DOM either way, so the list state survives the switch.
 */
export const ChatSidebar: React.FC<ChatSidebarProps> = ({
  onSessionClick,
  className,
}) => {
  const { t } = useTranslation();
  const sessionList = useChat((s) => s.sessionList);
  const nextCursor = useChat((s) => s.sessionListNextCursor);
  const currentSessionId = useChat((s) => s.currentSessionId);
  const isFetchingMore = useChat((s) => s.isFetchingMore);
  const runningSessionIds = useChat((s) => s.runningSessionIds);
  const loadMore = useChat((s) => s.loadMore);
  const setCurrentSession = useChat((s) => s.setCurrentSession);
  const deleteSession = useChat((s) => s.deleteSession);

  const sentinelRef = useRef<HTMLDivElement | null>(null);
  const scrollRef = useRef<HTMLDivElement | null>(null);

  // First page on mount. Read through `getState` so the effect runs once
  // per mount instead of on every list mutation; a remount (dialog
  // reopen) refetches when the list is empty, which also refreshes the
  // `last_active_at` ordering.
  useEffect(() => {
    if (useChat.getState().sessionList.length === 0) {
      void loadMore();
    }
  }, [loadMore]);

  // Lazy-load the next page. The observer is pinned to the scroll
  // container (not the document viewport) and skipped while a page is in
  // flight, mirroring `FeedList`. When `next_cursor` is null the sentinel
  // is not rendered at all, so no observer is installed and the list
  // stops fetching.
  useEffect(() => {
    if (nextCursor === null || isFetchingMore) return;
    const node = sentinelRef.current;
    const root = scrollRef.current;
    if (!node || !root) return;

    const observer = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting) {
          void loadMore();
        }
      },
      { root, rootMargin: "200px" },
    );
    observer.observe(node);
    return () => observer.disconnect();
  }, [nextCursor, isFetchingMore, loadMore]);

  const handleSelect = (id: string) => {
    // The hook fires for EVERY row tap — it is modal-dismiss semantics:
    // on mobile the rail is an overlay and any tap returns the user to
    // the panel (this is what the drawer e2e pins). What the spec's
    // "clicking the active session is a no-op" governs is the
    // panel/session state below: no refetch, no replay, no clear.
    onSessionClick?.();
    if (id === currentSessionId) return;
    void setCurrentSession(id);
  };

  const showFirstPageSkeleton = isFetchingMore && sessionList.length === 0;

  return (
    <aside
      data-testid="chat-sidebar"
      aria-label={t("chat.historyAria")}
      className={cn("flex flex-col min-h-0", className)}
    >
      <div className="flex items-center gap-1.5 px-2 py-1.5 text-xs font-medium uppercase tracking-wide text-muted-foreground">
        <History className="h-3 w-3" aria-hidden="true" />
        {t("chat.history")}
      </div>
      <div
        ref={scrollRef}
        data-testid="chat-session-list"
        className="flex-1 min-h-0 overflow-y-auto rounded-md border bg-muted/30 p-1"
      >
        {showFirstPageSkeleton && (
          <p
            className="px-2 py-3 text-xs text-muted-foreground"
            data-testid="chat-session-loading"
          >
            {t("chat.loading")}
          </p>
        )}
        {!showFirstPageSkeleton && sessionList.length === 0 && (
          <p
            className="px-2 py-3 text-xs text-muted-foreground"
            data-testid="chat-session-empty"
          >
            {t("chat.noConversations")}
          </p>
        )}
        <ul className="space-y-1">
          {sessionList.map((session) => {
            const isActive = session.id === currentSessionId;
            const storedTitle = session.title.trim();
            const label =
              storedTitle === "" ? t("chat.untitledSession") : storedTitle;
            return (
              <li key={session.id} className="group relative">
                <button
                  type="button"
                  onClick={() => handleSelect(session.id)}
                  aria-current={isActive ? "true" : undefined}
                  data-testid="chat-session-row"
                  data-session-id={session.id}
                  data-active={isActive ? "true" : undefined}
                  className={cn(
                    "block w-full rounded-md py-1.5 pl-2 pr-7 text-left transition-colors",
                    isActive
                      ? "bg-accent text-accent-foreground"
                      : "hover:bg-accent/50",
                  )}
                >
                  <span
                    className="block truncate text-sm"
                    data-testid="chat-session-title"
                  >
                    {label}
                  </span>
                  <span
                    className="block text-[11px] text-muted-foreground"
                    data-testid="chat-session-time"
                    title={session.last_active_at}
                  >
                    {timeLabel(session.last_active_at)}
                    {runningSessionIds.includes(session.id) && (
                      // A detached run is still streaming into this
                      // session (the panel moved on but the SSE
                      // connection stayed open — see chatStore's
                      // `detachLiveStream`). The indicator lives in
                      // `runningSessionIds` until the run's terminal
                      // frame.
                      <span
                        className="ml-1 inline-flex items-center align-middle"
                        data-testid="chat-session-running"
                        role="status"
                        aria-label={t("chat.stillWorking")}
                        title={t("chat.stillWorking")}
                      >
                        <span
                          className="h-1.5 w-1.5 rounded-full bg-emerald-500 animate-pulse"
                          aria-hidden="true"
                        />
                      </span>
                    )}
                  </span>
                </button>
                <button
                  type="button"
                  aria-label={t("chat.deleteSession", { label })}
                  data-testid="chat-session-delete"
                  data-session-id={session.id}
                  onClick={() => void deleteSession(session.id)}
                  className={cn(
                    "absolute right-1 top-1 rounded p-1 text-muted-foreground transition-opacity",
                    "opacity-0 hover:text-destructive",
                    "group-hover:opacity-100 group-focus-within:opacity-100 focus-visible:opacity-100",
                  )}
                >
                  <Trash2 className="h-3 w-3" aria-hidden="true" />
                </button>
              </li>
            );
          })}
        </ul>
        {nextCursor !== null && sessionList.length > 0 && (
          <div
            ref={sentinelRef}
            data-testid="chat-session-sentinel"
            aria-hidden="true"
            className="h-px"
          />
        )}
      </div>
    </aside>
  );
};
