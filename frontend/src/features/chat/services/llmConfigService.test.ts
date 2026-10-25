import { describe, it, expect, beforeEach, vi } from 'vitest';
import api from '~/services/api';
import { llmConfigService } from './llmConfigService';
import type { LlmConfigStatus } from '../types/llmConfig';

vi.spyOn(api, 'get');
vi.spyOn(api, 'put');
vi.spyOn(api, 'delete');

const mockedGet = api.get as unknown as ReturnType<typeof vi.fn>;
const mockedPut = api.put as unknown as ReturnType<typeof vi.fn>;
const mockedDelete = api.delete as unknown as ReturnType<typeof vi.fn>;

const unconfiguredBody = {
  configured: false,
  llm: null,
  embedding: null,
  tavily: null,
  fallback: null,
};

const configuredBody = {
  configured: true,
  llm: {
    base_url: 'https://api.deepseek.com/v1',
    model: 'deepseek-chat',
    api_key_masked: '••••abcd',
  },
  embedding: {
    base_url: 'http://localhost:8000/v1',
    model: 'Qwen/Qwen3-Embedding-0.6B',
    api_key_masked: '••••ef12',
  },
  tavily: {
    base_url: 'https://api.tavily.com/search',
    api_key_masked: '••••tvly',
  },
  fallback: {
    base_url: 'https://api.openai.com/v1',
    model: 'gpt-4o-mini',
    api_key_masked: '••••f9f9',
  },
};

const updateInput = {
  llm: {
    base_url: 'https://api.deepseek.com/v1',
    api_key: '',
    model: 'deepseek-chat',
  },
  embedding: {
    base_url: 'http://localhost:8000/v1',
    api_key: 'sk-embedding',
    model: 'Qwen/Qwen3-Embedding-0.6B',
  },
  tavily: {
    base_url: 'https://api.tavily.com/search',
    api_key: 'tvly-key',
  },
  fallback: null,
};

describe('llmConfigService', () => {
  beforeEach(() => {
    mockedGet.mockReset();
    mockedPut.mockReset();
    mockedDelete.mockReset();
  });

  describe('get', () => {
    it('GETs /me/llm-config and returns an unconfigured status for null sections', async () => {
      mockedGet.mockResolvedValueOnce({ data: unconfiguredBody });

      const status = await llmConfigService.get();

      expect(mockedGet).toHaveBeenCalledWith('/me/llm-config', {
        skipSentry: true,
        skipErrorToast: false,
      });
      expect(status).toEqual<LlmConfigStatus>({
        configured: false,
        llm: null,
        embedding: null,
        tavily: null,
        fallback: null,
      });
    });

    it('returns the masked sections of a configured row', async () => {
      mockedGet.mockResolvedValueOnce({ data: configuredBody });

      const status = await llmConfigService.get();

      expect(status.configured).toBe(true);
      expect(status.llm).toEqual(configuredBody.llm);
      expect(status.embedding).toEqual(configuredBody.embedding);
      expect(status.tavily).toEqual(configuredBody.tavily);
      expect(status.fallback).toEqual(configuredBody.fallback);
      expect(Object.keys(status.llm ?? {})).not.toContain('api_key');
    });

    it('coerces malformed sections to null instead of throwing', async () => {
      mockedGet.mockResolvedValueOnce({
        data: {
          configured: true,
          llm: 'garbage',
          embedding: 42,
          tavily: null,
          fallback: { base_url: 7, model: undefined, api_key_masked: 'ok' },
        },
      });

      const status = await llmConfigService.get();

      expect(status.llm).toBeNull();
      expect(status.embedding).toBeNull();
      expect(status.tavily).toBeNull();
      expect(status.fallback).toEqual({
        base_url: '',
        model: '',
        api_key_masked: 'ok',
      });
    });

    it('treats a missing body as unconfigured', async () => {
      mockedGet.mockResolvedValueOnce({ data: undefined });

      const status = await llmConfigService.get();

      expect(status).toEqual(unconfiguredBody);
    });
  });

  describe('update', () => {
    it('PUTs the exact wire contract body to /me/llm-config', async () => {
      mockedPut.mockResolvedValueOnce({ data: configuredBody });

      await llmConfigService.update(updateInput);

      expect(mockedPut).toHaveBeenCalledTimes(1);
      expect(mockedPut).toHaveBeenCalledTimes(1);
      expect(mockedPut.mock.calls[0]?.[0]).toBe('/me/llm-config');
      expect(mockedPut.mock.calls[0]?.[1]).toEqual({
        llm: {
          base_url: 'https://api.deepseek.com/v1',
          api_key: '',
          model: 'deepseek-chat',
        },
        embedding: {
          base_url: 'http://localhost:8000/v1',
          api_key: 'sk-embedding',
          model: 'Qwen/Qwen3-Embedding-0.6B',
        },
        tavily: {
          base_url: 'https://api.tavily.com/search',
          api_key: 'tvly-key',
        },
        fallback: null,
      });
      expect(mockedPut.mock.calls[0]?.[2]).toEqual({
        skipSentry: true,
        skipErrorToast: false,
      });
    });

    it('returns the masked status the upsert answers with', async () => {
      mockedPut.mockResolvedValueOnce({ data: configuredBody });

      const status = await llmConfigService.update(updateInput);

      expect(status.configured).toBe(true);
      expect(status.llm?.api_key_masked).toBe('••••abcd');
    });

    it('sends a fully populated fallback group when one is provided', async () => {
      mockedPut.mockResolvedValueOnce({ data: configuredBody });

      await llmConfigService.update({
        ...updateInput,
        fallback: {
          base_url: 'https://api.openai.com/v1',
          api_key: '',
          model: 'gpt-4o-mini',
        },
      });

      expect(mockedPut.mock.calls[0]?.[1]).toMatchObject({
        fallback: {
          base_url: 'https://api.openai.com/v1',
          api_key: '',
          model: 'gpt-4o-mini',
        },
      });
    });
  });

  describe('remove', () => {
    it('DELETEs /me/llm-config', async () => {
      mockedDelete.mockResolvedValueOnce({ data: undefined, status: 204 });

      await llmConfigService.remove();

      expect(mockedDelete).toHaveBeenCalledWith('/me/llm-config', {
        skipSentry: true,
        skipErrorToast: false,
      });
    });
  });
});
