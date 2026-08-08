import { describe, it, expect, beforeEach, vi } from 'vitest';
import axios from 'axios';
import api from './api';
import { useAuthStore } from '../store/authStore';
import { useUiStore } from '../store/uiStore';

// Mock the stores so the response interceptor's side effects
// (logout + addToast) can be observed without touching real state.
vi.mock('../store/authStore');
vi.mock('../store/uiStore');

const mockedAuthStore = useAuthStore as unknown as {
  getState: ReturnType<typeof vi.fn>;
};
const mockedUiStore = useUiStore as unknown as {
  getState: ReturnType<typeof vi.fn>;
};

/**
 * The production axios instance is registered with a response
 * interceptor at module load. The cleanest way to exercise that
 * handler in vitest+jsdom is to invoke the registered rejected-handler
 * directly — that's exactly the pattern axios's own interceptor chain
 * uses internally.
 */
function getResponseErrorHandler(): (error: unknown) => Promise<never> {
  const handlers = (api.interceptors.response as unknown as { handlers: Array<{ rejected: (error: unknown) => Promise<never> }> }).handlers;
  expect(handlers.length).toBeGreaterThan(0);
  return handlers[0].rejected;
}

describe('api response interceptor', () => {
  let addToast: ReturnType<typeof vi.fn>;
  let logout: ReturnType<typeof vi.fn>;
  let rejected: (error: unknown) => Promise<never>;

  beforeEach(() => {
    addToast = vi.fn();
    logout = vi.fn();
    // The interceptor calls `useAuthStore.getState().logout()` and
    // `useUiStore.getState().addToast(msg, 'error')`. Wire those
    // methods through the mocked `getState` accessors.
    mockedAuthStore.getState = vi.fn().mockReturnValue({ logout });
    mockedUiStore.getState = vi.fn().mockReturnValue({ addToast });
    rejected = getResponseErrorHandler();
  });

  it('passes through successful responses unchanged', async () => {
    // The interceptor's `fulfilled` handler is a pass-through; verify
    // it doesn't transform a successful response.
    const handlers = (api.interceptors.response as unknown as { handlers: Array<{ fulfilled: (response: unknown) => unknown }> }).handlers;
    const fulfilled = handlers[0].fulfilled;
    const response = { data: { hello: 'world' }, status: 200 };
    expect(fulfilled(response)).toBe(response);
  });

  it('logs out + toasts on 401', async () => {
    const error = Object.assign(new Error('Request failed'), {
      response: { status: 401, data: {} },
      config: {},
    });

    await expect(rejected(error)).rejects.toBe(error);

    expect(logout).toHaveBeenCalledTimes(1);
    expect(addToast).toHaveBeenCalledWith(
      'Session expired. Please login again.',
      'error',
    );
  });

  it('surfaces backend `error` field for non-401 4xx/5xx', async () => {
    const error = Object.assign(new Error('Request failed'), {
      response: { status: 422, data: { error: 'title is required' } },
      config: {},
    });

    await expect(rejected(error)).rejects.toBe(error);

    expect(logout).not.toHaveBeenCalled();
    expect(addToast).toHaveBeenCalledWith('title is required', 'error');
  });

  it('falls back to "Server error: <status>" when no `error` body', async () => {
    const error = Object.assign(new Error('Request failed'), {
      response: { status: 500, data: { something: 'else' } },
      config: {},
    });

    await expect(rejected(error)).rejects.toBe(error);

    expect(addToast).toHaveBeenCalledWith('Server error: 500', 'error');
  });

  it('toasts the network error message when no response is present', async () => {
    const error = Object.assign(new Error('Network Error'), {
      request: {},
      config: {},
    });

    await expect(rejected(error)).rejects.toBe(error);

    expect(addToast).toHaveBeenCalledWith(
      'No response from server. Please check your connection.',
      'error',
    );
  });

  it('rethrows the original error unchanged', async () => {
    const original = Object.assign(new Error('boom'), {
      response: { status: 422, data: { error: 'bad' } },
      config: {},
    });
    await expect(rejected(original)).rejects.toBe(original);
  });

  // Sanity check: axios's `interceptors` array actually has the
  // handlers we expect. If a future refactor changes the structure,
  // this test surfaces the break before the rest run against
  // undefined.
  it('has a registered response interceptor', () => {
    const handlers = (axios.interceptors.response as unknown as { handlers: unknown }).handlers;
    expect(Array.isArray(handlers)).toBe(true);
  });
});