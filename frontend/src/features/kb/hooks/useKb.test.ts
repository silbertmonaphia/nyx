import { act, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { TestProviders, renderHook } from '~/test/test-utils';
import { useAuthStore } from '~/store/authStore';
import { kbService } from '../services/kbService';
import { useKbSources } from './useKb';

vi.mock('../services/kbService');

const mockedService = kbService as unknown as {
  listSources: ReturnType<typeof vi.fn>;
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
