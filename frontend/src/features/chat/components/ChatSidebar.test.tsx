import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ChatSidebar } from './ChatSidebar';
import { i18n } from '~/i18n';
import { useChatStore } from '../store/chatStore';
import { chatService } from '../services/chatService';
import { ChatApiError, type SessionSummary } from '../types/chat';

// The component drives the real store so the sidebar's behaviour
// (ordering, single-flight lazy-load, click-to-replay) is exercised
// end-to-end; only the network service is mocked.
vi.mock('../services/chatService');

const mockedService = chatService as unknown as {
  listSessions: ReturnType<typeof vi.fn>;
  getMessages: ReturnType<typeof vi.fn>;
  resumeSession: ReturnType<typeof vi.fn>;
  deleteSession: ReturnType<typeof vi.fn>;
};

// `IntersectionObserver` comes from src/test/setup.js. The stub keeps a
// reference to the most recent instance and exposes `trigger()` so a
// test can simulate the sentinel entering the viewport without layout.
const IntersectionObserverStub = globalThis.IntersectionObserver as unknown as {
  last: {
    trigger: (isIntersecting: boolean, target?: Element | null) => void;
    root: Element | null;
    rootMargin: string;
  } | null;
  reset: () => void;
};

const CURRENT_SESSION_KEY = 'nyx.chat.currentSessionId';

function makeSession(id: string, overrides: Partial<SessionSummary> = {}): SessionSummary {
  return {
    id,
    user_id: 1,
    title: '',
    started_at: '2026-10-14T00:00:00Z',
    last_active_at: '2026-10-14T00:00:00Z',
    ended_at: null,
    message_count: 2,
    ...overrides,
  };
}

function resetStore(
  overrides: Partial<ReturnType<typeof useChatStore.getState>> = {},
) {
  useChatStore.setState({
    messages: [],
    isStreaming: false,
    error: null,
    currentSessionId: null,
    sessionList: [],
    sessionListNextCursor: null,
    isFetchingMore: false,
    runningSessionIds: [],
    ...overrides,
  });
  sessionStorage.clear();
}

describe('ChatSidebar', () => {
  beforeEach(() => {
    IntersectionObserverStub.reset();
    resetStore();
    mockedService.listSessions
      .mockReset()
      .mockResolvedValue({ sessions: [], nextCursor: null });
    mockedService.getMessages.mockReset().mockResolvedValue([]);
    mockedService.resumeSession.mockReset().mockResolvedValue(undefined);
    mockedService.deleteSession.mockReset().mockResolvedValue(undefined);
  });

  it('renders rows in list order with title, placeholder, and absolute + relative time', () => {
    const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000).toISOString();
    const threeDaysAgo = new Date(Date.now() - 3 * 24 * 60 * 60 * 1000).toISOString();
    resetStore({
      sessionList: [
        makeSession('s3', { title: 'Trip planning', last_active_at: fiveMinAgo }),
        makeSession('s2', { last_active_at: threeDaysAgo }),
      ],
    });

    render(<ChatSidebar />);

    const rows = screen.getAllByTestId('chat-session-row');
    // Rendered verbatim — the server owns `last_active_at DESC`.
    expect(rows.map((r) => r.getAttribute('data-session-id'))).toEqual([
      's3',
      's2',
    ]);
    // Empty title → generic placeholder (nothing client-side knows the
    // first message).
    expect(screen.getByText('Trip planning')).toBeInTheDocument();
    expect(screen.getByText('New chat')).toBeInTheDocument();

    const times = screen.getAllByTestId('chat-session-time');
    expect(times[0]).toHaveTextContent(/^\d{2}-\d{2} \d{2}:\d{2} · 5 min ago$/);
    expect(times[1]).toHaveTextContent(
      /^\d{2}-\d{2} \d{2}:\d{2} · 3 days ago$/,
    );
    expect(times[0]).toHaveAttribute('title', fiveMinAgo);
    expect(times[1]).toHaveAttribute('title', threeDaysAgo);
    expect(times[0].className).toMatch(/text-\[11px\]/);
  });

  it('renders the absolute date for a session older than a year', () => {
    const longAgo = new Date(Date.now() - 400 * 24 * 60 * 60 * 1000).toISOString();
    resetStore({
      sessionList: [makeSession('s-old', { last_active_at: longAgo })],
    });

    render(<ChatSidebar />);

    const time = screen.getByTestId('chat-session-time');
    expect(time).toHaveTextContent(/^\d{2}-\d{2} \d{2}:\d{2} · 1 year ago$/);
    expect(time).toHaveAttribute('title', longAgo);
  });

  it('shows the empty state when the first page came back empty', async () => {
    render(<ChatSidebar />);

    // The mount effect kicks off the first page; the placeholder only
    // appears once it resolves empty.
    expect(screen.getByTestId('chat-session-loading')).toBeInTheDocument();
    expect(await screen.findByTestId('chat-session-empty')).toHaveTextContent(
      'No conversations yet',
    );
    expect(screen.queryByTestId('chat-session-sentinel')).toBeNull();
  });

  it('shows a loading state while the first page is in flight', () => {
    resetStore({ isFetchingMore: true });

    render(<ChatSidebar />);

    expect(screen.getByTestId('chat-session-loading')).toHaveTextContent(
      'Loading…',
    );
    expect(screen.queryByTestId('chat-session-empty')).toBeNull();
  });

  it('fetches the first page on mount', async () => {
    mockedService.listSessions.mockResolvedValueOnce({
      sessions: [makeSession('s1')],
      nextCursor: null,
    });

    render(<ChatSidebar />);

    await waitFor(() => {
      expect(mockedService.listSessions).toHaveBeenCalledTimes(1);
    });
    expect(await screen.findByTestId('chat-session-row')).toHaveAttribute(
      'data-session-id',
      's1',
    );
  });

  it('drops a burst of sentinel intersections down to one page fetch', async () => {
    resetStore({
      sessionList: [makeSession('s2')],
      sessionListNextCursor: 'cursor-1',
    });
    mockedService.listSessions.mockResolvedValueOnce({
      sessions: [makeSession('s1')],
      nextCursor: null,
    });

    render(<ChatSidebar />);

    const observer = IntersectionObserverStub.last;
    expect(observer).not.toBeNull();
    expect(screen.getByTestId('chat-session-sentinel')).toBeInTheDocument();

    // Two synchronous intersections (the "scroll fast" case) — the
    // store's single-flight guard drops the second.
    observer?.trigger(true);
    observer?.trigger(true);

    await waitFor(() => {
      expect(mockedService.listSessions).toHaveBeenCalledTimes(1);
    });
    expect(mockedService.listSessions).toHaveBeenCalledWith({
      before: 'cursor-1',
    });
    await waitFor(() => {
      expect(screen.getAllByTestId('chat-session-row')).toHaveLength(2);
    });
  });

  it('removes the sentinel when the list is complete', () => {
    resetStore({
      sessionList: [makeSession('s1')],
      sessionListNextCursor: null,
    });

    render(<ChatSidebar />);

    expect(screen.queryByTestId('chat-session-sentinel')).toBeNull();
    expect(IntersectionObserverStub.last).toBeNull();
  });

  it('loads and replays the clicked session', async () => {
    const onSessionClick = vi.fn();
    resetStore({ sessionList: [makeSession('s1'), makeSession('s2')] });
    mockedService.getMessages.mockResolvedValueOnce([
      {
        id: 1,
        role: 'user',
        content: 'from history',
        tool_calls: null,
        steps: null,
        tokens: 0,
        created_at: '2026-10-14T00:00:00Z',
      },
    ]);

    render(<ChatSidebar onSessionClick={onSessionClick} />);

    await userEvent.click(screen.getAllByTestId('chat-session-row')[1]);

    await waitFor(() => {
      expect(mockedService.getMessages).toHaveBeenCalledWith('s2');
    });
    expect(mockedService.resumeSession).toHaveBeenCalledWith('s2');
    // The drawer-close hook fires so a mobile drawer collapses.
    expect(onSessionClick).toHaveBeenCalledTimes(1);

    await waitFor(() => {
      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'from history' },
      ]);
    });
    expect(screen.getAllByTestId('chat-session-row')[1]).toHaveAttribute(
      'data-active',
      'true',
    );
    expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBe('s2');
  });

  it('is a no-op when the active session row is clicked', async () => {
    const onSessionClick = vi.fn();
    resetStore({
      sessionList: [makeSession('s1')],
      currentSessionId: 's1',
      messages: [{ role: 'user', content: 'kept' }],
    });

    render(<ChatSidebar onSessionClick={onSessionClick} />);
    await userEvent.click(screen.getByTestId('chat-session-row'));

    expect(mockedService.resumeSession).not.toHaveBeenCalled();
    expect(mockedService.getMessages).not.toHaveBeenCalled();
    // The dismiss hook still fires (modal semantics — any tap leaves
    // the mobile overlay); what stays untouched is the panel state.
    expect(onSessionClick).toHaveBeenCalledTimes(1);
    expect(useChatStore.getState().messages).toEqual([
      { role: 'user', content: 'kept' },
    ]);
  });

  it('deletes a row and drops it from the list', async () => {
    resetStore({
      sessionList: [makeSession('s1'), makeSession('s2')],
      currentSessionId: 's1',
    });

    render(<ChatSidebar />);
    await userEvent.click(screen.getAllByTestId('chat-session-delete')[1]);

    await waitFor(() => {
      expect(mockedService.deleteSession).toHaveBeenCalledWith('s2');
    });
    await waitFor(() => {
      expect(screen.getAllByTestId('chat-session-row')).toHaveLength(1);
    });
    // Deleting a non-active session leaves the active one alone.
    expect(useChatStore.getState().currentSessionId).toBe('s1');
  });

  it('clears the panel when the active session is deleted', async () => {
    resetStore({
      sessionList: [makeSession('s1')],
      currentSessionId: 's1',
      messages: [{ role: 'user', content: 'gone' }],
    });
    sessionStorage.setItem(CURRENT_SESSION_KEY, 's1');

    render(<ChatSidebar />);
    await userEvent.click(screen.getByTestId('chat-session-delete'));

    await waitFor(() => {
      expect(useChatStore.getState().currentSessionId).toBeNull();
    });
    expect(useChatStore.getState().messages).toEqual([]);
    expect(useChatStore.getState().sessionList).toEqual([]);
    expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBeNull();
  });

  it('keeps the row (and stays quiet) when the delete fails transiently', async () => {
    resetStore({ sessionList: [makeSession('s1')] });
    mockedService.deleteSession.mockRejectedValueOnce(new Error('network down'));

    render(<ChatSidebar />);
    await userEvent.click(screen.getByTestId('chat-session-delete'));

    await waitFor(() => {
      expect(mockedService.deleteSession).toHaveBeenCalled();
    });
    expect(useChatStore.getState().sessionList.map((s) => s.id)).toEqual(['s1']);
  });

  it('drops a stale row when the server reports it already deleted', async () => {
    resetStore({ sessionList: [makeSession('s1')] });
    mockedService.deleteSession.mockRejectedValueOnce(
      new ChatApiError(404, 'Chat request failed (404)'),
    );

    render(<ChatSidebar />);
    await userEvent.click(screen.getByTestId('chat-session-delete'));

    await waitFor(() => {
      expect(useChatStore.getState().sessionList).toEqual([]);
    });
  });

  it('marks the active row for assistive tech and styling', () => {
    resetStore({
      sessionList: [makeSession('s1'), makeSession('s2')],
      currentSessionId: 's2',
    });

    render(<ChatSidebar />);

    const [first, second] = screen.getAllByTestId('chat-session-row');
    expect(first).not.toHaveAttribute('aria-current');
    expect(second).toHaveAttribute('aria-current', 'true');
  });

  it('shows a running indicator only on rows with a live detached run', () => {
    resetStore({
      sessionList: [makeSession('s1'), makeSession('s2'), makeSession('s3')],
      // A background run is still streaming into s2 (the panel may be
      // showing any of the three).
      runningSessionIds: ['s2'],
    });

    render(<ChatSidebar />);

    const indicators = screen.getAllByTestId('chat-session-running');
    expect(indicators).toHaveLength(1);
    const row = indicators[0].closest('[data-testid="chat-session-row"]');
    expect(row).toHaveAttribute('data-session-id', 's2');
    // Screen-reader affordance: the dot itself is decorative, the
    // wrapper carries the label.
    expect(indicators[0]).toHaveAttribute('role', 'status');
    expect(indicators[0]).toHaveAttribute(
      'aria-label',
      'Agent is still working on this conversation',
    );
  });

  it('removes the running indicator once the run finishes', async () => {
    resetStore({
      sessionList: [makeSession('s1')],
      runningSessionIds: ['s1'],
    });

    render(<ChatSidebar />);
    expect(screen.getAllByTestId('chat-session-running')).toHaveLength(1);

    // The stream's terminal frame clears the marker (store-side);
    // the row re-renders without it.
    resetStore({
      sessionList: [makeSession('s1')],
      runningSessionIds: [],
    });
    await waitFor(() => {
      expect(screen.queryByTestId('chat-session-running')).toBeNull();
    });
  });

  it('renders the Chinese labels and time buckets under the zh locale', async () => {
    const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000).toISOString();
    const threeDaysAgo = new Date(Date.now() - 3 * 24 * 60 * 60 * 1000).toISOString();
    await i18n.changeLanguage('zh');
    resetStore({
      sessionList: [
        makeSession('s3', { title: 'Trip planning', last_active_at: fiveMinAgo }),
        makeSession('s2', { last_active_at: threeDaysAgo }),
      ],
    });

    render(<ChatSidebar />);

    expect(screen.getByText('历史')).toBeInTheDocument();
    expect(screen.getByText('新建对话')).toBeInTheDocument();
    expect(screen.getByText('Trip planning')).toBeInTheDocument();

    const times = screen.getAllByTestId('chat-session-time');
    expect(times[0]).toHaveTextContent(/^\d{2}-\d{2} \d{2}:\d{2} · 5 分钟前$/);
    expect(times[1]).toHaveTextContent(/^\d{2}-\d{2} \d{2}:\d{2} · 3 天前$/);

    await i18n.changeLanguage('en');
  });
});
