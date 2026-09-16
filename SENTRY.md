# Sentry (frontend)

Browser-side error, performance, and session-replay sink for the Nyx SPA. Wired via `@sentry/react` in `frontend/src/services/sentry.ts`; init is gated on the literal presence of `VITE_SENTRY_DSN` (empty string + unset both turn Sentry off — no SDK script loads, no runtime cost). Backend stays on OTel→Jaeger; this file covers the SPA only.

## Config

Set the four env vars in `.env` (see `.env.example`):

- `VITE_SENTRY_DSN` — public DSN from the Sentry project (Settings → Client Keys). Inlined into the bundle at build time.
- `SENTRY_AUTH_TOKEN` — build-time only; consumed by `@sentry/vite-plugin` to upload sourcemaps. Never shipped to the bundle.
- `SENTRY_ORG` — Sentry organisation slug.
- `SENTRY_PROJECT` — Sentry project slug (`nyx-frontend` is the default).

Leave the three build-time vars empty for local dev builds without sourcemap upload — the Vite plugin only registers when all three are set, so an empty token is a noop rather than an error.

> **Lockfile sync:** editing `frontend/package.json` (e.g. bumping `@sentry/react`) requires regenerating `frontend/package-lock.json` — the Dockerfile's `npm ci` rejects an out-of-sync lockfile and the next `docker compose build frontend` fails. The pre-commit lint-staged hook does this automatically when it sees `frontend/package.json` staged. See CLAUDE.md for the manual command.

## Self-hosted Sentry

Operators running a self-hosted Sentry instance override two things in `docker-compose.yml` / `docker-compose.prod.yml`:

- `VITE_SENTRY_DSN` — point at your self-hosted DSN.
- `SENTRY_CSP_ORIGIN` — build-arg fed into the nginx `connect-src` directive via the `__SENTRY_CSP_ORIGIN__` placeholder. Default is the SaaS ingest host list (`https://*.ingest.sentry.io https://*.sentry.io`). Override with your self-hosted origin (e.g. `https://sentry.example.com`) or the browser refuses every envelope POST and Sentry silently noops in production.

## Sample rates

`tracesSampleRate: 1.0`, `replaysSessionSampleRate: 0`, `replaysOnErrorSampleRate: 1.0` — match the backend's `ParentBased(AlwaysSample)` for the first slice and bound replay storage cost. On the Developer plan (5K errors / 50 replays / 5M spans), dial `tracesSampleRate` down to `0.1` if spans burn through the monthly budget faster than expected. Replays already only fire on errors, so the replay budget is safe at the default.

## Release tagging

Deferred. `release: commitSha` is intentionally unset on `Sentry.init`; the CI plumbing that would stamp each release with the commit SHA is tracked separately (`FUTURE_FRONTEND.md` §9).