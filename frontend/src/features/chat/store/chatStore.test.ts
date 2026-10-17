import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act, waitFor } from '@testing-library/react';
import { chatService } from '../services/chatService';
import { useChatStore } from './chatStore';
import { ChatApiError, type ChatEvent, type SessionMessage } from '../types/chat';

// Auto-mock the service so we can yield hand-crafted streams per
// test. Mirrors the pattern in useFeeds.test.ts.
vi.mock('../services/chatService');

const mockedService = chatService as unknown as {
  streamMessage: ReturnType<typeof vi.fn>;
  submitApproval: ReturnType<typeof vi.fn>;
  listSessions: ReturnType<typeof vi.fn>;
  getMessages: ReturnType<typeof vi.fn>;
  createSession: ReturnType<typeof vi.fn>;
  resumeSession: ReturnType<typeof vi.fn>;
  deleteSession: ReturnType<typeof vi.fn>;
};

// sessionStorage key the store persists the active session id under.
const CURRENT_SESSION_KEY = 'nyx.chat.currentSessionId';

function resetStore() {
  // Abort + clear any stream a previous test left live FIRST: the
  // module-level bookkeeping (attachedToken / controller / detachHook /
  // streamSeq) lives outside the store, so a test that ends mid-stream
  // would otherwise leak a registered detach hook into the next test's
  // navigation actions (injecting a phantom rail marker).
  useChatStore.getState().clearActiveSession();
  useChatStore.setState({
    messages: [],
    isStreaming: false,
    error: null,
    currentSessionId: null,
    ownerUserId: undefined,
    sessionList: [],
    sessionListNextCursor: null,
    isFetchingMore: false,
    runningSessionIds: [],
  });
  // Clear any persisted session id from a previous test.
  localStorage.clear();
  sessionStorage.clear();
}

/**
 * Reset the auto-mocked service and give the session endpoints benign
 * defaults. Tests override per-case with `mockResolvedValueOnce`.
 */
function resetService() {
  mockedService.streamMessage.mockReset();
  mockedService.submitApproval.mockReset().mockResolvedValue(undefined);
  mockedService.listSessions
    .mockReset()
    .mockResolvedValue({ sessions: [], nextCursor: null });
  mockedService.getMessages.mockReset().mockResolvedValue([]);
  mockedService.createSession
    .mockReset()
    .mockResolvedValue({ session_id: 'srv-new', started_at: '2026-10-14T00:00:00Z' });
  mockedService.resumeSession.mockReset().mockResolvedValue(undefined);
  mockedService.deleteSession.mockReset().mockResolvedValue(undefined);
}

function makeSession(id: string, lastActiveAt = '2026-10-14T00:00:00Z') {
  return {
    id,
    user_id: 1,
    title: '',
    started_at: '2026-10-14T00:00:00Z',
    last_active_at: lastActiveAt,
    ended_at: null,
    message_count: 2,
  };
}

function makeMessageRow(overrides: Partial<SessionMessage> = {}): SessionMessage {
  return {
    id: 1,
    role: 'user',
    content: 'hello',
    tool_calls: null,
    tokens: 0,
    created_at: '2026-10-14T00:00:00Z',
    ...overrides,
  };
}

/**
 * Async generator that yields the given events, then waits on a
 * held promise until the test resolves it. This is the same
 * hold-promise-open pattern useFeeds.test.ts uses — it lets the
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
    resetService();
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

    it('attaches a partial-failure signal to the assistant row', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'delta', delta: 'partial ' },
              { kind: 'partial_failure', failedSteps: ['step_2'] },
              { kind: 'delta', delta: 'answer' },
            ],
            signal,
          ),
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });

      await waitFor(() => {
        const assistant = useChatStore
          .getState()
          .messages.find((m) => m.role === 'assistant');
        expect(assistant?.partialFailure).toEqual({ failedSteps: ['step_2'] });
      });

      await act(async () => {
        releaseStream();
      });

      // Additive: the signal rides alongside the answer, it does not
      // replace or truncate the streamed content.
      const assistant = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant');
      expect(assistant?.content).toBe('partial answer');
      expect(assistant?.partialFailure).toEqual({ failedSteps: ['step_2'] });
    });

    it('records retryCount from tool_result frames on the step row', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'step_start', step_id: 'step_1', step_kind: 'tool' },
              {
                kind: 'tool_call',
                step_id: 'step_1',
                call_id: 'c1',
                name: 'list_feeds',
                args: {},
              },
              {
                kind: 'tool_result',
                step_id: 'step_1',
                call_id: 'c1',
                ok: true,
                result: { items: [] },
                retryCount: 1,
              },
            ],
            signal,
          ),
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });

      await waitFor(() => {
        const assistant = useChatStore
          .getState()
          .messages.find((m) => m.role === 'assistant');
        expect(assistant?.steps?.[0]?.retryCount).toBe(1);
      });

      await act(async () => {
        releaseStream();
      });

      const assistant = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant');
      expect(assistant?.steps?.[0]?.toolResult).toEqual({ items: [] });
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

    it('appends a mid-stream note event to the assistant row without truncating', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'delta', delta: 'part' },
              { kind: 'note', text: '[continued on OpenAI]', provider: 'openai' },
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

      // Release the held stream so the store's finally block runs
      // and isStreaming flips back to false. The note was appended
      // mid-flight, before this release.
      await act(async () => {
        releaseStream();
      });

      const messages = useChatStore.getState().messages;
      const assistant = messages.find((m) => m.role === 'assistant');
      expect(assistant?.notes).toEqual(['[continued on OpenAI]']);
      // The delta landed before the note — content is preserved.
      expect(assistant?.content).toBe('part');
      // Usage still got recorded when done fired after the note.
      expect(assistant?.usage).toEqual({
        prompt_tokens: 4,
        completion_tokens: 2,
        total_tokens: 6,
      });
      // No truncation: the messages array still holds user + assistant.
      expect(messages.map((m) => m.role)).toEqual(['user', 'assistant']);
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

    it('drops the empty assistant row on a mid-stream error before any delta', async () => {
      // A `error` event with no preceding `delta` must not leave a
      // stranded empty bubble in the conversation — that history
      // would be sent to the planner on the next message and
      // re-planned as if the assistant had said nothing.
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
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
      // The user message survives; the empty assistant row is gone.
      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'hi' },
      ]);
    });

    it('keeps the partial assistant row on a mid-stream error after a delta', async () => {
      // Partial content was already streamed; the user saw it.
      // Dropping it would silently lose visible state. The error
      // toast explains the truncation; the row stays.
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'delta', delta: 'partial answer ' },
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

      const messages = useChatStore.getState().messages;
      expect(useChatStore.getState().error).toBe('upstream failed');
      expect(messages.map((m) => m.role)).toEqual(['user', 'assistant']);
      expect(
        messages.find((m) => m.role === 'assistant')?.content,
      ).toBe('partial answer ');
    });

    it('drops the empty assistant row when the service throws before any delta', async () => {
      // Thrown-error path (e.g. fetch rejection) follows the same
      // empty-vs-partial rule as the mid-stream error event.
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
      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'hi' },
      ]);
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

  describe('send options and session adoption', () => {
    it('does not send a rag field on the stream request', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              {
                kind: 'done',
                usage: {
                  prompt_tokens: 1,
                  completion_tokens: 1,
                  total_tokens: 2,
                },
              },
            ],
            signal,
          ),
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });

      await waitFor(() => {
        expect(mockedService.streamMessage).toHaveBeenCalled();
      });
      const call = mockedService.streamMessage.mock.calls[0];
      expect(call[2]).not.toHaveProperty('rag');
      // No active session in this suite's reset state → no run_id echo.
      expect(call[2].runId).toBeNull();

      await act(async () => {
        releaseStream();
      });
    });

    it('echoes the active session id as runId', async () => {
      useChatStore.setState({ currentSessionId: 'sess-active' });
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              {
                kind: 'done',
                usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
              },
            ],
            signal,
          ),
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });

      await waitFor(() => {
        expect(mockedService.streamMessage).toHaveBeenCalled();
      });
      expect(mockedService.streamMessage.mock.calls[0][2].runId).toBe(
        'sess-active',
      );

      await act(async () => {
        releaseStream();
      });
    });

    it('adopts the run_id frame as the active session and persists it', async () => {
      // The server mints the id on the stream's first frame; the store
      // captures it so a reload can find the conversation — and pulls
      // page 1 so the new session appears in the rail immediately.
      mockedService.listSessions.mockResolvedValueOnce({
        sessions: [
          {
            id: 'srv-run-1',
            user_id: 1,
            title: '',
            started_at: '2026-10-14T00:00:00Z',
            last_active_at: '2026-10-14T00:00:00Z',
            ended_at: null,
            message_count: 1,
          },
        ],
        nextCursor: null,
      });
      mockedService.streamMessage.mockImplementationOnce(
        (
          _messages: unknown,
          signal: AbortSignal,
          opts: { onRunId?: (id: string) => void },
        ) => {
          opts.onRunId?.('srv-run-1');
          return makeStream([{ kind: 'delta', delta: 'hi' }], signal);
        },
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });

      await waitFor(() => {
        expect(useChatStore.getState().currentSessionId).toBe('srv-run-1');
      });
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBe('srv-run-1');

      // The rail refresh lands the adopted session as the newest row.
      await waitFor(() => {
        expect(mockedService.listSessions).toHaveBeenCalled();
      });
      await waitFor(() => {
        expect(useChatStore.getState().sessionList[0]?.id).toBe('srv-run-1');
      });

      await act(async () => {
        releaseStream();
      });
    });

    it('adopts a server id that differs from the one the request carried (review M2)', async () => {
      // The server mints fresh when the echoed id was unknown /
      // soft-deleted / hit a store hiccup — the panel must follow the
      // session the server actually wrote to, not keep a stale id.
      useChatStore.setState({ currentSessionId: 'sess-stale' });
      mockedService.streamMessage.mockImplementationOnce(
        (
          _messages: unknown,
          signal: AbortSignal,
          opts: { onRunId?: (id: string) => void },
        ) => {
          opts.onRunId?.('srv-minted');
          return makeStream([{ kind: 'delta', delta: 'hi' }], signal);
        },
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });

      await waitFor(() => {
        expect(useChatStore.getState().currentSessionId).toBe('srv-minted');
      });
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBe('srv-minted');

      await act(async () => {
        releaseStream();
      });
    });

    it('keeps the id when the frame echoes the id the request carried', async () => {
      // Normal continuation: the echoed id round-trips and there is
      // nothing to adopt.
      useChatStore.setState({ currentSessionId: 'sess-picked' });
      mockedService.streamMessage.mockImplementationOnce(
        (
          _messages: unknown,
          signal: AbortSignal,
          opts: { onRunId?: (id: string) => void },
        ) => {
          opts.onRunId?.('sess-picked');
          return makeStream([{ kind: 'delta', delta: 'hi' }], signal);
        },
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });

      await waitFor(() => {
        expect(mockedService.streamMessage).toHaveBeenCalled();
      });
      await act(async () => {
        releaseStream();
      });

      expect(useChatStore.getState().currentSessionId).toBe('sess-picked');
    });

    it('does not adopt when the user switched sessions mid-flight', async () => {
      // The user's explicit pick wins over the server frame for a
      // request sent under a different id.
      useChatStore.setState({ currentSessionId: 'sess-sent' });
      let fireRunId: ((id: string) => void) | undefined;
      mockedService.streamMessage.mockImplementationOnce(
        (
          _messages: unknown,
          signal: AbortSignal,
          opts: { onRunId?: (id: string) => void },
        ) => {
          fireRunId = opts.onRunId;
          return makeStream([{ kind: 'delta', delta: 'hi' }], signal);
        },
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(mockedService.streamMessage).toHaveBeenCalled();
      });

      act(() => {
        useChatStore.setState({ currentSessionId: 'user-picked' });
        fireRunId?.('srv-other');
      });

      expect(useChatStore.getState().currentSessionId).toBe('user-picked');

      await act(async () => {
        releaseStream();
      });
    });
  });

  // ---- Agent-loop step trace (openspec/changes/agent-loop) ----
  // Steps live on the assistant row's `steps` array, populated
  // incrementally as the SSE stream emits plan / step_start /
  // tool_call / tool_result / step_end frames. The trace is
  // ephemeral — never persisted to localStorage.

  describe('agent steps', () => {
    it('seeds the assistant row with steps from a plan event', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              {
                kind: 'plan',
                steps: [
                  { id: 's1', kind: 'rag' },
                  { id: 's2', kind: 'tool', tool: 'list_feeds', args: { limit: 10 } },
                ],
              },
              {
                kind: 'done',
                usage: {
                  prompt_tokens: 1,
                  completion_tokens: 1,
                  total_tokens: 2,
                },
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

      const assistant = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant');
      expect(assistant?.steps?.map((s) => ({ id: s.id, kind: s.kind, status: s.status, toolName: s.toolName }))).toEqual([
        { id: 's1', kind: 'rag', status: 'running', toolName: undefined },
        { id: 's2', kind: 'tool', status: 'running', toolName: 'list_feeds' },
      ]);
    });

    it('updates steps on step_start / tool_call / tool_result / step_end', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'plan', steps: [{ id: 's1', kind: 'tool', tool: 'list_feeds' }] },
              { kind: 'step_start', step_id: 's1', step_kind: 'tool' },
              {
                kind: 'tool_call',
                step_id: 's1',
                call_id: 'c1',
                name: 'list_feeds',
                args: { limit: 10 },
              },
              {
                kind: 'tool_result',
                step_id: 's1',
                call_id: 'c1',
                ok: true,
                result: [{ id: 1 }],
              },
              { kind: 'step_end', step_id: 's1', status: 'ok', reason: '' },
              {
                kind: 'done',
                usage: {
                  prompt_tokens: 1,
                  completion_tokens: 1,
                  total_tokens: 2,
                },
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

      const assistant = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant');
      expect(assistant?.steps?.[0]).toEqual({
        id: 's1',
        kind: 'tool',
        status: 'ok',
        toolName: 'list_feeds',
        toolArgs: { limit: 10 },
        toolResult: [{ id: 1 }],
      });
    });

    it('appends a step row when step_start arrives without a preceding plan', async () => {
      // Defensive fallback: a server that forgets to emit `plan`
      // first still gets a row per step so the UI has something to
      // render.
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'step_start', step_id: 's9', step_kind: 'rag' },
              { kind: 'step_end', step_id: 's9', status: 'ok', reason: '' },
              {
                kind: 'done',
                usage: {
                  prompt_tokens: 1,
                  completion_tokens: 1,
                  total_tokens: 2,
                },
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

      const assistant = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant');
      expect(assistant?.steps?.[0]?.id).toBe('s9');
      expect(assistant?.steps?.[0]?.status).toBe('ok');
    });

    it('records tool_call_failed on a failed tool_result', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'step_start', step_id: 's1', step_kind: 'tool' },
              {
                kind: 'tool_call',
                step_id: 's1',
                call_id: 'c1',
                name: 'list_feeds',
                args: {},
              },
              {
                kind: 'tool_result',
                step_id: 's1',
                call_id: 'c1',
                ok: false,
                error: 'tool_call_failed',
              },
              {
                kind: 'step_end',
                step_id: 's1',
                status: 'failed',
                reason: 'tool_error',
              },
              {
                kind: 'done',
                usage: {
                  prompt_tokens: 1,
                  completion_tokens: 1,
                  total_tokens: 2,
                },
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

      const assistant = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant');
      expect(assistant?.steps?.[0]).toMatchObject({
        id: 's1',
        status: 'failed',
        reason: 'tool_error',
        toolResult: 'tool_call_failed',
      });
    });

    it('keeps steps ephemeral — localStorage stays empty after a run', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'plan', steps: [{ id: 's1', kind: 'rag' }] },
              { kind: 'step_start', step_id: 's1', step_kind: 'rag' },
              { kind: 'step_end', step_id: 's1', status: 'ok', reason: '' },
              {
                kind: 'done',
                usage: {
                  prompt_tokens: 1,
                  completion_tokens: 1,
                  total_tokens: 2,
                },
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

      // The store keeps nothing in localStorage; the agent step
      // trace in particular never lands there.
      const keys = Object.keys(localStorage);
      const hasStepsKey = keys.some((k) => /steps|agent|chat\.history/i.test(k));
      expect(hasStepsKey).toBe(false);
    });
  });

  // ---- Session history (openspec/changes/agent-chat-history-ui) ----
  // The sidebar's data plane: paginated list, click-to-replay, the
  // sessionStorage restore path, and the quiet-failure contracts.

  describe('loadMore', () => {
    it('fetches the first page and appends the next one', async () => {
      mockedService.listSessions
        .mockResolvedValueOnce({
          sessions: [makeSession('s3'), makeSession('s2')],
          nextCursor: 'cursor-1',
        })
        .mockResolvedValueOnce({
          sessions: [makeSession('s1')],
          nextCursor: null,
        });

      await act(async () => {
        await useChatStore.getState().loadMore();
      });
      expect(useChatStore.getState().sessionList.map((s) => s.id)).toEqual([
        's3',
        's2',
      ]);
      expect(useChatStore.getState().sessionListNextCursor).toBe('cursor-1');
      // First page: no cursor to hand back.
      expect(mockedService.listSessions).toHaveBeenLastCalledWith({
        before: null,
      });

      await act(async () => {
        await useChatStore.getState().loadMore();
      });
      expect(useChatStore.getState().sessionList.map((s) => s.id)).toEqual([
        's3',
        's2',
        's1',
      ]);
      expect(useChatStore.getState().sessionListNextCursor).toBeNull();
      expect(mockedService.listSessions).toHaveBeenLastCalledWith({
        before: 'cursor-1',
      });
    });

    it('is single-flight: the second concurrent call is dropped', async () => {
      let resolvePage!: (page: unknown) => void;
      mockedService.listSessions.mockReturnValueOnce(
        new Promise((resolve) => {
          resolvePage = resolve;
        }),
      );

      let first!: Promise<void>;
      let second!: Promise<void>;
      act(() => {
        first = useChatStore.getState().loadMore();
        second = useChatStore.getState().loadMore();
      });

      expect(mockedService.listSessions).toHaveBeenCalledTimes(1);

      await act(async () => {
        resolvePage({ sessions: [makeSession('s1')], nextCursor: null });
        await Promise.all([first, second]);
      });

      // Exactly one page appended — no duplicated rows.
      expect(useChatStore.getState().sessionList.map((s) => s.id)).toEqual(['s1']);
      expect(useChatStore.getState().isFetchingMore).toBe(false);
    });

    it('does not fetch once the list is complete (next_cursor null)', async () => {
      useChatStore.setState({
        sessionList: [makeSession('s1')],
        sessionListNextCursor: null,
      });

      await act(async () => {
        await useChatStore.getState().loadMore();
      });

      expect(mockedService.listSessions).not.toHaveBeenCalled();
    });

    it('fails quietly: no throw, no state change, isFetchingMore resets', async () => {
      mockedService.listSessions.mockRejectedValueOnce(new Error('network down'));

      await act(async () => {
        await useChatStore.getState().loadMore();
      });

      const state = useChatStore.getState();
      expect(state.sessionList).toEqual([]);
      expect(state.isFetchingMore).toBe(false);
      expect(state.error).toBeNull();
    });
  });

  describe('setCurrentSession', () => {
    it('clears the panel, resumes the session, and replays its messages', async () => {
      useChatStore.setState({
        currentSessionId: 'prev',
        messages: [{ role: 'user', content: 'stale' }],
      });
      mockedService.getMessages.mockResolvedValueOnce([
        makeMessageRow({ id: 1, role: 'user', content: 'hello' }),
        makeMessageRow({
          id: 2,
          role: 'assistant',
          content: 'hi there',
          tool_calls: [
            { step_id: 's1', call_id: 'c1', name: 'list_feeds', args: { limit: 5 } },
          ],
          tokens: 9,
        }),
      ]);

      await act(async () => {
        await useChatStore.getState().setCurrentSession('sess-1');
      });

      expect(mockedService.resumeSession).toHaveBeenCalledWith('sess-1');
      expect(mockedService.getMessages).toHaveBeenCalledWith('sess-1');
      const state = useChatStore.getState();
      expect(state.currentSessionId).toBe('sess-1');
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBe('sess-1');
      // Stored `tool_calls` rehydrate as the same agent-step shape the
      // live stream renders.
      expect(state.messages).toEqual([
        { role: 'user', content: 'hello' },
        {
          role: 'assistant',
          content: 'hi there',
          steps: [
            {
              id: 's1',
              kind: 'tool',
              status: 'ok',
              toolName: 'list_feeds',
              toolArgs: { limit: 5 },
            },
          ],
        },
      ]);
    });

    it('is a no-op when the session is already active', async () => {
      useChatStore.setState({
        currentSessionId: 'sess-1',
        messages: [{ role: 'user', content: 'kept' }],
      });

      await act(async () => {
        await useChatStore.getState().setCurrentSession('sess-1');
      });

      expect(mockedService.resumeSession).not.toHaveBeenCalled();
      expect(mockedService.getMessages).not.toHaveBeenCalled();
      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'kept' },
      ]);
    });

    it('clears the active session quietly when the session is gone (404)', async () => {
      const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
      mockedService.resumeSession.mockRejectedValueOnce(
        new ChatApiError(404, 'Chat request failed (404)'),
      );

      await act(async () => {
        await useChatStore.getState().setCurrentSession('deleted-session');
      });

      const state = useChatStore.getState();
      expect(state.currentSessionId).toBeNull();
      expect(state.messages).toEqual([]);
      expect(state.error).toBeNull();
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBeNull();
      // Spec: no error toast and no console error for the deleted case.
      expect(consoleError).not.toHaveBeenCalled();
      consoleError.mockRestore();
    });

    it('replays a mutating tool call without restoring an approval card', async () => {
      useChatStore.setState({
        currentSessionId: 'prev',
        messages: [{ role: 'user', content: 'stale' }],
      });
      mockedService.getMessages.mockResolvedValueOnce([
        makeMessageRow({
          id: 7,
          role: 'assistant',
          content: 'created it',
          tool_calls: [
            {
              step_id: 's1',
              call_id: 'c1',
              name: 'create_feed',
              args: { title: 'Rust Blog' },
            },
          ],
          tokens: 4,
        }),
      ]);

      await act(async () => {
        await useChatStore.getState().setCurrentSession('sess-r');
      });

      const steps = useChatStore.getState().messages[0]?.steps ?? [];
      expect(steps).toHaveLength(1);
      expect(steps[0]?.confirm).toBeUndefined();
    });

    it('clears the active session when replay 404s after a successful resume', async () => {
      mockedService.getMessages.mockRejectedValueOnce(
        new ChatApiError(404, 'Chat request failed (404)'),
      );

      await act(async () => {
        await useChatStore.getState().setCurrentSession('deleted-session');
      });

      expect(useChatStore.getState().currentSessionId).toBeNull();
      expect(useChatStore.getState().messages).toEqual([]);
    });

    it('keeps the active session when replay fails transiently', async () => {
      mockedService.getMessages.mockRejectedValueOnce(new Error('network down'));

      await act(async () => {
        await useChatStore.getState().setCurrentSession('sess-1');
      });

      // Not a 404: the id stays (a retry may succeed), the panel is
      // simply empty.
      expect(useChatStore.getState().currentSessionId).toBe('sess-1');
      expect(useChatStore.getState().messages).toEqual([]);
    });
  });

  describe('hydrateSession', () => {
    it('restores the persisted session id and its messages', async () => {
      sessionStorage.setItem(CURRENT_SESSION_KEY, 'sess-restored');
      mockedService.getMessages.mockResolvedValueOnce([
        makeMessageRow({ id: 1, role: 'user', content: 'earlier' }),
      ]);

      await act(async () => {
        await useChatStore.getState().hydrateSession();
      });

      expect(useChatStore.getState().currentSessionId).toBe('sess-restored');
      expect(mockedService.getMessages).toHaveBeenCalledWith('sess-restored');
      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'earlier' },
      ]);
    });

    it('does nothing when no session is persisted', async () => {
      await act(async () => {
        await useChatStore.getState().hydrateSession();
      });

      expect(mockedService.getMessages).not.toHaveBeenCalled();
      expect(useChatStore.getState().currentSessionId).toBeNull();
    });

    it('does not refetch when messages are already on screen', async () => {
      sessionStorage.setItem(CURRENT_SESSION_KEY, 'sess-live');
      useChatStore.setState({
        currentSessionId: 'sess-live',
        messages: [{ role: 'user', content: 'already here' }],
      });

      await act(async () => {
        await useChatStore.getState().hydrateSession();
      });

      expect(mockedService.getMessages).not.toHaveBeenCalled();
    });

    it('clears the persisted id quietly when the session was deleted', async () => {
      const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
      sessionStorage.setItem(CURRENT_SESSION_KEY, 'sess-gone');
      mockedService.getMessages.mockRejectedValueOnce(
        new ChatApiError(404, 'Chat request failed (404)'),
      );

      await act(async () => {
        await useChatStore.getState().hydrateSession();
      });

      const state = useChatStore.getState();
      expect(state.currentSessionId).toBeNull();
      expect(state.messages).toEqual([]);
      expect(state.error).toBeNull();
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBeNull();
      expect(consoleError).not.toHaveBeenCalled();
      consoleError.mockRestore();
    });
  });

  describe('startNewSession', () => {
    it('clears the panel and makes the minted session active', async () => {
      useChatStore.setState({
        currentSessionId: 'prev',
        messages: [{ role: 'user', content: 'stale' }],
      });
      mockedService.createSession.mockResolvedValueOnce({
        session_id: 'srv-fresh',
        started_at: '2026-10-14T00:00:00Z',
      });

      await act(async () => {
        await useChatStore.getState().startNewSession();
      });

      const state = useChatStore.getState();
      expect(state.messages).toEqual([]);
      expect(state.currentSessionId).toBe('srv-fresh');
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBe('srv-fresh');
      // The sidebar list is untouched — no scroll, no clear.
      expect(state.sessionList).toEqual([]);
    });

    it('leaves the tab session-less when the create call fails', async () => {
      useChatStore.setState({
        currentSessionId: 'prev',
        messages: [{ role: 'user', content: 'stale' }],
      });
      mockedService.createSession.mockRejectedValueOnce(new Error('network down'));

      await act(async () => {
        await useChatStore.getState().startNewSession();
      });

      const state = useChatStore.getState();
      expect(state.currentSessionId).toBeNull();
      expect(state.messages).toEqual([]);
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBeNull();
    });

    it('deletes the minted session as an orphan when a send raced the create', async () => {
      // A message starting mid-POST must win: adopting the minted id
      // here would point the panel at a session the server never wrote
      // to (the stream adopts its own id from the run_id frame instead).
      // Simulate the race: flip isStreaming while the POST is in flight.
      mockedService.createSession.mockImplementationOnce(async () => {
        useChatStore.setState({ isStreaming: true });
        return { session_id: 'srv-orphan', started_at: '2026-10-14T00:00:00Z' };
      });

      await act(async () => {
        await useChatStore.getState().startNewSession();
      });

      const state = useChatStore.getState();
      expect(state.currentSessionId).toBeNull();
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBeNull();
      expect(mockedService.deleteSession).toHaveBeenCalledWith('srv-orphan');
    });
  });

  describe('deleteSession', () => {
    it('removes the row from the list and leaves the panel alone', async () => {
      useChatStore.setState({
        sessionList: [makeSession('s1'), makeSession('s2')],
        currentSessionId: 's2',
        messages: [{ role: 'user', content: 'kept' }],
      });

      await act(async () => {
        await useChatStore.getState().deleteSession('s1');
      });

      expect(mockedService.deleteSession).toHaveBeenCalledWith('s1');
      expect(useChatStore.getState().sessionList.map((s) => s.id)).toEqual(['s2']);
      expect(useChatStore.getState().currentSessionId).toBe('s2');
      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'kept' },
      ]);
    });

    it('clears the panel when the deleted session was active', async () => {
      useChatStore.setState({
        sessionList: [makeSession('s1')],
        currentSessionId: 's1',
        messages: [{ role: 'user', content: 'gone' }],
      });
      sessionStorage.setItem(CURRENT_SESSION_KEY, 's1');

      await act(async () => {
        await useChatStore.getState().deleteSession('s1');
      });

      const state = useChatStore.getState();
      expect(state.sessionList).toEqual([]);
      expect(state.currentSessionId).toBeNull();
      expect(state.messages).toEqual([]);
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBeNull();
    });

    it('drops a stale row when the server reports it missing (404)', async () => {
      useChatStore.setState({ sessionList: [makeSession('s1')] });
      mockedService.deleteSession.mockRejectedValueOnce(
        new ChatApiError(404, 'Chat request failed (404)'),
      );

      await act(async () => {
        await useChatStore.getState().deleteSession('s1');
      });

      expect(useChatStore.getState().sessionList).toEqual([]);
    });

    it('keeps the row when the delete fails transiently', async () => {
      useChatStore.setState({ sessionList: [makeSession('s1')] });
      mockedService.deleteSession.mockRejectedValueOnce(new Error('network down'));

      await act(async () => {
        await useChatStore.getState().deleteSession('s1');
      });

      expect(useChatStore.getState().sessionList.map((s) => s.id)).toEqual(['s1']);
    });
  });

  // ---- Approval cards (openspec change agent-feed-tools §6.1/6.2) ----

  describe('approvals', () => {
    /** Seed an assistant turn whose step carries a pending card. */
    function seedPendingCard() {
      useChatStore.setState({
        messages: [
          { role: 'user', content: 'add a feed' },
          {
            role: 'assistant',
            content: '',
            steps: [
              {
                id: 'step_2',
                kind: 'tool',
                status: 'running',
                toolName: 'create_feed',
                confirm: {
                  runId: 'run-1',
                  approvalId: 'ap-1',
                  tool: 'create_feed',
                  args: { title: 'Rust Blog' },
                  expiresAt: 1791979500.0,
                  decision: 'pending',
                },
              },
            ],
          },
        ],
        isStreaming: false,
        error: null,
      });
    }

    it('attaches a pending card to the step named by the confirm_request frame', async () => {
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              {
                kind: 'plan',
                steps: [{ id: 'step_2', kind: 'tool', tool: 'create_feed' }],
              },
              { kind: 'step_start', step_id: 'step_2', step_kind: 'tool' },
              {
                kind: 'confirm_request',
                run_id: 'run-1',
                step_id: 'step_2',
                tool: 'create_feed',
                args: { title: 'Rust Blog' },
                approval_id: 'ap-1',
                expires_at: 1791979500.0,
              },
              {
                kind: 'done',
                usage: {
                  prompt_tokens: 1,
                  completion_tokens: 1,
                  total_tokens: 2,
                },
              },
            ],
            signal,
          ),
      );

      await act(async () => {
        useChatStore.getState().send('add a feed');
      });

      await act(async () => {
        releaseStream();
      });

      const assistant = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant');
      expect(assistant?.steps?.[0]).toEqual({
        id: 'step_2',
        kind: 'tool',
        status: 'running',
        toolName: 'create_feed',
        toolArgs: { title: 'Rust Blog' },
        confirm: {
          runId: 'run-1',
          approvalId: 'ap-1',
          tool: 'create_feed',
          args: { title: 'Rust Blog' },
          expiresAt: 1791979500.0,
          decision: 'pending',
        },
      });
    });

    it('appends a step row when the confirm_request precedes any step frame', async () => {
      // Defensive fallback: a partial trace (no plan / step_start) must
      // still give the card a row to render on.
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              {
                kind: 'confirm_request',
                run_id: 'run-1',
                step_id: 'step_9',
                tool: 'delete_feed',
                args: { id: 4 },
                approval_id: 'ap-9',
                expires_at: 1791979500.0,
              },
              {
                kind: 'done',
                usage: {
                  prompt_tokens: 1,
                  completion_tokens: 1,
                  total_tokens: 2,
                },
              },
            ],
            signal,
          ),
      );

      await act(async () => {
        useChatStore.getState().send('delete that feed');
      });

      await act(async () => {
        releaseStream();
      });

      const assistant = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant');
      expect(assistant?.steps?.[0]).toEqual({
        id: 'step_9',
        kind: 'tool',
        status: 'running',
        toolName: 'delete_feed',
        toolArgs: { id: 4 },
        confirm: {
          runId: 'run-1',
          approvalId: 'ap-9',
          tool: 'delete_feed',
          args: { id: 4 },
          expiresAt: 1791979500.0,
          decision: 'pending',
        },
      });
    });

    it('posts the approve decision body and flips the card to approved', async () => {
      seedPendingCard();

      await act(async () => {
        await useChatStore.getState().submitApproval('run-1', 'ap-1', 'approve');
      });

      expect(mockedService.submitApproval).toHaveBeenCalledWith({
        run_id: 'run-1',
        approval_id: 'ap-1',
        decision: 'approve',
      });
      const step = useChatStore.getState().messages[1].steps?.[0];
      expect(step?.confirm?.decision).toBe('approved');
      expect(step?.confirm?.error).toBeUndefined();
    });

    it('posts the deny decision body and flips the card to denied', async () => {
      seedPendingCard();

      await act(async () => {
        await useChatStore.getState().submitApproval('run-1', 'ap-1', 'deny');
      });

      expect(mockedService.submitApproval).toHaveBeenCalledWith({
        run_id: 'run-1',
        approval_id: 'ap-1',
        decision: 'deny',
      });
      expect(
        useChatStore.getState().messages[1].steps?.[0].confirm?.decision,
      ).toBe('denied');
    });

    it('keeps the card pending with a hint when the decision POST fails', async () => {
      seedPendingCard();
      mockedService.submitApproval.mockRejectedValueOnce(new Error('boom'));

      await act(async () => {
        await useChatStore.getState().submitApproval('run-1', 'ap-1', 'approve');
      });

      const confirm = useChatStore.getState().messages[1].steps?.[0].confirm;
      expect(confirm?.decision).toBe('pending');
      // A static client-side hint — never the server / axios message.
      expect(confirm?.error).toMatch(/could not send/i);
      expect(confirm?.error).not.toContain('boom');
    });

    it('resolves the card as expired when the decision POST 404s', async () => {
      // Design.md D2: a resumed run replays its checkpointed
      // confirm_request, whose approval id the decision endpoint
      // answers with a typed 404 — the client resolves that card as
      // expired instead of offering a retry that can never succeed.
      seedPendingCard();
      mockedService.submitApproval.mockRejectedValueOnce({
        response: { status: 404 },
      });

      await act(async () => {
        await useChatStore.getState().submitApproval('run-1', 'ap-1', 'approve');
      });

      const confirm = useChatStore.getState().messages[1].steps?.[0].confirm;
      expect(confirm?.decision).toBe('expired');
      // No retry hint on the terminal path.
      expect(confirm?.error).toBeUndefined();
    });

    it('expireApproval flips a pending card to expired', () => {
      seedPendingCard();

      useChatStore.getState().expireApproval('ap-1');

      expect(
        useChatStore.getState().messages[1].steps?.[0].confirm?.decision,
      ).toBe('expired');
    });

    it('expireApproval never overwrites a recorded outcome', async () => {
      seedPendingCard();

      await act(async () => {
        await useChatStore.getState().submitApproval('run-1', 'ap-1', 'approve');
      });
      useChatStore.getState().expireApproval('ap-1');

      expect(
        useChatStore.getState().messages[1].steps?.[0].confirm?.decision,
      ).toBe('approved');
    });

    it('a decision recorded after a local expiry wins', async () => {
      seedPendingCard();

      useChatStore.getState().expireApproval('ap-1');
      expect(
        useChatStore.getState().messages[1].steps?.[0].confirm?.decision,
      ).toBe('expired');

      await act(async () => {
        await useChatStore.getState().submitApproval('run-1', 'ap-1', 'approve');
      });

      expect(
        useChatStore.getState().messages[1].steps?.[0].confirm?.decision,
      ).toBe('approved');
    });

    it('expireApproval on an unknown approval id is a no-op', () => {
      seedPendingCard();
      const before = useChatStore.getState().messages;

      useChatStore.getState().expireApproval('missing');

      expect(useChatStore.getState().messages).toBe(before);
    });

    it('a retry hint never lands on an already-expired card', async () => {
      seedPendingCard();
      mockedService.submitApproval.mockRejectedValueOnce(new Error('boom'));
      useChatStore.getState().expireApproval('ap-1');

      await act(async () => {
        await useChatStore.getState().submitApproval('run-1', 'ap-1', 'approve');
      });

      const confirm = useChatStore.getState().messages[1].steps?.[0].confirm;
      expect(confirm?.decision).toBe('expired');
      expect(confirm?.error).toBeUndefined();
    });

    it('records the denial tool_result on the step (existing result path)', async () => {
      // After a deny, the agent answers the paused call with an
      // `ok:false` tool_result carrying the denial text; the store
      // routes it through the regular step-result field.
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) =>
          makeStream(
            [
              { kind: 'step_start', step_id: 'step_2', step_kind: 'tool' },
              {
                kind: 'confirm_request',
                run_id: 'run-1',
                step_id: 'step_2',
                tool: 'create_feed',
                args: { title: 'Rust Blog' },
                approval_id: 'ap-1',
                expires_at: 1791979500.0,
              },
              {
                kind: 'tool_result',
                step_id: 'step_2',
                call_id: 'c1',
                ok: false,
                // The producer's static denial text (approvals.py
                // `denial_text_for`), byte-for-byte.
                error:
                  'Confirmation required: the user denied this call. The `create_feed` tool was not executed.',
              },
              {
                kind: 'step_end',
                step_id: 'step_2',
                status: 'failed',
                reason: 'tool_denied',
              },
              {
                kind: 'done',
                usage: {
                  prompt_tokens: 1,
                  completion_tokens: 1,
                  total_tokens: 2,
                },
              },
            ],
            signal,
          ),
      );

      await act(async () => {
        useChatStore.getState().send('add a feed');
      });

      await act(async () => {
        releaseStream();
      });

      const step = useChatStore
        .getState()
        .messages.find((m) => m.role === 'assistant')?.steps?.[0];
      expect(step?.toolResult).toBe(
        'Confirmation required: the user denied this call. The `create_feed` tool was not executed.',
      );
      expect(step?.status).toBe('failed');
    });
  });

  describe('background runs (navigation detaches instead of aborting)', () => {
    /**
     * Push-based stream mock: events are fed one at a time from the
     * test, and `close()` ends the generator. Unlike the shared
     * `makeStream` helper (whose single `__chatRelease` hook can only
     * hold ONE stream), each instance is independent — detached-run
     * tests need two live streams at once (a detached one + an
     * attached one).
     */
    function makePushStream() {
      const queue: ChatEvent[] = [];
      let notify: (() => void) | null = null;
      let closed = false;
      let failure: Error | null = null;
      const captured: {
        signal?: AbortSignal;
        opts?: { onRunId?: (id: string) => void };
      } = {};
      async function* impl(
        _messages: unknown,
        signal: AbortSignal,
        opts?: { onRunId?: (id: string) => void },
      ): AsyncGenerator<ChatEvent> {
        captured.signal = signal;
        captured.opts = opts;
        while (!closed || queue.length > 0) {
          if (failure) throw failure;
          if (queue.length === 0) {
            await new Promise<void>((resolve) => {
              notify = resolve;
            });
            continue;
          }
          yield queue.shift()!;
        }
      }
      return {
        impl,
        captured,
        push(event: ChatEvent) {
          queue.push(event);
          notify?.();
          notify = null;
        },
        close() {
          closed = true;
          notify?.();
          notify = null;
        },
        fail(err: Error) {
          failure = err;
          notify?.();
          notify = null;
        },
      };
    }

    it('startNewSession detaches the live stream: not aborted, still draining, rail marked', async () => {
      useChatStore.setState({ currentSessionId: 'sess-a' });
      const stream = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(stream.impl);

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        await useChatStore.getState().startNewSession();
      });

      const state = useChatStore.getState();
      // The core guarantee: the connection was NOT aborted — the run
      // keeps going server-side.
      expect(stream.captured.signal?.aborted).toBe(false);
      // Panel moved on; the run's session is marked as still working.
      expect(state.currentSessionId).toBe('srv-new');
      expect(state.messages).toEqual([]);
      expect(state.isStreaming).toBe(false);
      expect(state.runningSessionIds).toEqual(['sess-a']);

      // A frame that arrives after the detach is drained and dropped —
      // it must not leak into the new session's panel.
      await act(async () => {
        stream.push({ kind: 'delta', delta: 'late answer' });
        await Promise.resolve();
      });
      expect(useChatStore.getState().messages).toEqual([]);

      // Natural end: the rail refreshes and the marker clears. The
      // finished run belongs to 'sess-a', not the active 'srv-new', so
      // no replay fetch fires.
      await act(async () => {
        stream.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().runningSessionIds).toEqual([]);
      });
      expect(mockedService.listSessions).toHaveBeenCalledTimes(1);
      expect(mockedService.getMessages).not.toHaveBeenCalled();
      expect(useChatStore.getState().isStreaming).toBe(false);
    });

    it('replays the finished run when its session is back on screen', async () => {
      useChatStore.setState({ currentSessionId: 'sess-a' });
      const stream = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(stream.impl);

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      // Detach, then come back to the run's session before it ends:
      // the replay shows the persisted rows as they are now …
      await act(async () => {
        await useChatStore.getState().startNewSession();
      });
      mockedService.getMessages.mockResolvedValueOnce([
        makeMessageRow({ id: 1, role: 'user', content: 'hi' }),
      ]);
      await act(async () => {
        await useChatStore.getState().setCurrentSession('sess-a');
      });
      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'hi' },
      ]);

      // … and the run's terminal frame re-replays: by then the
      // assistant row is durable server-side (the agent persists it
      // before `[DONE]`), so the completed turn lands in full.
      mockedService.getMessages.mockResolvedValueOnce([
        makeMessageRow({ id: 1, role: 'user', content: 'hi' }),
        makeMessageRow({ id: 2, role: 'assistant', content: 'full answer' }),
      ]);
      await act(async () => {
        stream.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().messages).toEqual([
          { role: 'user', content: 'hi' },
          { role: 'assistant', content: 'full answer' },
        ]);
      });
      expect(mockedService.getMessages).toHaveBeenCalledTimes(2);
      expect(useChatStore.getState().runningSessionIds).toEqual([]);
    });

    it('a run_id frame arriving while detached marks the frame id and is never adopted', async () => {
      const stream = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(stream.impl);

      act(() => {
        void useChatStore.getState().send('first message');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        await useChatStore.getState().startNewSession();
      });
      expect(useChatStore.getState().currentSessionId).toBe('srv-new');

      // The server mints the session (the frame follows the row
      // insert): a detached stream re-keys its rail marker to the
      // actual id but never adopts it into the panel.
      await act(async () => {
        stream.captured.opts?.onRunId?.('srv-minted');
        await Promise.resolve();
      });
      expect(useChatStore.getState().currentSessionId).toBe('srv-new');
      expect(useChatStore.getState().runningSessionIds).toEqual(['srv-minted']);

      await act(async () => {
        stream.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().runningSessionIds).toEqual([]);
      });
    });

    it("detached frames cannot leak into the new session's panel", async () => {
      useChatStore.setState({ currentSessionId: 'sess-a' });
      const streamA = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(streamA.impl);

      act(() => {
        void useChatStore.getState().send('a');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        await useChatStore.getState().startNewSession();
      });

      // The new session streams its own turn — its assistant row lands
      // at the SAME index A's did (both are messages[1]). This is the
      // collision the attachment gate exists to prevent.
      const streamB = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(streamB.impl);
      act(() => {
        void useChatStore.getState().send('b');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });
      await act(async () => {
        streamB.push({ kind: 'delta', delta: 'B-answer' });
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().messages[1]?.content).toBe('B-answer');
      });

      // A's late delta (index 1 in A's old array) must be dropped.
      await act(async () => {
        streamA.push({ kind: 'delta', delta: 'A-late' });
        await Promise.resolve();
      });
      await act(async () => {
        streamA.push({
          kind: 'confirm_request',
          run_id: 'run-a',
          step_id: 'step_2',
          tool: 'create_feed',
          args: { title: 'A-feed' },
          approval_id: 'ap-a',
          expires_at: Date.now() / 1000 + 300,
        });
        await Promise.resolve();
      });
      expect(
        useChatStore
          .getState()
          .messages.flatMap((m) => m.steps ?? [])
          .filter((s) => s.confirm),
      ).toHaveLength(0);
      await waitFor(() => {
        expect(useChatStore.getState().messages[1]?.content).toBe('B-answer');
      });
      expect(useChatStore.getState().messages).toHaveLength(2);
      expect(useChatStore.getState().messages[1]?.content).not.toContain(
        'A-late',
      );

      await act(async () => {
        streamA.close();
        streamB.close();
        await Promise.resolve();
      });
    });

    it('send refuses a follow-up while the active session has a live detached run', async () => {
      useChatStore.setState({ currentSessionId: 'sess-a' });
      const stream = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(stream.impl);

      act(() => {
        void useChatStore.getState().send('one');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        await useChatStore.getState().startNewSession();
      });
      await act(async () => {
        await useChatStore.getState().setCurrentSession('sess-a');
      });

      // The busy session's composer is (also) guarded in the store: a
      // second concurrent run on one session would interleave rows.
      await act(async () => {
        await useChatStore.getState().send('two');
      });
      expect(mockedService.streamMessage).toHaveBeenCalledTimes(1);
      // The refused send appended nothing.
      expect(useChatStore.getState().messages).toEqual([]);

      await act(async () => {
        stream.close();
        await Promise.resolve();
      });
    });

    it("an older stream's terminal cannot clear a newer stream's isStreaming", async () => {
      useChatStore.setState({ currentSessionId: 'sess-a' });
      const streamA = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(streamA.impl);

      act(() => {
        void useChatStore.getState().send('a');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        await useChatStore.getState().startNewSession();
      });

      const streamB = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(streamB.impl);
      act(() => {
        void useChatStore.getState().send('b');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      // A finishes long after B started: its finally must not flip the
      // panel's streaming state off under B.
      await act(async () => {
        streamA.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().runningSessionIds).toEqual([]);
      });
      expect(useChatStore.getState().isStreaming).toBe(true);

      await act(async () => {
        streamB.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(false);
      });
    });

    it('reset (dialog close) detaches instead of aborting', async () => {
      useChatStore.setState({ currentSessionId: 'sess-a' });
      const stream = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(stream.impl);

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        useChatStore.getState().reset();
      });

      const state = useChatStore.getState();
      expect(state.messages).toEqual([]);
      expect(state.isStreaming).toBe(false);
      expect(stream.captured.signal?.aborted).toBe(false);
      expect(state.runningSessionIds).toEqual(['sess-a']);

      await act(async () => {
        stream.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().runningSessionIds).toEqual([]);
      });
    });

    it('setCurrentSession detaches the live stream of the session left behind', async () => {
      useChatStore.setState({ currentSessionId: 'sess-a' });
      const stream = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(stream.impl);

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        await useChatStore.getState().setCurrentSession('sess-b');
      });

      expect(stream.captured.signal?.aborted).toBe(false);
      expect(useChatStore.getState().currentSessionId).toBe('sess-b');
      expect(useChatStore.getState().runningSessionIds).toEqual(['sess-a']);

      await act(async () => {
        stream.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().runningSessionIds).toEqual([]);
      });
    });

    it('deleting the active session still aborts its run (destructive gesture wins)', async () => {
      useChatStore.setState({
        currentSessionId: 'sess-a',
        sessionList: [makeSession('sess-a')],
      });
      const stream = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(stream.impl);

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        await useChatStore.getState().deleteSession('sess-a');
      });

      expect(stream.captured.signal?.aborted).toBe(true);
      expect(useChatStore.getState().currentSessionId).toBeNull();
      expect(useChatStore.getState().messages).toEqual([]);
      expect(useChatStore.getState().runningSessionIds).toEqual([]);
    });

    it('a detached failure logs instead of surfacing in the panel', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      useChatStore.setState({ currentSessionId: 'sess-a' });
      const stream = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(stream.impl);

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        await useChatStore.getState().startNewSession();
      });

      // Real failure (not an abort) while detached: the new session's
      // panel must not show the old run's error — log only.
      await act(async () => {
        stream.fail(new Error('network down'));
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().runningSessionIds).toEqual([]);
      });
      expect(useChatStore.getState().error).toBeNull();
      expect(useChatStore.getState().messages).toEqual([]);
      warn.mockRestore();
    });

    it('keeps the rail marker (composer lock) up until the completion replay lands', async () => {
      useChatStore.setState({ currentSessionId: 'sess-a' });
      const stream = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(stream.impl);

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      await act(async () => {
        await useChatStore.getState().startNewSession();
      });
      await act(async () => {
        await useChatStore.getState().setCurrentSession('sess-a');
      });
      expect(useChatStore.getState().runningSessionIds).toEqual(['sess-a']);

      // The terminal frame lands while the session is on screen: the
      // completion replay is issued but stays in flight.
      let releaseReplay: ((rows: SessionMessage[]) => void) | null = null;
      mockedService.getMessages.mockImplementationOnce(
        () =>
          new Promise<SessionMessage[]>((resolve) => {
            releaseReplay = resolve;
          }),
      );
      await act(async () => {
        stream.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(mockedService.getMessages).toHaveBeenCalledTimes(2);
      });
      // Still marked: unlocking before the replay lands would let a
      // fast follow-up start a run that the replay then clobbers.
      expect(useChatStore.getState().runningSessionIds).toEqual(['sess-a']);
      await act(async () => {
        await useChatStore.getState().send('too early');
      });
      expect(mockedService.streamMessage).toHaveBeenCalledTimes(1);

      await act(async () => {
        releaseReplay!([
          makeMessageRow({ id: 1, role: 'user', content: 'hi' }),
          makeMessageRow({ id: 2, role: 'assistant', content: 'answer' }),
        ]);
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().runningSessionIds).toEqual([]);
      });
      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'hi' },
        { role: 'assistant', content: 'answer' },
      ]);
    });

    it('tracks two detached runs in different sessions independently', async () => {
      useChatStore.setState({ currentSessionId: 'sess-a' });

      const streamA = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(streamA.impl);
      act(() => {
        void useChatStore.getState().send('a');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      mockedService.createSession.mockResolvedValueOnce({
        session_id: 'srv-1',
        started_at: '2026-10-14T00:00:00Z',
      });
      await act(async () => {
        await useChatStore.getState().startNewSession();
      });
      expect(useChatStore.getState().runningSessionIds).toEqual(['sess-a']);

      const streamB = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(streamB.impl);
      act(() => {
        void useChatStore.getState().send('b');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      mockedService.createSession.mockResolvedValueOnce({
        session_id: 'srv-2',
        started_at: '2026-10-14T00:00:00Z',
      });
      await act(async () => {
        await useChatStore.getState().startNewSession();
      });
      expect(useChatStore.getState().runningSessionIds).toEqual([
        'sess-a',
        'srv-1',
      ]);

      // A finishes: only A's marker retires; B keeps pulsing.
      await act(async () => {
        streamA.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().runningSessionIds).toEqual(['srv-1']);
      });

      await act(async () => {
        streamB.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().runningSessionIds).toEqual([]);
      });
    });

    it('cancel is a no-op for a detached run (no client handle, marker intact)', async () => {
      useChatStore.setState({ currentSessionId: 'sess-a' });
      const stream = makePushStream();
      mockedService.streamMessage.mockImplementationOnce(stream.impl);

      act(() => {
        void useChatStore.getState().send('a');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });
      await act(async () => {
        await useChatStore.getState().startNewSession();
      });

      await act(async () => {
        useChatStore.getState().cancel();
      });
      expect(stream.captured.signal?.aborted).toBe(false);
      expect(useChatStore.getState().runningSessionIds).toEqual(['sess-a']);

      await act(async () => {
        stream.close();
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().runningSessionIds).toEqual([]);
      });
    });
  });

  describe('resetForUser', () => {
    it('adopts the first identity without clearing the restored tab state', () => {
      useChatStore.setState({
        messages: [{ role: 'user', content: 'hello' }],
        currentSessionId: 'sess-a',
        sessionList: [makeSession('sess-a')],
      });

      useChatStore.getState().resetForUser(1);

      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'hello' },
      ]);
      expect(useChatStore.getState().currentSessionId).toBe('sess-a');
      expect(useChatStore.getState().sessionList).toEqual([
        makeSession('sess-a'),
      ]);
    });

    it('is a no-op for the same identity', () => {
      useChatStore.getState().resetForUser(1);
      useChatStore.setState({
        messages: [{ role: 'user', content: 'hello' }],
        currentSessionId: 'sess-a',
        sessionList: [makeSession('sess-a')],
      });

      useChatStore.getState().resetForUser(1);

      expect(useChatStore.getState().messages).toHaveLength(1);
      expect(useChatStore.getState().currentSessionId).toBe('sess-a');
      expect(useChatStore.getState().sessionList).toHaveLength(1);
    });

    it('clears the panel, rail, and persisted id when the identity changes', () => {
      useChatStore.getState().resetForUser(1);
      sessionStorage.setItem(CURRENT_SESSION_KEY, 'sess-a');
      useChatStore.setState({
        messages: [{ role: 'user', content: 'old user message' }],
        isStreaming: true,
        error: 'boom',
        currentSessionId: 'sess-a',
        sessionList: [makeSession('sess-a')],
        sessionListNextCursor: 'cursor',
        isFetchingMore: true,
        runningSessionIds: ['sess-a'],
      });

      useChatStore.getState().resetForUser(2);

      expect(useChatStore.getState()).toMatchObject({
        messages: [],
        isStreaming: false,
        error: null,
        currentSessionId: null,
        sessionList: [],
        sessionListNextCursor: null,
        isFetchingMore: false,
        runningSessionIds: [],
      });
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBeNull();
    });

    it('clears on logout (null identity)', () => {
      useChatStore.getState().resetForUser(1);
      sessionStorage.setItem(CURRENT_SESSION_KEY, 'sess-b');
      useChatStore.setState({
        messages: [{ role: 'assistant', content: 'old answer' }],
        currentSessionId: 'sess-b',
        sessionList: [makeSession('sess-b')],
      });

      useChatStore.getState().resetForUser(null);

      expect(useChatStore.getState().messages).toEqual([]);
      expect(useChatStore.getState().currentSessionId).toBeNull();
      expect(useChatStore.getState().sessionList).toEqual([]);
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBeNull();
    });

    it('drops a list page that resolves after the identity changed', async () => {
      useChatStore.getState().resetForUser(3);
      let resolveList!: (page: {
        sessions: ReturnType<typeof makeSession>[];
        nextCursor: string | null;
      }) => void;
      mockedService.listSessions.mockReturnValueOnce(
        new Promise((resolve) => {
          resolveList = resolve;
        }),
      );

      const pending = useChatStore.getState().loadMore();
      await Promise.resolve();
      useChatStore.getState().resetForUser(4);
      resolveList({ sessions: [makeSession('old-user-session')], nextCursor: null });
      await act(async () => {
        await pending;
      });

      expect(useChatStore.getState().sessionList).toEqual([]);
      expect(useChatStore.getState().isFetchingMore).toBe(false);
    });

    it('drops a page-one refresh that resolves after the identity changed', async () => {
      useChatStore.getState().resetForUser(1);
      let resolveList!: (page: {
        sessions: ReturnType<typeof makeSession>[];
        nextCursor: string | null;
      }) => void;
      mockedService.listSessions.mockReturnValueOnce(
        new Promise((resolve) => {
          resolveList = resolve;
        }),
      );

      const pending = useChatStore.getState().refreshSessionList();
      await Promise.resolve();
      useChatStore.getState().resetForUser(2);
      resolveList({ sessions: [makeSession('old-user-session')], nextCursor: null });
      await act(async () => {
        await pending;
      });

      expect(useChatStore.getState().sessionList).toEqual([]);
    });

    it('does not adopt a session created for the previous identity', async () => {
      useChatStore.getState().resetForUser(1);
      let resolveCreate!: (value: {
        session_id: string;
        started_at: string;
      }) => void;
      mockedService.createSession.mockReturnValueOnce(
        new Promise((resolve) => {
          resolveCreate = resolve;
        }),
      );

      let pending!: Promise<void>;
      await act(async () => {
        pending = useChatStore.getState().startNewSession();
        await Promise.resolve();
      });
      useChatStore.getState().resetForUser(2);
      resolveCreate({
        session_id: 'old-user-session',
        started_at: '2026-10-14T00:00:00Z',
      });
      await act(async () => {
        await pending;
      });

      expect(useChatStore.getState().currentSessionId).toBeNull();
      expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBeNull();
      expect(mockedService.deleteSession).not.toHaveBeenCalled();
    });

    it('aborts the attached stream when the identity changes', async () => {
      useChatStore.getState().resetForUser(1);
      let capturedSignal: AbortSignal | null = null;
      mockedService.streamMessage.mockImplementationOnce(
        (_messages: unknown, signal: AbortSignal) => {
          capturedSignal = signal;
          return (async function* () {
            await new Promise<void>((resolve) => {
              signal.addEventListener('abort', () => resolve(), { once: true });
            });
          })();
        },
      );

      await act(async () => {
        void useChatStore.getState().send('hi');
        await Promise.resolve();
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });

      useChatStore.getState().resetForUser(2);

      expect(capturedSignal!.aborted).toBe(true);
      expect(useChatStore.getState().messages).toEqual([]);
      expect(useChatStore.getState().isStreaming).toBe(false);
    });

    it('ignores a run_id frame delivered after the identity changed', async () => {
      useChatStore.getState().resetForUser(1);
      let fireRunId!: () => void;
      const runIdGate = new Promise<void>((resolve) => {
        fireRunId = resolve;
      });
      mockedService.streamMessage.mockImplementationOnce(
        (
          _messages: unknown,
          _signal: AbortSignal,
          opts?: { onRunId?: (id: string) => void },
        ) =>
          (async function* () {
            await runIdGate;
            opts?.onRunId?.('old-user-session');
            await new Promise<void>(() => {});
          })(),
      );

      act(() => {
        void useChatStore.getState().send('hi');
      });
      await waitFor(() => {
        expect(useChatStore.getState().isStreaming).toBe(true);
      });
      await act(async () => {
        await useChatStore.getState().startNewSession();
      });
      useChatStore.getState().resetForUser(2);
      const callsBefore = mockedService.listSessions.mock.calls.length;

      await act(async () => {
        fireRunId();
        await Promise.resolve();
      });

      expect(useChatStore.getState().runningSessionIds).toEqual([]);
      expect(mockedService.listSessions.mock.calls.length).toBe(callsBefore);
    });

    it('does not clear the new identity panel when a stale replay 404s', async () => {
      useChatStore.getState().resetForUser(1);
      let rejectResume!: (err: Error) => void;
      mockedService.resumeSession.mockReturnValueOnce(
        new Promise((_resolve, reject) => {
          rejectResume = reject;
        }),
      );

      act(() => {
        void useChatStore.getState().setCurrentSession('sess-a');
      });
      useChatStore.getState().resetForUser(2);
      await act(async () => {
        useChatStore.setState({
          currentSessionId: 'sess-b',
          messages: [{ role: 'user', content: 'bob message' }],
        });
        rejectResume(new ChatApiError(404, 'gone'));
        await Promise.resolve();
      });

      expect(useChatStore.getState().currentSessionId).toBe('sess-b');
      expect(useChatStore.getState().messages).toEqual([
        { role: 'user', content: 'bob message' },
      ]);
    });
  });
});
