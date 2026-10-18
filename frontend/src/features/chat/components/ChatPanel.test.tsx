import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ChatPanel } from './ChatPanel';
import { useChatStore } from '../store/chatStore';
import { chatService } from '../services/chatService';
import type { SessionMessage, SessionSummary } from '../types/chat';

vi.mock('../services/chatService');

const mockedService = chatService as unknown as {
  streamMessage: ReturnType<typeof vi.fn>;
  submitApproval: ReturnType<typeof vi.fn>;
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
  overrides: Partial<SessionMessage> = {},
): SessionMessage {
  return {
    id,
    role,
    content,
    tool_calls: toolCalls,
    steps: null,
    tokens: 0,
    prompt_tokens: null,
    completion_tokens: null,
    created_at: '2026-10-14T00:00:00Z',
    ...overrides,
  };
}

function resetStore(
  overrides: Partial<ReturnType<typeof useChatStore.getState>> = {},
) {
  // Abort + clear any stream a previous test left live (module-level
  // bookkeeping sits outside the store — see the store's test file).
  useChatStore.getState().clearActiveSession();
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

  it('replays the persisted token usage split', async () => {
    useChatStore.setState({
      sessionList: [makeSession('s1', { title: 'Usage run' })],
    });
    mockedService.getMessages.mockResolvedValueOnce([
      makeRow(1, 'user', 'hi'),
      makeRow(2, 'assistant', 'hello', null, {
        tokens: 9,
        prompt_tokens: 7,
        completion_tokens: 2,
      }),
    ]);

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await userEvent.click(screen.getByTestId('chat-session-row'));

    expect(await screen.findByTestId('chat-message-assistant')).toHaveTextContent(
      /tokens: 9 \( prompt 7 · completion 2\)/,
    );
  });

  it('replays a pre-split row with its total only', async () => {
    useChatStore.setState({
      sessionList: [makeSession('s1', { title: 'Legacy run' })],
    });
    mockedService.getMessages.mockResolvedValueOnce([
      makeRow(1, 'user', 'hi'),
      makeRow(2, 'assistant', 'hello', null, { tokens: 42 }),
    ]);

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await userEvent.click(screen.getByTestId('chat-session-row'));

    const bubble = await screen.findByTestId('chat-message-assistant');
    expect(bubble).toHaveTextContent(/tokens: 42/);
    expect(bubble).not.toHaveTextContent(/prompt/);
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

// ---- Approval cards (openspec change agent-feed-tools §6.2) ----
// A `confirm_request` frame renders an inline card inside the assistant
// turn. Approve/Deny post the decision through chatService (the axios
// client stamps the JWT); the stream stays paused until the decision
// lands. A denial then arrives as the call's `tool_result` and renders
// through the regular agent-step result path.

describe('ChatPanel approval card', () => {
  beforeEach(() => {
    resetStore();
    mockedService.streamMessage.mockReset();
    mockedService.submitApproval.mockReset().mockResolvedValue(undefined);
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

  /**
   * Stream that pauses on a `confirm_request` until the returned
   * release hook fires, then continues with the denied call's frames.
   * Mirrors the held-stream shape the real agent produces (design.md
   * D2: one SSE response, decision brokered out-of-band).
   */
  function mockPausedStream(
    expiresAtSeconds: number = Date.now() / 1000 + 300,
  ): () => void {
    let release: (() => void) | null = null;
    mockedService.streamMessage.mockImplementationOnce(async function* () {
      yield {
        kind: 'plan',
        steps: [{ id: 'step_2', kind: 'tool', tool: 'create_feed' }],
      };
      yield { kind: 'step_start', step_id: 'step_2', step_kind: 'tool' };
      yield {
        kind: 'confirm_request',
        run_id: 'run-1',
        step_id: 'step_2',
        tool: 'create_feed',
        args: { title: 'Rust Blog' },
        approval_id: 'ap-1',
        expires_at: expiresAtSeconds,
      };
      await new Promise<void>((resolve) => {
        release = resolve;
      });
      yield {
        kind: 'tool_result',
        step_id: 'step_2',
        call_id: 'c1',
        ok: false,
        // The producer's static denial text (approvals.py
        // `denial_text_for`), byte-for-byte.
        error:
          'Confirmation required: the user denied this call. The `create_feed` tool was not executed.',
      };
      yield {
        kind: 'step_end',
        step_id: 'step_2',
        status: 'failed',
        reason: 'tool_denied',
      };
      yield { kind: 'delta', delta: 'OK — I did not create it.' };
      yield {
        kind: 'done',
        usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
      };
      yield { kind: 'terminator' };
    });
    return () => release?.();
  }

  async function sendMessage() {
    await userEvent.type(screen.getByTestId('chat-input'), 'add the rust blog');
    await userEvent.click(screen.getByTestId('chat-send'));
  }

  it('renders the inline card with the tool and args inside the assistant turn', async () => {
    const release = mockPausedStream();
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await sendMessage();

    const card = await screen.findByTestId('chat-confirm-card');
    expect(card).toHaveAttribute('data-decision', 'pending');
    expect(card).toHaveAttribute('data-approval-id', 'ap-1');
    expect(screen.getByTestId('chat-confirm-tool')).toHaveTextContent(
      'create_feed',
    );
    expect(screen.getByTestId('chat-confirm-args')).toHaveTextContent(
      'Rust Blog',
    );
    // At the bubble level, not inside the agent-steps disclosure: a
    // paused run must be actionable without expanding anything.
    expect(screen.getByTestId('chat-message-assistant')).toContainElement(
      card,
    );
    release();
  });

  it('posts the approve decision body and flips the card to decided', async () => {
    const release = mockPausedStream();
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await sendMessage();

    await userEvent.click(await screen.findByTestId('chat-confirm-approve'));

    expect(mockedService.submitApproval).toHaveBeenCalledTimes(1);
    expect(mockedService.submitApproval).toHaveBeenCalledWith({
      run_id: 'run-1',
      approval_id: 'ap-1',
      decision: 'approve',
    });
    await waitFor(() => {
      expect(screen.getByTestId('chat-confirm-card')).toHaveAttribute(
        'data-decision',
        'approved',
      );
    });
    expect(screen.queryByTestId('chat-confirm-approve')).toBeNull();
    release();
  });

  it('posts the deny decision body and renders the denial result in the turn', async () => {
    const release = mockPausedStream();
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await sendMessage();

    await userEvent.click(await screen.findByTestId('chat-confirm-deny'));

    expect(mockedService.submitApproval).toHaveBeenCalledTimes(1);
    expect(mockedService.submitApproval).toHaveBeenCalledWith({
      run_id: 'run-1',
      approval_id: 'ap-1',
      decision: 'deny',
    });

    // The agent continues on the denial: the paused call closes with a
    // result frame the existing step-row renderer shows in-line.
    release();
    await waitFor(() => {
      expect(screen.getByTestId('chat-confirm-card')).toHaveAttribute(
        'data-decision',
        'denied',
      );
    });
    expect(await screen.findByTestId('chat-agent-step-result')).toHaveTextContent(
      'Confirmation required: the user denied this call',
    );
    expect(screen.getByTestId('chat-message-assistant')).toHaveTextContent(
      'OK — I did not create it.',
    );
  });

  it('keeps the buttons live and shows a hint when the decision POST fails', async () => {
    mockedService.submitApproval.mockRejectedValueOnce(new Error('boom'));
    const release = mockPausedStream();
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await sendMessage();

    await userEvent.click(await screen.findByTestId('chat-confirm-approve'));

    await waitFor(() => {
      expect(screen.getByTestId('chat-confirm-error')).toHaveTextContent(
        /could not send/i,
      );
    });
    expect(screen.getByTestId('chat-confirm-card')).toHaveAttribute(
      'data-decision',
      'pending',
    );
    expect(screen.getByTestId('chat-confirm-approve')).toBeInTheDocument();
    release();
  });

  it('resolves the card as expired when the decision POST 404s', async () => {
    // Design.md D2: a resumed run replays its checkpointed
    // confirm_request, whose approval id the decision endpoint
    // answers with a typed 404 — the card resolves as expired, with
    // no retry affordance (a retry could never succeed).
    mockedService.submitApproval.mockRejectedValueOnce({
      response: { status: 404 },
    });
    const release = mockPausedStream();
    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    await sendMessage();

    await userEvent.click(await screen.findByTestId('chat-confirm-approve'));

    await waitFor(() => {
      expect(screen.getByTestId('chat-confirm-card')).toHaveAttribute(
        'data-decision',
        'expired',
      );
    });
    expect(screen.getByTestId('chat-confirm-status')).toHaveTextContent(
      /expired/i,
    );
    expect(screen.queryByTestId('chat-confirm-approve')).toBeNull();
    expect(screen.queryByTestId('chat-confirm-deny')).toBeNull();
    expect(screen.queryByTestId('chat-confirm-error')).toBeNull();
    release();
  });

  it('expires the card at its deadline without posting a decision', async () => {
    vi.useFakeTimers();
    const T0 = new Date('2026-10-15T00:00:00Z').getTime();
    vi.setSystemTime(new Date(T0));
    try {
      const release = mockPausedStream(T0 / 1000 + 2);
      render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
      await act(async () => {
        void useChatStore.getState().send('add the rust blog');
      });

      expect(screen.getByTestId('chat-confirm-card')).toHaveAttribute(
        'data-decision',
        'pending',
      );
      expect(
        screen.getByTestId('chat-confirm-countdown'),
      ).toHaveTextContent('Expires in 0:02');

      act(() => {
        vi.advanceTimersByTime(3_000);
      });

      expect(screen.getByTestId('chat-confirm-card')).toHaveAttribute(
        'data-decision',
        'expired',
      );
      expect(screen.getByTestId('chat-confirm-status')).toHaveTextContent(
        /expired/i,
      );
      expect(screen.queryByTestId('chat-confirm-approve')).toBeNull();
      expect(screen.queryByTestId('chat-confirm-deny')).toBeNull();
      expect(mockedService.submitApproval).not.toHaveBeenCalled();
      release();
    } finally {
      vi.useRealTimers();
    }
  });

  it('locks the composer with a hint while the active session has a live detached run', async () => {
    // The panel is watching the session a background run is still
    // streaming into: sending would interleave a second run's rows
    // into the same conversation (chatStore design D4).
    resetStore({
      currentSessionId: 'sess-busy',
      runningSessionIds: ['sess-busy'],
    });

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    expect(screen.getByTestId('chat-input')).toBeDisabled();
    expect(screen.getByTestId('chat-send')).toBeDisabled();
    expect(
      screen.getByTestId('chat-session-busy-hint'),
    ).toHaveTextContent(/still working/i);
    // Guarded by lock, not by a live stream of this panel's own — no
    // Cancel affordance appears (the detached run has no abort handle).
    expect(screen.queryByTestId('chat-cancel')).toBeNull();
  });

  it('re-enables the composer when the background run finishes', async () => {
    resetStore({
      currentSessionId: 'sess-busy',
      runningSessionIds: ['sess-busy'],
    });

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);
    expect(screen.getByTestId('chat-input')).toBeDisabled();

    // The run reaches its terminal frame: the store clears the marker
    // (and re-replays the session, which the mocked service answers
    // empty).
    useChatStore.setState({ runningSessionIds: [] });
    await waitFor(() => {
      expect(screen.getByTestId('chat-input')).not.toBeDisabled();
    });
    expect(screen.queryByTestId('chat-session-busy-hint')).toBeNull();
  });

  it('leaves the composer unlocked while the panel is on a different session', async () => {
    // A background run in ANOTHER session must not lock this panel —
    // that is the point of detaching: keep chatting elsewhere.
    resetStore({
      currentSessionId: 'sess-other',
      runningSessionIds: ['sess-busy'],
    });

    render(<ChatPanel open={true} onOpenChange={vi.fn()} />);

    expect(screen.getByTestId('chat-input')).not.toBeDisabled();
    expect(screen.queryByTestId('chat-session-busy-hint')).toBeNull();
  });
});
