import { describe, expect, it, beforeEach, vi } from 'vitest';
import { act } from '@testing-library/react';
import { chatService } from '../services/chatService';
import { useChatStore } from './chatStore';
import { useAuthStore } from '~/store/authStore';
import type { ChatEvent } from '../types/chat';

vi.mock('../services/chatService');

const mockedService = chatService as unknown as {
  streamMessage: ReturnType<typeof vi.fn>;
  createSession: ReturnType<typeof vi.fn>;
  listSessions: ReturnType<typeof vi.fn>;
  getMessages: ReturnType<typeof vi.fn>;
  getMessagesPage: ReturnType<typeof vi.fn>;
  cancelRun: ReturnType<typeof vi.fn>;
};

const KNOWLEDGE_KEY = 'nyx.chat.knowledge.1';

const quickStream = () =>
  (async function* (): AsyncGenerator<ChatEvent> {
    yield { kind: 'terminator' };
  })();

const reset = () => {
  useChatStore.getState().clearActiveSession();
  useChatStore.setState({
    messages: [],
    isStreaming: false,
    error: null,
    llmGateOpen: false,
    possiblyIncomplete: false,
    currentSessionId: null,
    ownerUserId: 1,
    sessionList: [],
    sessionListNextCursor: null,
    isFetchingMore: false,
    runningSessionIds: [],
    knowledgeEnabled: false,
  });
  localStorage.clear();
  sessionStorage.clear();
  useAuthStore.setState({
    user: {
      id: 1,
      username: 'tester',
      created_at: '',
      updated_at: '',
    },
    isAuthenticated: true,
  });
  mockedService.streamMessage.mockReset();
  mockedService.streamMessage.mockImplementation(() => quickStream());
  mockedService.createSession
    .mockReset()
    .mockResolvedValue({ session_id: 'srv', started_at: '2026-01-01T00:00:00Z' });
  mockedService.listSessions
    .mockReset()
    .mockResolvedValue({ sessions: [], nextCursor: null });
  mockedService.getMessages.mockReset().mockResolvedValue([]);
  mockedService.getMessagesPage
    .mockReset()
    .mockImplementation(async (id: string) => ({
      messages: await (
        mockedService.getMessages as unknown as (sid: string) => Promise<unknown[]>
      )(id),
      hasMore: false,
      nextBefore: null,
    }));
  mockedService.cancelRun.mockReset().mockResolvedValue(undefined);
};

const lastStreamOptions = () => {
  const calls = mockedService.streamMessage.mock.calls;
  const call = calls[calls.length - 1];
  return call?.[2] as { kb?: boolean } | undefined;
};

describe('knowledge toggle', () => {
  beforeEach(reset);

  it('defaults to disabled and sends no kb flag', async () => {
    await act(async () => {
      await useChatStore.getState().send('hello');
    });
    expect(useChatStore.getState().knowledgeEnabled).toBe(false);
    expect(lastStreamOptions()?.kb).toBe(false);
  });

  it('persists the choice and sends kb:true on the next request', async () => {
    act(() => {
      useChatStore.getState().setKnowledgeEnabled(true);
    });
    expect(localStorage.getItem(KNOWLEDGE_KEY)).toBe('true');
    expect(useChatStore.getState().knowledgeEnabled).toBe(true);

    await act(async () => {
      await useChatStore.getState().send('hello');
    });
    expect(lastStreamOptions()?.kb).toBe(true);
  });

  it('persists disabling again', () => {
    act(() => {
      useChatStore.getState().setKnowledgeEnabled(true);
      useChatStore.getState().setKnowledgeEnabled(false);
    });
    expect(localStorage.getItem(KNOWLEDGE_KEY)).toBe('false');
    expect(useChatStore.getState().knowledgeEnabled).toBe(false);
  });

  it('scopes the choice to the active user', () => {
    act(() => {
      useChatStore.getState().setKnowledgeEnabled(true);
    });
    expect(localStorage.getItem('nyx.chat.knowledge.1')).toBe('true');

    act(() => {
      useChatStore.getState().resetForUser(2);
    });
    expect(useChatStore.getState().knowledgeEnabled).toBe(false);
    expect(localStorage.getItem('nyx.chat.knowledge.2')).toBeNull();

    act(() => {
      useChatStore.getState().setKnowledgeEnabled(true);
    });
    expect(localStorage.getItem('nyx.chat.knowledge.2')).toBe('true');

    act(() => {
      useChatStore.getState().resetForUser(1);
    });
    expect(useChatStore.getState().knowledgeEnabled).toBe(true);
    expect(localStorage.getItem('nyx.chat.knowledge.1')).toBe('true');
  });
});
