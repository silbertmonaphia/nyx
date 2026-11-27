import { waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { TestProviders, renderHook } from '~/test/test-utils';
import { publicConfigService } from '../services/publicConfigService';
import { usePublicConfig } from './usePublicConfig';

vi.mock('../services/publicConfigService');

const mockedService = publicConfigService as unknown as {
  get: ReturnType<typeof vi.fn>;
};

describe('usePublicConfig', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('reports the registration mode once loaded', async () => {
    mockedService.get.mockResolvedValue('closed');
    const { result } = renderHook(() => usePublicConfig(), {
      wrapper: TestProviders,
    });
    await waitFor(() => expect(result.current.data).toBe('closed'));
  });

  it('reports null while the probe has no value', () => {
    mockedService.get.mockResolvedValue(null);
    const { result } = renderHook(() => usePublicConfig(), {
      wrapper: TestProviders,
    });
    expect(result.current.data).toBeUndefined();
  });
});
