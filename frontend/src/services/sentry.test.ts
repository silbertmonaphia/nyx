import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// The Sentry SDK is replaced wholesale so we can assert on init /
// setUser / captureException. ES-module namespace exports are
// read-only, so `vi.spyOn(Sentry, 'init')` would fail; mocking the
// module swaps the binding entirely so the boundary's Sentry calls
// land in vi.fn()s we control.
vi.mock('@sentry/react', () => ({
  init: vi.fn(),
  setUser: vi.fn(),
  captureException: vi.fn(),
  browserTracingIntegration: vi.fn(() => ({ name: 'BrowserTracing' })),
  replayIntegration: vi.fn(() => ({ name: 'Replay' })),
}));

import * as Sentry from '@sentry/react';

// `vi.resetModules()` in beforeEach ensures each test gets a
// fresh `sentry` module so we can re-evaluate the `initialized`
// flag against a freshly stubbed `import.meta.env`. Same pattern
// as telemetry.test.ts.
describe('sentry', () => {
  beforeEach(() => {
    vi.resetModules();
    vi.unstubAllEnvs();
    vi.mocked(Sentry.init).mockClear();
    vi.mocked(Sentry.setUser).mockClear();
    vi.mocked(Sentry.captureException).mockClear();
    vi.mocked(Sentry.browserTracingIntegration).mockClear();
    vi.mocked(Sentry.replayIntegration).mockClear();
  });

  afterEach(() => {
    vi.unstubAllEnvs();
    vi.restoreAllMocks();
  });

  it('initSentry returns false when VITE_SENTRY_DSN is unset', async () => {
    const { initSentry } = await import('./sentry');
    expect(initSentry()).toBe(false);
    expect(Sentry.init).not.toHaveBeenCalled();
  });

  it('initSentry returns false when VITE_SENTRY_DSN is empty string', async () => {
    vi.stubEnv('VITE_SENTRY_DSN', '');
    const { initSentry } = await import('./sentry');
    expect(initSentry()).toBe(false);
    expect(Sentry.init).not.toHaveBeenCalled();
  });

  it('initSentry calls Sentry.init with the expected shape when DSN is set', async () => {
    vi.stubEnv('VITE_SENTRY_DSN', 'https://public@sentry.io/1');
    vi.stubEnv(
      'VITE_API_URL',
      'https://api.nyx.com/api',
    );
    const { initSentry } = await import('./sentry');
    expect(initSentry()).toBe(true);

    expect(Sentry.init).toHaveBeenCalledTimes(1);
    const call = Sentry.init.mock.calls[0];
    const config = call[0];
    expect(config.dsn).toBe('https://public@sentry.io/1');
    expect(config.sendDefaultPii).toBe(false);
    expect(config.tracesSampleRate).toBe(1.0);
    expect(config.replaysSessionSampleRate).toBe(0);
    expect(config.replaysOnErrorSampleRate).toBe(1.0);
    // browserTracingIntegration + replayIntegration both fire.
    expect(Sentry.browserTracingIntegration).toHaveBeenCalledTimes(1);
    expect(Sentry.replayIntegration).toHaveBeenCalledTimes(1);
    expect(Array.isArray(config.integrations)).toBe(true);
    expect((config.integrations as unknown[]).length).toBe(2);
    // tracePropagationTargets covers same-origin /api/* plus the
    // API base host. The exact host regex/string is opaque to the
    // test, but the array length should be at least 2.
    const targets = (
      Sentry.browserTracingIntegration.mock.calls[0][0] as {
        tracePropagationTargets: unknown[];
      }
    ).tracePropagationTargets;
    expect(targets.length).toBeGreaterThanOrEqual(2);
  });

  it('initSentry is idempotent (never re-inits)', async () => {
    vi.stubEnv('VITE_SENTRY_DSN', 'https://public@sentry.io/1');
    const { initSentry } = await import('./sentry');
    initSentry();
    initSentry();
    expect(Sentry.init).toHaveBeenCalledTimes(1);
  });

  it('initSentry swallows Sentry.init errors and returns false', async () => {
    vi.stubEnv('VITE_SENTRY_DSN', 'https://public@sentry.io/1');
    vi.mocked(Sentry.init).mockImplementationOnce(() => {
      throw new Error('boom');
    });
    const { initSentry } = await import('./sentry');
    // Never throws — degrades to a noop. Same contract as initTelemetry.
    // Asserting the return value directly covers both:
    //   - "it didn't throw" (a throw would surface as an error from
    //     expect(), not as a value mismatch)
    //   - "it signalled not-initialised" so callers can gate
    //     downstream calls (setSentryUser, captureSentryException)
    //     on the return value.
    // A future change that returns `true` after a swallowed error
    // fails this test.
    expect(initSentry()).toBe(false);
  });

  it('setSentryUser is a noop when Sentry is not initialised', async () => {
    const { setSentryUser } = await import('./sentry');
    setSentryUser({ id: 1, username: 'tester' });
    expect(Sentry.setUser).not.toHaveBeenCalled();
  });

  it('setSentryUser forwards the user object when initialised', async () => {
    vi.stubEnv('VITE_SENTRY_DSN', 'https://public@sentry.io/1');
    const { initSentry, setSentryUser } = await import('./sentry');
    initSentry();
    setSentryUser({ id: 42, username: 'alice' });
    expect(Sentry.setUser).toHaveBeenCalledWith({
      id: 42,
      username: 'alice',
    });
  });

  it('setSentryUser(null) clears the user context when initialised', async () => {
    vi.stubEnv('VITE_SENTRY_DSN', 'https://public@sentry.io/1');
    const { initSentry, setSentryUser } = await import('./sentry');
    initSentry();
    setSentryUser(null);
    expect(Sentry.setUser).toHaveBeenCalledWith(null);
  });

  it('captureSentryException is a noop when Sentry is not initialised', async () => {
    const { captureSentryException } = await import('./sentry');
    captureSentryException(new Error('boom'));
    expect(Sentry.captureException).not.toHaveBeenCalled();
  });

  it('captureSentryException forwards error + context when initialised', async () => {
    vi.stubEnv('VITE_SENTRY_DSN', 'https://public@sentry.io/1');
    const { initSentry, captureSentryException } = await import('./sentry');
    initSentry();
    const err = new Error('boom');
    captureSentryException(err, { tags: { http_status: 500 } });
    expect(Sentry.captureException).toHaveBeenCalledWith(err, {
      tags: { http_status: 500 },
    });
  });

  it('captureSentryException never throws when Sentry.captureException throws', async () => {
    vi.stubEnv('VITE_SENTRY_DSN', 'https://public@sentry.io/1');
    vi.mocked(Sentry.captureException).mockImplementationOnce(() => {
      throw new Error('transport down');
    });
    const { captureSentryException } = await import('./sentry');
    expect(() => captureSentryException(new Error('boom'))).not.toThrow();
  });
});