import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ChatPanel } from './ChatPanel';
import { useChatStore } from '../store/chatStore';
import { chatService } from '../services/chatService';
import type { SessionMessage, SessionSummary } from '../types/chat';

vi.mock('../services/chatService');

const mockedService = chatService as unknown as {
  streamMessage: ReturnType<typeof vi.fn>;
  listSessions: ReturnType<typeof vi.fn>;
  getMessages: ReturnType<typeof vi.fn>;
  resumeSession: ReturnType<typeof vi.fn>;
  createSession: ReturnType<typeof vi.fn>;
  deleteSession: ReturnType<typeof vi.fn>;
};

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

function makeRow(
  id: number,
  role: 'user' | 'assistant',
  content: string,
  toolCalls: SessionMessage['tool_calls'] = null,
): SessionMessage {
  return {
    id,
    role,
    content,
    tool_calls: toolCalls,
    tokens: 0,
    created_at: '2026-10-14T00:00:00Z',
  };
}

function resetStore() {
  useChatStore.setState({
    messages: [],
    isStreaming: false,
    error: null,
    currentSessionId: null,
    sessionList: [],
    sessionListNextCursor: null,
    isFetchingMore: false,
  });
  sessionStorage.clear();
}

describe('ChatPanel', () => {
  beforeEach(() => {
    resetStore();
    mockedService.streamMessage.mockReset();
    // The panel hosts the history rail, which fetches its first page on
    // mount — give the session endpoints benign defaults so unrelated
    // tests aren't racing a rejected mock.
    mockedService.listSessions
      .mockReset()
      .mockResolvedValue({ sessions: [], nextCursor: null });
    mockedService.getMessages.mockReset().mockResolvedValue([]);
    mockedService.resumeSession.mockReset().mockResolvedValue(undefined);
    mockedService.createSession
      .mockReset()
      .mockResolvedValue({ session_id: 's-new' });
    mockedService.deleteSession.mockReset().mockResolvedValue(undefined);
  });

  it('sends a message and renders the assistant bubble with concatenated content', async () => {
    // Yield two deltas + a done so the assistant row ends up with
    // "Hello" and a usage block.
    mockedService.streamMessage.mockImplementationOnce(
      async function* () {
        yield { kind: 'delta', delta: 'Hel' };
        yield { kind: 'delta', delta: 'lo' };
        yield {
          kind: 'done',
          usage: {
            prompt_tokens: 1,
            completion_tokens: 2,
            total_tokens: 3,
          },
        };
        yield { kind: 'terminator' };
      },
    );

    const onOpenChange = vi.fn();
    render(<ChatPanel open={true} onOpenChange={onOpenChange} />);

    // Type into the textarea and submit.
    const input = screen.getByTestId('chat-input') as HTMLTextAreaElement;
    await userEvent.type(input, 'Say hi');
    await userEvent.click(screen.getByTestId('chat-send'));

    // Wait for the user message bubble to appear.
    await waitFor(() => {
      expect(screen.getByTestId('chat-message-user')).toHaveTextContent('Say hi');
    });

    // The deltas get concatenated into a single assistant bubble.
    await waitFor(() => {
      const bubbles = screen.getAllByTestId('chat-message-assistant');
      expect(bubbles.length).toBe(1);
      expect(bubbles[0]).toHaveTextContent('Hello');
    });

    // Usage is rendered once the done event lands.
    await waitFor(() => {
      expect(screen.getByTestId('chat-message-assistant')).toHaveTextContent(
        /tokens: 3/,
      );
    });

    // Service received the user message in the request body (no
    // model field — the brief is explicit).
    expect(mockedService.streamMessage).toHaveBeenCalledTimes(1);
    const [sentMessages] = mockedService.streamMessage.mock.calls[0];
    expect(sentMessages).toEqual([{ role: 'user', content: 'Say hi' }]);
  });

  it('disables the Send button while streaming and shows Cancel', async () => {
    // Hold the stream open so we can observe the disabled state.
    // The mock honors the AbortSignal so clicking Cancel actually
    // unblocks the store's for-await loop.
    let abortListener: (() => void) | null = null;
    mockedService.streamMessage.mockImplementationOnce(async function* () {
      yield { kind: 'delta', delta: '...' };
      await new Promise<void>((resolve) => {
        abortListener = resolve;
      });
    });

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    const input = screen.getByTestId('chat-input') as HTMLTextAreaElement;
    await userEvent.type(input, 'hold the line');
    await userEvent.click(screen.getByTestId('chat-send'));

    // Mid-stream: Send is disabled and Cancel appears.
    await waitFor(() => {
      expect(screen.getByTestId('chat-send')).toBeDisabled();
    });
    expect(screen.getByTestId('chat-cancel')).toBeInTheDocument();

    // Cancel aborts the stream — isStreaming flips back to false.
    await userEvent.click(screen.getByTestId('chat-cancel'));
    if (abortListener) abortListener();

    // After cancel: isStreaming is false. The draft is also empty
    // (the store's send() clears it on submit), so Send is disabled
    // for an independent reason — but a fresh type re-enables it.
    await waitFor(() => {
      expect(useChatStore.getState().isStreaming).toBe(false);
    });
    await userEvent.type(input, 'again');
    expect(screen.getByTestId('chat-send')).not.toBeDisabled();
  });

  it('renders a chat error from the store', async () => {
    // Seed the store directly — simulates a mid-stream error that
    // the store already recorded.
    useChatStore.setState({
      messages: [{ role: 'user', content: 'hi' }],
      isStreaming: false,
      error: 'boom',
    });

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    expect(screen.getByTestId('chat-error')).toHaveTextContent('boom');
  });

  it('renders chat-note testids on an assistant row that carries notes', async () => {
    // Seed the store directly — simulates a mid-stream failover
    // where the router emitted `event: note` and the assistant row
    // accumulated the note text. The panel should render it as an
    // italic muted line without disturbing the main content.
    useChatStore.setState({
      messages: [
        { role: 'user', content: 'hi' },
        {
          role: 'assistant',
          content: 'Hello',
          notes: ['[continued on OpenAI]'],
        },
      ],
      isStreaming: false,
      error: null,
    });

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    const note = screen.getByTestId('chat-note');
    expect(note).toHaveTextContent('[continued on OpenAI]');
    // The note sits next to the assistant content (same bubble), and
    // the italic utility class is what drives the muted look.
    const assistantBubble = screen.getByTestId('chat-message-assistant');
    expect(assistantBubble).toContainElement(note);
    expect(note.className).toMatch(/italic/);
  });
});

describe('ChatPanel useFeeds toggle', () => {
  it('renders the checkbox and lets the user flip it', async () => {
    render(<ChatPanel open={true} onOpenChange={() => {}} />);
    const toggle = screen.getByTestId('chat-use-feeds') as HTMLInputElement;
    expect(toggle.checked).toBe(false);

    await userEvent.click(toggle);
    expect(toggle.checked).toBe(true);
  });

  it('disables the toggle while a stream is in flight', async () => {
    // Seed a fake streaming state by writing into the store
    // directly — the user-facing send path is exercised in
    // chatStore.test.ts.
    useChatStore.setState({ isStreaming: true });
    render(<ChatPanel open={true} onOpenChange={() => {}} />);
    const toggle = screen.getByTestId('chat-use-feeds') as HTMLInputElement;
    expect(toggle.disabled).toBe(true);

    useChatStore.setState({ isStreaming: false });
  });

  it('shows a "grounded in your feeds" hint when the toggle is on', async () => {
    useChatStore.setState({ useFeeds: true });
    render(<ChatPanel open={true} onOpenChange={() => {}} />);
    expect(screen.getByText(/grounded in your feeds/i)).toBeInTheDocument();
    useChatStore.setState({ useFeeds: false });
  });
});

// ---- Agent-loop steps region (openspec/changes/agent-loop) ----
// When the assistant row carries `steps`, ChatPanel renders an
// "agent steps" <details> with one row per step. The single-turn
// path emits no steps, so this region stays hidden.

describe('ChatPanel agent steps', () => {
  beforeEach(() => {
    resetStore();
  });

  it('renders nothing in single-turn mode (no steps block)', () => {
    // Seed a clean single-turn assistant row — no `steps` field.
    useChatStore.setState({
      messages: [
        { role: 'user', content: 'hi' },
        { role: 'assistant', content: 'hello' },
      ],
      isStreaming: false,
      error: null,
    });

    render(<ChatPanel open={true} onOpenChange={() => {}} />);

    expect(screen.queryByTestId('chat-agent-steps')).not.toBeInTheDocument();
    expect(screen.queryByTestId('chat-agent-step')).not.toBeInTheDocument();
  });

  it('renders the steps block when the assistant row carries steps', () => {
    useChatStore.setState({
      messages: [
        { role: 'user', content: 'hi' },
        {
          role: 'assistant',
          content: 'final answer',
          steps: [
            { id: 's1', kind: 'rag', status: 'ok' },
            {
              id: 's2',
              kind: 'tool',
              status: 'ok',
              toolName: 'list_feeds',
              toolArgs: { limit: 10 },
              toolResult: [{ id: 1 }],
            },
          ],
        },
      ],
      isStreaming: false,
      error: null,
    });

    render(<ChatPanel open={true} onOpenChange={() => {}} />);

    const block = screen.getByTestId('chat-agent-steps');
    expect(block).toBeInTheDocument();
    const rows = screen.getAllByTestId('chat-agent-step');
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveAttribute('data-step-id', 's1');
    expect(rows[0]).toHaveAttribute('data-step-status', 'ok');
    expect(rows[1]).toHaveAttribute('data-step-id', 's2');

    // Tool name surfaces as a labelled span so a screen reader
    // announces it; the args stay collapsed by default.
    expect(screen.getByTestId('chat-agent-step-tool')).toHaveTextContent(
      'list_feeds',
    );
    expect(screen.getByTestId('chat-agent-step-args')).toBeInTheDocument();
    expect(screen.getByTestId('chat-agent-step-result')).toHaveTextContent(
      /result:/,
    );
  });

  it('renders a failure row with status=failed and reason', () => {
    useChatStore.setState({
      messages: [
        { role: 'user', content: 'hi' },
        {
          role: 'assistant',
          content: 'partial',
          steps: [
            {
              id: 's1',
              kind: 'react',
              status: 'failed',
              reason: 'react_max_iterations',
            },
          ],
        },
      ],
      isStreaming: false,
      error: null,
    });

    render(<ChatPanel open={true} onOpenChange={() => {}} />);

    const row = screen.getByTestId('chat-agent-step');
    expect(row).toHaveAttribute('data-step-status', 'failed');
    expect(screen.getByTestId('chat-agent-step-reason')).toHaveTextContent(
      'react_max_iterations',
    );
  });

  it('streams steps into the panel as events arrive', async () => {
    // Drive a real stream through the store so the panel sees the
    // steps accumulate live, then assert the final state.
    mockedService.streamMessage.mockImplementationOnce(
      async function* () {
        yield {
          kind: 'plan',
          steps: [{ id: 's1', kind: 'tool', tool: 'list_feeds' }],
        };
        yield { kind: 'step_start', step_id: 's1', step_kind: 'tool' };
        yield {
          kind: 'tool_call',
          step_id: 's1',
          call_id: 'c1',
          name: 'list_feeds',
          args: { limit: 5 },
        };
        yield {
          kind: 'tool_result',
          step_id: 's1',
          call_id: 'c1',
          ok: true,
          result: [],
        };
        yield { kind: 'step_end', step_id: 's1', status: 'ok', reason: '' };
        yield { kind: 'delta', delta: 'final ' };
        yield {
          kind: 'done',
          usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
        };
        yield { kind: 'terminator' };
      },
    );

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    const input = screen.getByTestId('chat-input') as HTMLTextAreaElement;
    await userEvent.type(input, 'list them');
    await userEvent.click(screen.getByTestId('chat-send'));

    // Wait for the streamed content + steps to land.
    await waitFor(() => {
      expect(screen.getByTestId('chat-agent-steps')).toBeInTheDocument();
    });
    await waitFor(() => {
      expect(screen.getByTestId('chat-message-assistant')).toHaveTextContent(
        'final ',
      );
    });

    const tool = screen.getByTestId('chat-agent-step-tool');
    expect(tool).toHaveTextContent('list_feeds');
    const row = screen.getByTestId('chat-agent-step');
    expect(row).toHaveAttribute('data-step-status', 'ok');
  });
});

describe('ChatPanel partial-failure banner', () => {
  beforeEach(() => {
    resetStore();
    mockedService.streamMessage.mockReset();
  });

  it('renders the banner with the failed step ids', () => {
    useChatStore.setState({
      messages: [
        {
          role: 'assistant',
          content: 'Answer with gaps.',
          partialFailure: { failedSteps: ['step_2', 'step_4'] },
        },
      ],
    });
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    const banner = screen.getByTestId('chat-partial-failure-banner');
    expect(banner).toHaveAttribute('data-failed-steps', 'step_2,step_4');
    expect(banner).toHaveTextContent('step_2');
    expect(banner).toHaveTextContent('step_4');
    expect(banner).toHaveTextContent(/may be incomplete/);
  });

  it('renders the plan-level variant when no step ids are present', () => {
    useChatStore.setState({
      messages: [
        { role: 'assistant', content: '', partialFailure: { failedSteps: [] } },
      ],
    });
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    const banner = screen.getByTestId('chat-partial-failure-banner');
    expect(banner).toHaveAttribute('data-failed-steps', '');
    expect(banner).toHaveTextContent('Run did not reach a terminal step');
  });

  it('does not render the banner for clean runs', () => {
    useChatStore.setState({
      messages: [{ role: 'assistant', content: 'All good.' }],
    });
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    expect(screen.queryByTestId('chat-partial-failure-banner')).toBeNull();
  });

  it('shows the banner without expanding the agent-steps disclosure', () => {
    useChatStore.setState({
      messages: [
        {
          role: 'assistant',
          content: 'Answer.',
          partialFailure: { failedSteps: ['step_1'] },
          steps: [
            {
              id: 'step_1',
              kind: 'tool',
              status: 'failed',
              reason: 'tool unavailable',
            },
          ],
        },
      ],
    });
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    // The disclosure is collapsed (no running steps) — the banner must
    // still be visible, i.e. it is NOT nested inside the disclosure.
    const disclosure = screen.getByTestId('chat-agent-steps');
    expect(disclosure).not.toHaveAttribute('open');
    expect(screen.getByTestId('chat-partial-failure-banner')).toBeInTheDocument();
  });

  it('renders the retry badge on a step that was retried', () => {
    useChatStore.setState({
      messages: [
        {
          role: 'assistant',
          content: 'Answer.',
          steps: [
            {
              id: 'step_1',
              kind: 'tool',
              status: 'ok',
              toolName: 'list_feeds',
              retryCount: 2,
            },
          ],
        },
      ],
    });
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    expect(screen.getByTestId('chat-agent-step-retry')).toHaveTextContent('↻ 2');
  });

  it('omits the retry badge on first-try success', () => {
    useChatStore.setState({
      messages: [
        {
          role: 'assistant',
          content: 'Answer.',
          steps: [
            {
              id: 'step_1',
              kind: 'tool',
              status: 'ok',
              toolName: 'list_feeds',
              retryCount: 0,
            },
          ],
        },
      ],
    });
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    expect(screen.queryByTestId('chat-agent-step-retry')).toBeNull();
  });
});

// ---- Chat history rail (openspec/changes/agent-chat-history-ui) ----
// The panel hosts <ChatSidebar>: a static left column at `md:` and up,
// an overlay drawer below it. Session rows replay stored messages into
// the same `messages` array the live stream writes to.

describe('ChatPanel chat history', () => {
  beforeEach(() => {
    resetStore();
    mockedService.streamMessage.mockReset();
    mockedService.listSessions
      .mockReset()
      .mockResolvedValue({ sessions: [], nextCursor: null });
    mockedService.getMessages.mockReset().mockResolvedValue([]);
    mockedService.resumeSession.mockReset().mockResolvedValue(undefined);
    mockedService.createSession
      .mockReset()
      .mockResolvedValue({ session_id: 's-new' });
    mockedService.deleteSession.mockReset().mockResolvedValue(undefined);
  });

  it('mounts the rail and the new-chat action', () => {
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    expect(screen.getByTestId('chat-sidebar')).toBeInTheDocument();
    expect(screen.getByTestId('chat-sidebar-toggle')).toBeInTheDocument();
    expect(screen.getByTestId('chat-new-session')).toBeInTheDocument();
  });

  it('starts a new session without dropping the history list', async () => {
    useChatStore.setState({
      sessionList: [makeSession('s1', { title: 'Earlier' })],
      currentSessionId: 's1',
      messages: [{ role: 'user', content: 'old turn' }],
    });
    sessionStorage.setItem('nyx.chat.currentSessionId', 's1');

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await userEvent.click(screen.getByTestId('chat-new-session'));

    await waitFor(() => {
      expect(mockedService.createSession).toHaveBeenCalledTimes(1);
    });
    await waitFor(() => {
      expect(useChatStore.getState().currentSessionId).toBe('s-new');
    });
    // Panel cleared, list untouched (spec Scenario "New chat clears the
    // panel": the rail keeps its rows).
    expect(useChatStore.getState().messages).toEqual([]);
    expect(screen.queryByTestId('chat-message-user')).toBeNull();
    expect(useChatStore.getState().sessionList).toHaveLength(1);
    expect(screen.getByText('Earlier')).toBeInTheDocument();
  });

  it('blanks the panel for the new chat before the POST resolves', async () => {
    let resolveCreate: ((v: { session_id: string }) => void) | null = null;
    mockedService.createSession.mockImplementationOnce(
      () =>
        new Promise<{ session_id: string }>((resolve) => {
          resolveCreate = resolve;
        }),
    );
    useChatStore.setState({
      currentSessionId: 's1',
      messages: [{ role: 'assistant', content: 'old answer' }],
    });

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await userEvent.click(screen.getByTestId('chat-new-session'));

    // Cleared synchronously — the UI guarantee does not depend on the
    // network.
    await waitFor(() => {
      expect(useChatStore.getState().messages).toEqual([]);
    });
    expect(useChatStore.getState().currentSessionId).toBeNull();

    resolveCreate?.({ session_id: 's-new' });
    await waitFor(() => {
      expect(useChatStore.getState().currentSessionId).toBe('s-new');
    });
  });

  it('collapses the drawer when a session is picked below md', async () => {
    useChatStore.setState({ sessionList: [makeSession('s1', { title: 'A chat' })] });

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    // Hidden below md, static from md up — classes only, no JS media
    // query. jsdom can't evaluate Tailwind, so assert the class list.
    const rail = screen.getByTestId('chat-sidebar');
    expect(rail.className).toContain('hidden');
    expect(rail.className).toContain('md:flex');

    await userEvent.click(screen.getByTestId('chat-sidebar-toggle'));
    expect(screen.getByTestId('chat-sidebar-toggle')).toHaveAttribute(
      'aria-expanded',
      'true',
    );
    expect(screen.getByTestId('chat-sidebar').className).toContain('absolute');

    await userEvent.click(screen.getByTestId('chat-session-row'));
    await waitFor(() => {
      expect(screen.getByTestId('chat-sidebar').className).toContain('hidden');
    });
    expect(screen.getByTestId('chat-sidebar-toggle')).toHaveAttribute(
      'aria-expanded',
      'false',
    );
  });

  it('replays a stored session as bubbles with its agent steps', async () => {
    useChatStore.setState({
      sessionList: [makeSession('s1', { title: 'Feeds run' })],
    });
    mockedService.getMessages.mockResolvedValueOnce([
      makeRow(1, 'user', 'which feeds do I have?'),
      makeRow(2, 'assistant', 'Three of them.', [
        { step_id: 'step_1', call_id: 'c1', name: 'list_feeds', args: { limit: 5 } },
      ]),
    ]);

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await userEvent.click(screen.getByTestId('chat-session-row'));

    expect(await screen.findByTestId('chat-message-user')).toHaveTextContent(
      'which feeds do I have?',
    );
    expect(screen.getByTestId('chat-message-assistant')).toHaveTextContent(
      'Three of them.',
    );
    // Stored tool_calls replay through the same "agent steps"
    // disclosure the live stream feeds.
    expect(screen.getByTestId('chat-agent-steps')).toBeInTheDocument();
    expect(screen.getByTestId('chat-agent-step-tool')).toHaveTextContent(
      'list_feeds',
    );
  });

  it('streams a new turn below the replayed history on the active run id', async () => {
    useChatStore.setState({
      sessionList: [makeSession('s1', { title: 'Feeds run' })],
    });
    mockedService.getMessages.mockResolvedValueOnce([
      makeRow(1, 'user', 'earlier question'),
      makeRow(2, 'assistant', 'earlier answer'),
    ]);
    mockedService.streamMessage.mockImplementationOnce(async function* () {
      yield { kind: 'delta', delta: 'follow-up answer' };
      yield { kind: 'terminator' };
    });

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await userEvent.click(screen.getByTestId('chat-session-row'));
    await screen.findByTestId('chat-message-assistant');

    await userEvent.type(screen.getByTestId('chat-input'), 'and now?');
    await userEvent.click(screen.getByTestId('chat-send'));

    await waitFor(() => {
      expect(screen.getAllByTestId('chat-message-assistant')).toHaveLength(2);
    });
    // The resumed history goes out with the request so the agent keeps
    // its context, and the active session is echoed as `run_id`.
    const [sent, , opts] = mockedService.streamMessage.mock.calls[0];
    expect(sent).toEqual([
      { role: 'user', content: 'earlier question' },
      { role: 'assistant', content: 'earlier answer' },
      { role: 'user', content: 'and now?' },
    ]);
    expect(opts.runId).toBe('s1');
  });

  it('restores the tab session from sessionStorage on open', async () => {
    sessionStorage.setItem('nyx.chat.currentSessionId', 's9');
    mockedService.getMessages.mockResolvedValueOnce([
      makeRow(1, 'user', 'restored turn'),
    ]);

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    expect(await screen.findByTestId('chat-message-user')).toHaveTextContent(
      'restored turn',
    );
    expect(mockedService.resumeSession).toHaveBeenCalledWith('s9');
    expect(useChatStore.getState().currentSessionId).toBe('s9');
  });
});
