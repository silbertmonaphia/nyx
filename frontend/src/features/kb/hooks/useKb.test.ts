import { act, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { TestProviders, renderHook } from '~/test/test-utils';
import { useAuthStore } from '~/store/authStore';
import { kbService } from '../services/kbService';
import { useKbAvailability, useKbSettings, useKbSources } from './useKb';

vi.mock('../services/kbService');

const mockedService = kbService as unknown as {
  listSources: ReturnType<typeof vi.fn>;
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
