import { act, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { TestProviders, renderHook } from '~/test/test-utils';
import { useAuthStore } from '~/store/authStore';
import { kbService } from '../services/kbService';
import {
  useKbAvailability,
  useKbDocuments,
  useKbSettings,
  useKbSources,
} from './useKb';
import type { KbDocument } from '../types/kb';

vi.mock('../services/kbService');

const mockedService = kbService as unknown as {
  listSources: ReturnType<typeof vi.fn>;
  listDocuments: ReturnType<typeof vi.fn>;
  getSettings: ReturnType<typeof vi.fn>;
};

const user = (id: number) => ({
  id,
  username: `user-${id}`,
  created_at: '',
  updated_at: '',
});

describe('useKbSources identity scoping', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockedService.listSources.mockResolvedValue([]);
  });

  it('refetches under a new query key when the authenticated user changes', async () => {
    useAuthStore.setState({ user: user(1), isAuthenticated: true });
    const { rerender } = renderHook(() => useKbSources(), {
      wrapper: TestProviders,
    });
    await waitFor(() => expect(mockedService.listSources).toHaveBeenCalledTimes(1));

    act(() => {
      useAuthStore.setState({ user: user(2), isAuthenticated: true });
    });
    rerender();

    await waitFor(() => expect(mockedService.listSources).toHaveBeenCalledTimes(2));
  });

  it('does not fetch without an authenticated user', () => {
    useAuthStore.setState({ user: null, isAuthenticated: false });
    renderHook(() => useKbSources(), { wrapper: TestProviders });
    expect(mockedService.listSources).not.toHaveBeenCalled();
  });
});

const useAvailabilityProbe = () => ({
  status: useKbSettings().status,
  available: useKbAvailability(),
});

const makeDocument = (id: number): KbDocument => ({
  id,
  source_id: 1,
  origin: 'inline',
  rel_path: `notes-${id}.md`,
  title: `Notes ${id}`,
  doc_type: 'markdown',
  byte_size: 10,
  status: 'indexed',
  generation: 1,
  mtime: null,
  updated_at: '2026-01-01T00:00:00Z',
});

describe('useKbDocuments pagination', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useAuthStore.setState({ user: user(1), isAuthenticated: true });
  });

  it('loads pages until a short page ends the list', async () => {
    mockedService.listDocuments
      .mockResolvedValueOnce(
        Array.from({ length: 50 }, (_, index) => makeDocument(index + 1)),
      )
      .mockResolvedValueOnce(
        Array.from({ length: 20 }, (_, index) => makeDocument(index + 51)),
      );

    const { result } = renderHook(() => useKbDocuments({ sourceId: 7 }), {
      wrapper: TestProviders,
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(mockedService.listDocuments).toHaveBeenCalledWith({
      sourceId: 7,
      limit: 50,
      offset: 0,
    });
    expect(result.current.hasNextPage).toBe(true);

    await act(async () => {
      await result.current.fetchNextPage();
    });

    expect(mockedService.listDocuments).toHaveBeenLastCalledWith({
      sourceId: 7,
      limit: 50,
      offset: 50,
    });
    await waitFor(() => expect(result.current.hasNextPage).toBe(false));
    expect(result.current.data?.pages.flat()).toHaveLength(70);
  });
});

describe('useKbAvailability', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useAuthStore.setState({ user: user(1), isAuthenticated: true });
  });

  it('becomes true once the settings endpoint reports the corpus enabled', async () => {
    mockedService.getSettings.mockResolvedValue({
      enabled: true,
      embedding_model: 'mock-embedding',
    });
    const { result } = renderHook(() => useAvailabilityProbe(), {
      wrapper: TestProviders,
    });
    await waitFor(() => expect(result.current.status).toBe('success'));
    expect(result.current.available).toBe(true);
  });

  it('settles false when the settings endpoint reports the corpus disabled', async () => {
    mockedService.getSettings.mockResolvedValue({
      enabled: false,
      embedding_model: '',
    });
    const { result } = renderHook(() => useAvailabilityProbe(), {
      wrapper: TestProviders,
    });
    await waitFor(() => expect(result.current.status).toBe('success'));
    expect(result.current.available).toBe(false);
  });

  it('settles false when the settings endpoint 404s without retrying', async () => {
    mockedService.getSettings.mockRejectedValue({ response: { status: 404 } });
    const { result } = renderHook(() => useAvailabilityProbe(), {
      wrapper: TestProviders,
    });
    await waitFor(() => expect(result.current.status).toBe('error'));
    expect(result.current.available).toBe(false);
    expect(mockedService.getSettings).toHaveBeenCalledTimes(1);
  });

  it('does not fetch without an authenticated user', () => {
    useAuthStore.setState({ user: null, isAuthenticated: false });
    renderHook(() => useKbAvailability(), { wrapper: TestProviders });
    expect(mockedService.getSettings).not.toHaveBeenCalled();
  });
});
