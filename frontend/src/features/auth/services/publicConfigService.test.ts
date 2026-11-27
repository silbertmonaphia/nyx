import { beforeEach, describe, expect, it, vi } from 'vitest';
import api from '~/services/api';
import { publicConfigService } from './publicConfigService';

vi.mock('~/services/api', () => ({
  default: {
    get: vi.fn(),
  },
}));

const mockedApi = api as unknown as {
  get: ReturnType<typeof vi.fn>;
};

describe('publicConfigService', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('reads registration_mode from the public config envelope', async () => {
    for (const mode of ['open', 'invite_only', 'closed'] as const) {
      mockedApi.get.mockResolvedValue({ data: { registration_mode: mode } });
      await expect(publicConfigService.get()).resolves.toBe(mode);
    }

    expect(mockedApi.get).toHaveBeenCalledWith('/config', {
      skipErrorToast: true,
      skipSentry: true,
    });
  });

  it('returns null for a missing or unknown mode', async () => {
    mockedApi.get.mockResolvedValue({ data: {} });
    await expect(publicConfigService.get()).resolves.toBeNull();

    mockedApi.get.mockResolvedValue({ data: { registration_mode: 'paused' } });
    await expect(publicConfigService.get()).resolves.toBeNull();
  });
});
