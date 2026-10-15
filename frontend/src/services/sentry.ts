/// <reference types="vite/client" />

import * as Sentry from '@sentry/react';

let initialized = false;

/**
 * Initialize Sentry for the browser SPA. Reads `VITE_SENTRY_DSN` at
 * build time (Vite inlines `import.meta.env` values into the bundle);
 * when unset or empty, this is a noop so the bundle pays no runtime
 * cost and no Sentry script loads.
 *
 * Mirrors the gate shape at `telemetry.ts:30-34`: DSN presence is
 * the boolean, not a separate `enabled` flag — empty string and
 * `undefined` both turn Sentry off. Returns `true` only when Sentry
 * was actually wired up so callers can gate subsequent calls.
 *
 * `tracePropagationTargets` scopes which outgoing requests get
 * `sentry-trace` + `baggage` headers. axios hits `VITE_API_URL` (or
 * `http://localhost:8080/api` as the dev default at
 * `services/api.ts:10`) cross-origin in production, plus same-origin
 * `/api/*` paths the SPA may still hit during dev. We mirror both so
 * Sentry can stitch the user-initiated trace to the backend span.
 */
export function initSentry(): boolean {
  if (initialized) return true;
  const dsn = import.meta.env.VITE_SENTRY_DSN;
  if (!dsn) {
    return false;
  }

  try {
    const apiBaseUrl =
      import.meta.env.VITE_API_URL || 'http://localhost:8080/api';
    let apiOrigin: string | null = null;
    try {
      apiOrigin = new URL(apiBaseUrl).origin;
    } catch {
      // Relative baseURL or malformed env — skip the cross-origin
      // target. Same-origin `/^\/api\//` still covers it.
    }

    // tracesSampleRate 1.0 matches the backend's ParentBased(AlwaysSample)
    // for the first slice. Sampling can be dialled down once free-tier
    // cost is visible — see FUTURE_FRONTEND.md §11 (post-rollout).
    Sentry.init({
      dsn,
      integrations: [
        Sentry.browserTracingIntegration({
          tracePropagationTargets: [
            /^\/api\//,
            ...(apiOrigin ? [apiOrigin] : []),
          ],
        }),
        Sentry.replayIntegration(),
      ],
      tracesSampleRate: 1.0,
      // Session replay: only record when an error fires (not every
      // session). replaysSessionSampleRate=0 + replaysOnErrorSampleRate=1
      // bounds replay storage cost on the free tier.
      replaysSessionSampleRate: 0,
      replaysOnErrorSampleRate: 1.0,
      // Never ship PII (cookies, IP, user-agent) by default. The
      // explicit `setUser` calls below opt in the user id / username
      // at login time.
      sendDefaultPii: false,
      // release: deferred — CI plumbing tracked separately (see FUTURE_FRONTEND.md §9).
    });
    initialized = true;
    return true;
  } catch (err) {
    // Never let Sentry kill the app — degrade to noop.
    console.warn('Sentry init failed:', err);
    return false;
  }
}

/**
 * Tag the active Sentry session with a user. Mirrors the fields the
 * login / refresh wire envelope resolves with (id, username).
 * Pass `null` to clear (logout). Noop when Sentry wasn't initialised
 * — call sites don't need to gate the call.
 */
export function setSentryUser(
  user: { id: number | string; username: string } | null,
): void {
  if (!initialized) return;
  Sentry.setUser(user);
}

/**
 * Forward an exception to Sentry. The caller passes the raw error so
 * Sentry gets the full stack + AxiosError shape (Sentry does its own
 * PII scrubbing); structured extras (request id, sanitised payload,
 * component stack) ride on the second arg.
 *
 * Noop when Sentry wasn't initialised — call sites don't need to
 * gate the call. Never throws.
 */
export function captureSentryException(
  error: unknown,
  context?: Sentry.CaptureContext,
): void {
  if (!initialized) return;
  try {
    Sentry.captureException(error, context);
  } catch {
    // Sentry's transport can throw (network down, quota hit) — never
    // let an observability call sink the boundary it's guarding.
  }
}