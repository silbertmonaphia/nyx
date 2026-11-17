import { beforeEach, describe, expect, it, vi } from 'vitest';
import api from '~/services/api';
import { featuresService } from './featuresService';

vi.mock('~/services/api', () => ({
  default: {
    get: vi.fn(),
  },
}));

const mockedApi = api as unknown as {
  get: ReturnType<typeof vi.fn>;
};

describe('featuresService', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('reads agent_enabled from the features envelope', async () => {
    mockedApi.get.mockResolvedValue({ data: { agent_enabled: true } });
    await expect(featuresService.get()).resolves.toEqual({ agentEnabled: true });
    expect(mockedApi.get).toHaveBeenCalledWith('/features', {
      skipErrorToast: true,
      skipSentry: true,
    });
  });

  it('treats a missing or non-boolean flag as disabled', async () => {
    mockedApi.get.mockResolvedValue({ data: {} });
    await expect(featuresService.get()).resolves.toEqual({ agentEnabled: false });

    mockedApi.get.mockResolvedValue({ data: { agent_enabled: 'true' } });
    await expect(featuresService.get()).resolves.toEqual({ agentEnabled: false });
  });
});
