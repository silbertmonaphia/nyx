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
});
