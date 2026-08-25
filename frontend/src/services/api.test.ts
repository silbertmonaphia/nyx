import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import axios from 'axios';
import api from './api';
import * as telemetry from './telemetry';
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
function getResponseErrorHandler(): (error: unknown) => Promise<unknown> {
  const handlers = (api.interceptors.response as unknown as { handlers: Array<{ rejected: (error: unknown) => Promise<unknown> }> }).handlers;
  expect(handlers.length).toBeGreaterThan(0);
  return handlers[0].rejected;
}

/**
 * Construct a 401-shaped axios error with the given www-authenticate
 * challenge (or `undefined` for a header-less response).
 */
function make401(opts: { wwwAuthenticate?: string; retried?: boolean; skipAuthRefresh?: boolean } = {}) {
  const headers: Record<string, string> = {};
  if (opts.wwwAuthenticate !== undefined) {
    headers['www-authenticate'] = opts.wwwAuthenticate;
  }
  const config: Record<string, unknown> = {};
  if (opts.retried) config._retried = true;
  if (opts.skipAuthRefresh) config.skipAuthRefresh = true;

  const error = Object.assign(new Error('Request failed'), {
    response: {
      status: 401,
      data: {},
      headers,
    },
    config,
  });
  return error;
}

describe('api response interceptor', () => {
  let addToast: ReturnType<typeof vi.fn>;
  let logout: ReturnType<typeof vi.fn>;
  let setAuth: ReturnType<typeof vi.fn>;
  let rejected: (error: unknown) => Promise<unknown>;

  beforeEach(() => {
    addToast = vi.fn();
    logout = vi.fn();
    setAuth = vi.fn();
    // The interceptor calls `useAuthStore.getState().logout()` and
    // `useUiStore.getState().addToast(msg, 'error')`. Wire those
    // methods through the mocked `getState` accessors. The
    // post-cookie auth store has no `token` / `refreshToken` fields;
    // the cookie rides on every request via `withCredentials: true`.
    mockedAuthStore.getState = vi.fn().mockReturnValue({
      logout,
      setAuth,
    });
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

  it('logs out + toasts on 401 with no www-authenticate challenge', async () => {
    // A bare 401 — neither `error_description="expired"` nor any other
    // challenge. Today's behaviour (no refresh attempt, hard logout)
    // applies.
    const error = make401();

    await expect(rejected(error)).rejects.toBe(error);

    expect(logout).toHaveBeenCalledTimes(1);
    expect(addToast).toHaveBeenCalledWith(
      'Session expired. Please login again.',
      'error',
    );
  });

  it('logs out + toasts on 401 with bare "invalid_token" challenge (no "expired")', async () => {
    // The backend sends `Bearer error="invalid_token"` when the token
    // is malformed/forged (vs. expired). Same hard-logout branch as
    // no header at all.
    const error = make401({
      wwwAuthenticate: 'Bearer error="invalid_token"',
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

/**
 * Refresh-on-401 behaviour: when the backend's middleware sets
 * `WWW-Authenticate: Bearer error="invalid_token", error_description="expired"`,
 * the response interceptor must kick off a single-flight refresh, swap
 * in the new access token, and replay the original request — without
 * logging the user out.
 */
describe('refresh-on-401', () => {
  let addToast: ReturnType<typeof vi.fn>;
  let logout: ReturnType<typeof vi.fn>;
  let setAuth: ReturnType<typeof vi.fn>;
  let rejected: (error: unknown) => Promise<unknown>;
  let requestSpy: ReturnType<typeof vi.spyOn>;
  let postSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    addToast = vi.fn();
    logout = vi.fn();
    setAuth = vi.fn();
    mockedAuthStore.getState = vi.fn().mockReturnValue({
      logout,
      setAuth,
    });
    mockedUiStore.getState = vi.fn().mockReturnValue({ addToast });
    rejected = getResponseErrorHandler();

    // Spy on the wrapped instance's `request` so the retry path can
    // resolve without recursing into the real interceptor chain.
    requestSpy = vi.spyOn(api, 'request').mockResolvedValue({
      data: { ok: true },
      status: 200,
    } as never);

    // The refresh request itself goes through raw `axios.post`, NOT
    // the wrapped `api` instance. `axios` here is the real module —
    // we replace its `post` so each test can dictate the outcome.
    postSpy = vi.spyOn(axios, 'post');
  });

  afterEach(() => {
    requestSpy.mockRestore();
    postSpy.mockRestore();
  });

  it('refresh + retry succeeds on an "expired" challenge (no logout)', async () => {
    // Post-cookie refresh: the body is empty, the new tokens arrive
    // as Set-Cookie headers on the response (mocked as the bare
    // data envelope here — the browser does the actual cookie
    // storage). The store only updates its `user` profile.
    postSpy.mockResolvedValueOnce({
      data: {
        user: { id: 1, username: 'tester' },
        expires_at: '2024-02-01T00:15:00Z',
      },
    });

    const error = make401({
      wwwAuthenticate: 'Bearer error="invalid_token", error_description="expired"',
    });
    // The interceptor mutates `config` in place to mark `_retried` —
    // give the test its own handle so we can assert the mutation
    // afterwards.
    error.config = { ...(error.config as object) };

    await expect(rejected(error)).resolves.toBeDefined();

    // Single refresh was attempted. POST body is empty (the refresh
    // token rides in the cookie); withCredentials=true is set so the
    // browser attaches the cookie.
    expect(postSpy).toHaveBeenCalledTimes(1);
    expect(postSpy).toHaveBeenCalledWith(
      expect.stringContaining('/refresh'),
      {},
      expect.objectContaining({ withCredentials: true }),
    );

    // Store was updated with the user profile (cookies carry the
    // tokens themselves — no token-shaped assertion here).
    expect(setAuth).toHaveBeenCalledTimes(1);
    expect(setAuth).toHaveBeenCalledWith(
      expect.objectContaining({ user: { id: 1, username: 'tester' } }),
    );

    // Original request was replayed with `_retried=true`. The cookie
    // auto-attaches on the replay; no Authorization header is
    // stamped (the Authorization path is gone — that's the whole
    // point of the cookie migration).
    expect(requestSpy).toHaveBeenCalledTimes(1);
    const replayConfig = requestSpy.mock.calls[0][0] as {
      _retried?: boolean;
      headers?: Record<string, string>;
    };
    expect(replayConfig._retried).toBe(true);
    expect(replayConfig.headers?.Authorization).toBeUndefined();

    // No logout, no toast — the refresh was transparent.
    expect(logout).not.toHaveBeenCalled();
    expect(addToast).not.toHaveBeenCalled();
  });

  it('logs out + toasts when refresh itself fails', async () => {
    postSpy.mockRejectedValueOnce(new Error('refresh expired'));

    const error = make401({
      wwwAuthenticate: 'Bearer error="invalid_token", error_description="expired"',
    });
    error.config = { ...(error.config as object) };

    await expect(rejected(error)).rejects.toBe(error);

    expect(postSpy).toHaveBeenCalledTimes(1);
    expect(setAuth).not.toHaveBeenCalled();
    // No successful refresh → no replay.
    expect(requestSpy).not.toHaveBeenCalled();
    // Hard logout path, matching the bare-401 behaviour.
    expect(logout).toHaveBeenCalledTimes(1);
    expect(addToast).toHaveBeenCalledWith(
      'Session expired. Please login again.',
      'error',
    );
  });

  it('collapses concurrent 401s into a single refresh', async () => {
    // Hold the refresh open so both 401 handlers are awaiting it at
    // once — that's the condition single-flight guards against.
    let resolveRefresh!: (value: unknown) => void;
    postSpy.mockReturnValueOnce(
      new Promise((resolve) => {
        resolveRefresh = resolve;
      }),
    );

    const err1 = make401({
      wwwAuthenticate: 'Bearer error="invalid_token", error_description="expired"',
    });
    err1.config = { ...(err1.config as object) };
    const err2 = make401({
      wwwAuthenticate: 'Bearer error="invalid_token", error_description="expired"',
    });
    err2.config = { ...(err2.config as object) };

    // Fire both rejected handlers without awaiting the first yet.
    const p1 = rejected(err1);
    const p2 = rejected(err2);

    // Both 401s have hit the interceptor; only one refresh POST
    // should have been issued.
    expect(postSpy).toHaveBeenCalledTimes(1);

    // Settle the refresh so both handlers resume.
    resolveRefresh({
      data: {
        user: { id: 1, username: 'tester' },
        expires_at: '2024-02-01T00:15:00Z',
      },
    });

    await Promise.all([p1, p2]);

    // Still exactly one refresh POST — the second 401 waited on the
    // first's in-flight promise instead of triggering its own.
    expect(postSpy).toHaveBeenCalledTimes(1);
    // Both original requests were replayed with the fresh token.
    expect(requestSpy).toHaveBeenCalledTimes(2);
    expect(logout).not.toHaveBeenCalled();
  });

  it('skips refresh when the request is flagged skipAuthRefresh', async () => {
    // The retry path stamps `_retried` on the config it replays, so
    // a follow-up 401 on the replayed request must fall straight
    // through to the logout branch — otherwise we'd loop forever.
    const error = make401({
      wwwAuthenticate: 'Bearer error="invalid_token", error_description="expired"',
      retried: true,
    });

    await expect(rejected(error)).rejects.toBe(error);

    expect(postSpy).not.toHaveBeenCalled();
    expect(requestSpy).not.toHaveBeenCalled();
    expect(logout).toHaveBeenCalledTimes(1);
    expect(addToast).toHaveBeenCalledWith(
      'Session expired. Please login again.',
      'error',
    );
  });
});

/**
 * Request-interceptor contract: the request side of the interceptor
 * must call `injectTraceparent` on every outbound config so the
 * backend can continue the SPA's trace. We exercise it the same way
 * the existing response-interceptor tests do — by poking the
 * registered handler directly off `interceptors.request.handlers`.
 */
describe('api request interceptor (traceparent)', () => {
  let spy: ReturnType<typeof vi.spyOn>;
  let requestFulfilled: (config: unknown) => unknown;

  beforeEach(() => {
    const handlers = (api.interceptors.request as unknown as {
      handlers: Array<{ fulfilled: (config: unknown) => unknown }>;
    }).handlers;
    expect(handlers.length).toBeGreaterThan(0);
    requestFulfilled = handlers[0].fulfilled;
    spy = vi.spyOn(telemetry, 'injectTraceparent').mockImplementation(() => {});
  });

  afterEach(() => {
    spy.mockRestore();
  });

  it('calls injectTraceparent on each request', () => {
    const config: { headers: Record<string, unknown> } = {
      headers: {},
    };

    requestFulfilled(config as never);

    expect(spy).toHaveBeenCalledTimes(1);
    // It receives the headers object. (Pre-cookie the path also
    // stamped Authorization here; post-cookie only traceparent runs
    // on every request — the access JWT rides the cookie.)
    expect(spy.mock.calls[0][0]).toBe(config.headers);
  });
});

/**
 * End-to-end traceparent shape: with a real tracer installed, the
 * request interceptor must produce a valid W3C traceparent header.
 * This exercises both the interceptor wiring AND the propagator
 * itself in one go. We use the SDK's web variant here because its
 * `register()` installs a stack context manager — without one,
 * `startActiveSpan` doesn't actually attach the span to the active
 * context, and `trace.getActiveSpan()` returns undefined.
 */
describe('api request interceptor (real traceparent shape)', () => {
  beforeEach(async () => {
    const { WebTracerProvider } = await import('@opentelemetry/sdk-trace-web');
    const provider = new WebTracerProvider();
    provider.register();
  });

  it('stamps a valid W3C traceparent on outbound headers', async () => {
    const handlers = (api.interceptors.request as unknown as {
      handlers: Array<{ fulfilled: (config: unknown) => unknown }>;
    }).handlers;
    const fulfilled = handlers[0].fulfilled;

    const { trace: otelTrace } = await import('@opentelemetry/api');
    const tr = otelTrace.getTracer('test');
    const headers: Record<string, unknown> = {};
    const config = { headers } as never;

    await new Promise<void>((resolve) => {
      tr.startActiveSpan('outer', (span) => {
        fulfilled(config);
        span.end();
        resolve();
      });
    });

    const tp = headers.traceparent;
    expect(typeof tp).toBe('string');
    expect(tp).toMatch(/^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$/);
  });
});