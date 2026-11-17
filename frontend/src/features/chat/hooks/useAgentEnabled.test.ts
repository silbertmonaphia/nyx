import { waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { TestProviders, renderHook } from '~/test/test-utils';
import { useAuthStore } from '~/store/authStore';
import { featuresService } from '../services/featuresService';
import { useAgentEnabled } from './useAgentEnabled';

vi.mock('../services/featuresService');

const mockedService = featuresService as unknown as {
  get: ReturnType<typeof vi.fn>;
};

const user = (id: number) => ({
  id,
  username: `user-${id}`,
  created_at: '',
  updated_at: '',
});

describe('useAgentEnabled', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useAuthStore.setState({ user: user(1), isAuthenticated: true });
  });

  it('reports the feature flag once loaded', async () => {
    mockedService.get.mockResolvedValue({ agentEnabled: true });
    const { result } = renderHook(() => useAgentEnabled(), {
      wrapper: TestProviders,
    });
    await waitFor(() => expect(result.current).toBe(true));
  });

  it('stays undefined until the probe settles', () => {
    mockedService.get.mockResolvedValue({ agentEnabled: false });
    const { result } = renderHook(() => useAgentEnabled(), {
      wrapper: TestProviders,
    });
    expect(result.current).toBeUndefined();
  });

  it('stays undefined when the probe fails', async () => {
    mockedService.get.mockRejectedValue(new Error('probe failed'));
    const { result } = renderHook(() => useAgentEnabled(), {
      wrapper: TestProviders,
    });
    await waitFor(() => expect(mockedService.get).toHaveBeenCalled());
    expect(result.current).toBeUndefined();
  });

  it('does not fetch without an authenticated user', () => {
    useAuthStore.setState({ user: null, isAuthenticated: false });
    renderHook(() => useAgentEnabled(), { wrapper: TestProviders });
    expect(mockedService.get).not.toHaveBeenCalled();
  });
});
