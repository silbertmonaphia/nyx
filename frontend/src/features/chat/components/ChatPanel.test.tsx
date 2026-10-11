import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ChatPanel } from './ChatPanel';
import { useChatStore } from '../store/chatStore';
import { chatService } from '../services/chatService';

vi.mock('../services/chatService');

const mockedService = chatService as unknown as {
  streamMessage: ReturnType<typeof vi.fn>;
};

function resetStore() {
  useChatStore.setState({ messages: [], isStreaming: false, error: null });
}

describe('ChatPanel', () => {
  beforeEach(() => {
    resetStore();
    mockedService.streamMessage.mockReset();
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
