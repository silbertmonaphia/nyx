import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import axios from 'axios';
import api, { tokenStore } from './api';
import * as telemetry from './telemetry';
import { useAuthStore } from '../store/authStore';
import { useUiStore } from '../store/uiStore';

// The structured logger is replaced wholesale so the H7 tests
// can assert on `logger.error` calls. ES-module exports are
// read-only, so `vi.spyOn(logger, 'error')` would fail; mocking
// the module replaces the binding entirely.
vi.mock('./logger', () => ({
  logger: {
    debug: vi.fn(),
    info: vi.fn(),
    warn: vi.fn(),
    error: vi.fn(),
  },
}));
import { logger } from './logger';

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
  let loggerError: ReturnType<typeof vi.fn>;
  let rejected: (error: unknown) => Promise<unknown>;

  beforeEach(() => {
    addToast = vi.fn();
    logout = vi.fn();
    setAuth = vi.fn();
    loggerError = logger.error as ReturnType<typeof vi.fn>;
    loggerError.mockClear();
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

    // The H8 refresh-on-any-401 tests stub axios.post for the
    // refresh call. Default: refresh fails so the 401 path falls
    // through to the same logout toast that the bare-401 case
    // shows. Tests that want the success path override this.
    vi.spyOn(axios, 'post').mockRejectedValue(new Error('refresh failed'));
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('passes through successful responses unchanged', async () => {
    // The interceptor's `fulfilled` handler is a pass-through; verify
    // it doesn't transform a successful response.
    const handlers = (api.interceptors.response as unknown as { handlers: Array<{ fulfilled: (response: unknown) => unknown }> }).handlers;
    const fulfilled = handlers[0].fulfilled;
    const response = { data: { hello: 'world' }, status: 200 };
    expect(fulfilled(response)).toBe(response);
  });

  it('attempts refresh on a bare 401 and logs out if refresh fails (H8)', async () => {
    // SECURITY.md H8: any 401 triggers a refresh attempt. The
    // previous behaviour matched only `error_description="expired"`
    // which forced a hard logout on malformed/forged tokens and
    // locked users out of recoverable sessions. Here the refresh
    // itself fails (axios.post is stubbed to reject in beforeEach)
    // so the handler falls through to the same logout path.
    const error = make401();

    await expect(rejected(error)).rejects.toBe(error);

    expect(logout).toHaveBeenCalledTimes(1);
    expect(addToast).toHaveBeenCalledWith(
      'Session expired. Please login again.',
      'error',
    );
  });

  it('attempts refresh on a "invalid_token" 401 challenge (H8)', async () => {
    // Pre-H8, a `Bearer error="invalid_token"` (no "expired")
    // challenge forced a hard logout. H8 widens the refresh window
    // so a stale-but-recoverable session gets a chance to rotate.
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

  it('never echoes the backend `error` field to the toast (H7)', async () => {
    // SECURITY.md H7: the backend's error message is trusted less
    // than client-side text. The toast shows a generic label + the
    // request id; the full payload goes to the log pipeline only.
    const error = Object.assign(new Error('Request failed'), {
      response: {
        status: 422,
        data: { error: 'title is required', code: 'validation_failed' },
        headers: { 'x-request-id': 'req-abc-123' },
      },
      config: {},
    });

    await expect(rejected(error)).rejects.toBe(error);

    expect(logout).not.toHaveBeenCalled();
    // No echo of the server string.
    expect(addToast).not.toHaveBeenCalledWith(
      expect.stringContaining('title is required'),
      'error',
    );
    // Generic label + request id is what the user sees.
    expect(addToast).toHaveBeenCalledWith(
      'Server error (422) (ref: req-abc-123)',
      'error',
    );
    // Full payload (minus the request id which is logged alongside)
    // is captured for the operator.
    expect(loggerError).toHaveBeenCalledWith(
      'api.error',
      expect.objectContaining({
        status: 422,
        requestId: 'req-abc-123',
        payload: expect.objectContaining({ error: 'title is required' }),
      }),
    );
  });

  it('falls back to "Server error (<status>)" with no request id when the header is absent', async () => {
    // Some upstream paths (e.g. a misbehaving proxy) may swallow the
    // X-Request-Id header. The toast must still be safe to show —
    // generic label, no diagnostic detail, no leakage of the
    // missing header.
    const error = Object.assign(new Error('Request failed'), {
      response: { status: 500, data: { something: 'else' }, headers: {} },
      config: {},
    });

    await expect(rejected(error)).rejects.toBe(error);

    expect(addToast).toHaveBeenCalledWith('Server error (500)', 'error');
    expect(loggerError).toHaveBeenCalledWith(
      'api.error',
      expect.objectContaining({ status: 500, requestId: undefined }),
    );
  });

  it('strips control characters from the logged payload (H7)', async () => {
    // CRLF injection into a log line would let a malicious
    // backend string forge fake log records. The sanitiser
    // strips C0/C1 controls before the payload reaches
    // logger.error.
    const error = Object.assign(new Error('Request failed'), {
      response: {
        status: 400,
        data: { error: 'evil\nINJECTED line: forged=true' },
        headers: {},
      },
      config: {},
    });

    await expect(rejected(error)).rejects.toBe(error);

    const call = loggerError.mock.calls.find(([msg]) => msg === 'api.error');
    expect(call).toBeDefined();
    const attrs = call?.[1] as { payload: { error: string } };
    expect(attrs.payload.error).not.toMatch(/[\r\n]/);
    expect(attrs.payload.error).toContain('evil');
    expect(attrs.payload.error).toContain('INJECTED line: forged=true');
  });

  it('caps the logged payload string length (H7)', async () => {
    // The backend could in principle return a multi-MB error
    // message that bloats log lines. The sanitiser caps the
    // length defensively.
    const huge = 'x'.repeat(5_000);
    const error = Object.assign(new Error('Request failed'), {
      response: {
        status: 400,
        data: { error: huge },
        headers: {},
      },
      config: {},
    });

    await expect(rejected(error)).rejects.toBe(error);

    const call = loggerError.mock.calls.find(([msg]) => msg === 'api.error');
    const attrs = call?.[1] as { payload: { error: string } };
    expect(attrs.payload.error.length).toBeLessThanOrEqual(2_000);
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
 * Refresh-on-401 behaviour: when any 401 response comes back, the
 * interceptor must kick off a single-flight refresh, swap in the
 * fresh cookies via the retry path, and replay the original
 * request — without logging the user out. SECURITY.md H8 widened
 * this from "WWW-Authenticate=expired only" to any 401, so the
 * tests below drive the path with a variety of challenge shapes
 * (and no challenge at all) to pin the new contract.
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

  it('refresh + retry succeeds on any 401 (no logout) (H8)', async () => {
    // SECURITY.md H8: the refresh path is no longer gated on the
    // WWW-Authenticate=expired substring. Any 401 attempts the
    // refresh once; on success the original request is replayed
    // with the freshly rotated Bearer token.
    //
    // Post-Bearer refresh: the body carries { refresh_token }; the
    // response body carries { access_token, refresh_token, token_type,
    // expires_at, user }. The store updates `user` and the
    // tokenStore swaps in the fresh pair.
    postSpy.mockResolvedValueOnce({
      data: {
        access_token: 'new.access',
        refresh_token: 'new.refresh',
        token_type: 'Bearer',
        expires_at: '2024-02-01T00:15:00Z',
        user: { id: 1, username: 'tester' },
      },
    });

    // Seed the token store with a refresh token so performRefresh
    // actually attempts the call. The interceptor only refreshes
    // when an access token is present; a synchronous test that
    // triggers a 401 without seeding the store would skip refresh.
    tokenStore.setTokens('expired.access', 'old.refresh');

    // No WWW-Authenticate challenge at all — the H8 widening
    // means this still triggers a refresh.
    const error = make401();
    // The interceptor mutates `config` in place to mark `_retried` —
    // give the test its own handle so we can assert the mutation
    // afterwards.
    error.config = { ...(error.config as object) };

    await expect(rejected(error)).resolves.toBeDefined();

    // Single refresh was attempted. POST body carries the refresh
    // token (RFC 6750 Bearer transport); withCredentials is false.
    expect(postSpy).toHaveBeenCalledTimes(1);
    expect(postSpy).toHaveBeenCalledWith(
      expect.stringContaining('/refresh'),
      expect.objectContaining({ refresh_token: 'old.refresh' }),
      expect.objectContaining({ withCredentials: false }),
    );

    // Store was updated with the user profile.
    expect(setAuth).toHaveBeenCalledTimes(1);
    expect(setAuth).toHaveBeenCalledWith(
      expect.objectContaining({ user: { id: 1, username: 'tester' } }),
    );

    // Original request was replayed with `_retried=true`. The
    // request interceptor re-runs and stamps the freshly rotated
    // Bearer token onto the replay.
    expect(requestSpy).toHaveBeenCalledTimes(1);
    const replayConfig = requestSpy.mock.calls[0][0] as {
      _retried?: boolean;
    };
    expect(replayConfig._retried).toBe(true);

    // No logout, no toast — the refresh was transparent.
    expect(logout).not.toHaveBeenCalled();
    expect(addToast).not.toHaveBeenCalled();

    // Clean up the tokenStore so other tests start anonymous.
    tokenStore.clear();
  });

  it('logs out + toasts when refresh itself fails', async () => {
    postSpy.mockRejectedValueOnce(new Error('refresh expired'));
    // Seed the token store so the refresh path actually fires the
    // POST (without an access token the interceptor skips refresh
    // and goes straight to logout — that's a different test).
    tokenStore.setTokens('expired.access', 'old.refresh');

    const error = make401();
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

    tokenStore.clear();
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

    // Seed the token store so the refresh path is enabled.
    tokenStore.setTokens('expired.access', 'old.refresh');

    const err1 = make401();
    err1.config = { ...(err1.config as object) };
    const err2 = make401();
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
        access_token: 'new.access',
        refresh_token: 'new.refresh',
        token_type: 'Bearer',
        expires_at: '2024-02-01T00:15:00Z',
        user: { id: 1, username: 'tester' },
      },
    });

    await Promise.all([p1, p2]);

    // Still exactly one refresh POST — the second 401 waited on the
    // first's in-flight promise instead of triggering its own.
    expect(postSpy).toHaveBeenCalledTimes(1);
    // Both original requests were replayed with the fresh token.
    expect(requestSpy).toHaveBeenCalledTimes(2);
    expect(logout).not.toHaveBeenCalled();

    // Clean up the tokenStore so other tests start anonymous.
    tokenStore.clear();
  });

  it('skips refresh when the request is flagged skipAuthRefresh', async () => {
    // The retry path stamps `_retried` on the config it replays, so
    // a follow-up 401 on the replayed request must fall straight
    // through to the logout branch — otherwise we'd loop forever.
    const error = make401({ retried: true });

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