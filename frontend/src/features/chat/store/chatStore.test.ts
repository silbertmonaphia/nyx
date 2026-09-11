import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act, waitFor } from '@testing-library/react';
import { chatService } from '../services/chatService';
import { useChatStore } from './chatStore';
import type { ChatEvent } from '../types/chat';

// Auto-mock the service so we can yield hand-crafted streams per
// test. Mirrors the pattern in useMovies.test.ts.
vi.mock('../services/chatService');

const mockedService = chatService as unknown as {
  streamMessage: ReturnType<typeof vi.fn>;
};

function resetStore() {
  useChatStore.setState({
    messages: [],
    isStreaming: false,
    error: null,
  });
}

/**
 * Async generator that yields the given events, then waits on a
 * held promise until the test resolves it. This is the same
 * hold-promise-open pattern useMovies.test.ts uses — it lets the
 * test drive `act` while the stream is in flight so we can
 * observe the intermediate state.
 */
async function* makeStream(
  events: ChatEvent[],
  signal: AbortSignal,
): AsyncGenerator<ChatEvent> {
  for (const e of events) {
    if (signal.aborted) return;
    yield e;
  }
  // Hang on a held promise so the store stays in isStreaming=true
  // until the test explicitly releases it. The promise resolves
  // either on abort (Cancel) or when the test calls `release()`.
  await new Promise<void>((resolve) => {
    const onAbort = () => {
      signal.removeEventListener('abort', onAbort);
      resolve();
    };
    signal.addEventListener('abort', onAbort, { once: true });
    if (signal.aborted) {
      onAbort();
      return;
    }
    // Track a release hook so the test can settle the hang
    // without going through the cancel() path (which would slice
    // the partial assistant off and mask what we want to assert).
    const g = globalThis as unknown as { __chatRelease?: () => void };
    g.__chatRelease = () => {
      signal.removeEventListener('abort', onAbort);
      resolve();
    };
  });
}

/** Resolve any held stream so the store's finally block runs. */
function releaseStream() {
  const g = globalThis as unknown as { __chatRelease?: () => void };
  if (g.__chatRelease) {
    g.__chatRelease();
    delete g.__chatRelease;
  }
}

describe('useChatStore', () => {
  beforeEach(() => {
    resetStore();
    mockedService.streamMessage.mockReset();
  });

  describe('send', () => {
    it('pushes a user message and appends deltas to a single assistant row', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'delta', delta: 'Hel' },
              { kind: 'delta', delta: 'lo' },
            ],
            signal,
          ),
      );

      act(() => {
        void useChatStore.getState().send('Hi there');
      });

      // After send lands: a user row + an empty assistant row, and
      // isStreaming is true while the generator is held open.
      await waitFor(() => {
        expect(useChatStore.getState().messages.length).toBe(2);
      });
      expect(useChatStore.getState().messages[0]).toEqual({
        role: 'user',
        content: 'Hi there',
      });
      expect(useChatStore.getState().isStreaming).toBe(true);

      // Drain the generator so the finally block runs and
      // isStreaming flips back to false.
      await act(async () => {
        releaseStream();
      });

      expect(useChatStore.getState().isStreaming).toBe(false);
      // The two deltas were concatenated into a single assistant
      // bubble (not two rows).
      const assistant = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant');
      expect(assistant?.content).toBe('Hello');
    });

    it('sets usage on the assistant row when an event:done arrives', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'delta', delta: 'ok' },
              {
                kind: 'done',
                usage: {
                  prompt_tokens: 4,
                  completion_tokens: 2,
                  total_tokens: 6,
                },
              },
            ],
            signal,
          ),
      );

      await act(async () => {
        useChatStore.getState().send('hi');
      });

      // Both deltas + done fire before the generator hangs on the
      // held promise; release the hang so isStreaming flips without
      // also slicing the assistant row (which cancel would do).
      await act(async () => {
        releaseStream();
      });

      const assistant = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant');
      expect(assistant?.usage).toEqual({
        prompt_tokens: 4,
        completion_tokens: 2,
        total_tokens: 6,
      });
    });

    it('captures a mid-stream error event into store.error', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'delta', delta: 'part' },
              {
                kind: 'error',
                error: 'upstream failed',
                request_id: 'req-x',
              },
            ],
            signal,
          ),
      );

      await act(async () => {
        useChatStore.getState().send('hi');
      });

      await act(async () => {
        releaseStream();
      });

      expect(useChatStore.getState().error).toBe('upstream failed');
    });

    it('captures a thrown service error into store.error', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        () =>
          (async function* () {
            throw new Error('network down');
          })(),
      );

      await act(async () => {
        useChatStore.getState().send('hi');
      });

      expect(useChatStore.getState().error).toBe('network down');
      expect(useChatStore.getState().isStreaming).toBe(false);
    });

    it('refuses to send when isStreaming is true', async () => {
      // Hold the first stream open so a second send would collide.
      mockedService.streamMessage.mockImplementationOnce(
        (messages: unknown, signal: AbortSignal) =>
          makeStream([], signal),
      );

      act(() => {
        void useChatStore.getState().send('first');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      const callsBefore = mockedService.streamMessage.mock.calls.length;
      await act(async () => {
        await useChatStore.getState().send('second');
      });
      expect(mockedService.streamMessage.mock.calls.length).toBe(callsBefore);

      await act(async () => {
        await useChatStore.getState().cancel();
      });
    });

    it('does not send an empty / whitespace-only message', async () => {
      await act(async () => {
        await useChatStore.getState().send('   ');
      });
      expect(mockedService.streamMessage).not.toHaveBeenCalled();
    });
  });

  describe('cancel', () => {
    it('flips isStreaming to false and removes the partial assistant row', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (messages: unknown, signal: AbortSignal) =>
          makeStream([{ kind: 'delta', delta: 'partial' }], signal),
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(useChatStore.getState().messages.length).toBe(2);
      });

      await act(async () => {
        await useChatStore.getState().cancel();
      });

      expect(useChatStore.getState().isStreaming).toBe(false);
      // The half-built assistant bubble is dropped; the user
      // message survives.
      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'hi' },
      ]);
    });

    it('is idempotent (no in-flight stream)', async () => {
      await act(async () => {
        await useChatStore.getState().cancel();
      });
      expect(useChatStore.getState().isStreaming).toBe(false);
    });
  });

  describe('reset', () => {
    it('clears messages, error, and isStreaming', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (messages: unknown, signal: AbortSignal) =>
          makeStream([], signal),
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        await useChatStore.getState().reset();
      });

      const state = useChatStore.getState();
      expect(state.messages).toEqual([]);
      expect(state.isStreaming).toBe(false);
      expect(state.error).toBeNull();
    });
  });
});
