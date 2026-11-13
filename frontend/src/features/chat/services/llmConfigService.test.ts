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

vi.spyOn(api, 'post');
const mockedPost = api.post as unknown as ReturnType<typeof vi.fn>;

const capturedFromGoHandler = {
  slots: {
    chat: {
      probed: true,
      status: 'ok',
      http_status: 200,
      request: {
        method: 'POST',
        url: 'https://upstream.example/v1/chat/completions',
        headers: {
          Accept: 'text/event-stream',
          Authorization: 'Bearer ••••9876',
          'Cache-Control': 'no-cache',
          Connection: 'keep-alive',
          'Content-Type': 'application/json',
        },
        body: {
          model: 'm',
          messages: [{ role: 'user', content: 'nyx connectivity probe' }],
          max_completion_tokens: 1,
          stream: true,
          stream_options: { include_usage: true },
        },
      },
    },
    embedding: {
      probed: true,
      status: 'auth_failed',
      http_status: 401,
      request: {
        method: 'POST',
        url: 'https://upstream.example/v1/embeddings',
        headers: { Authorization: 'Bearer ••••9876', 'Content-Type': 'application/json' },
        body: { model: 'e', input: ['nyx connectivity probe'] },
      },
    },
    fallback: { probed: false },
    tavily: null,
  },
};

describe('llmConfigService.validate', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('round-trips the Go handler fixture through the TS consumer', async () => {
    mockedPost.mockResolvedValueOnce({ data: capturedFromGoHandler });
    const out = await llmConfigService.validate({ slots: ['chat', 'embedding'] });
    expect(mockedPost).toHaveBeenCalledWith(
      '/me/llm-config/validate',
      { slots: ['chat', 'embedding'] },
      { skipSentry: true, skipErrorToast: false },
    );
    expect(out.slots.chat?.probed).toBe(true);
    expect(out.slots.chat?.status).toBe('ok');
    expect(out.slots.chat?.http_status).toBe(200);
    expect(out.slots.chat?.request?.method).toBe('POST');
    expect(out.slots.chat?.request?.headers.Authorization).toBe('Bearer ••••9876');
    expect(out.slots.chat?.request?.body).toMatchObject({ model: 'm', stream: true });
    expect(out.slots.embedding?.status).toBe('auth_failed');
    expect(out.slots.embedding?.http_status).toBe(401);
    expect(out.slots.fallback?.probed).toBe(false);
    expect(out.slots.tavily).toBeNull();
  });

  it('renders a width mismatch that still carries its upstream 200', async () => {
    const widthMismatch = {
      slots: {
        chat: null,
        embedding: {
          probed: true,
          status: 'width_mismatch',
          http_status: 200,
          detail: 'expected 1024 dims, got 3',
          request: {
            method: 'POST',
            url: 'https://upstream.example/v1/embeddings',
            headers: { Authorization: 'Bearer ••••9876' },
            body: { model: 'e', input: ['nyx connectivity probe'] },
          },
        },
        fallback: null,
        tavily: null,
      },
    };
    mockedPost.mockResolvedValueOnce({ data: widthMismatch });
    const out = await llmConfigService.validate({ slots: ['embedding'] });
    expect(out.slots.embedding?.status).toBe('width_mismatch');
    expect(out.slots.embedding?.http_status).toBe(200);
    expect(out.slots.embedding?.detail).toContain('expected 1024 dims');
  });

  it('never lets a plaintext key through the parsed result', async () => {
    const withSecret = JSON.parse(JSON.stringify(capturedFromGoHandler));
    withSecret.slots.chat.request.headers.Authorization = 'Bearer ••••9876';
    mockedPost.mockResolvedValueOnce({ data: withSecret });
    const out = await llmConfigService.validate({});
    const wire = JSON.stringify(out);
    expect(wire).toContain('••••9876');
    expect(wire).not.toMatch(/sk-[a-z0-9-]{8,}/i);
  });
});
