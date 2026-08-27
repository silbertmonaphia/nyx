# Nyx Frontend: Architectural Evolution

The history and design decisions behind the current frontend. For the high-level "done / next" checklist, see `FUTURE.md` §4.

## 1. Core Architecture

- [x] **TypeScript** — strict mode (`.tsx` across components, typed API responses, typed Zustand stores).
- [x] **Feature-based folder structure** — domain-driven:
  ```
  frontend/src/
  ├── features/
  │   ├── movies/              # components, hooks, services, store, types
  │   └── auth/                # components
  ├── components/              # Shared UI primitives (Button, Card, Dialog, Input, Label, Textarea, ToastContainer)
  ├── services/                # axios client (api.ts)
  ├── store/                   # Zustand stores (authStore, uiStore, movieUiStore)
  ├── hooks/                   # Custom React hooks
  ├── types/                   # Cross-feature TypeScript types
  ├── utils/                   # Helpers (cn classnames merger)
  └── test/                    # Vitest setup (e.g. IntersectionObserver polyfill)
  ```

## 2. Data Fetching & Server State

- [x] **TanStack Query** — automatic caching, background refetch, pagination, `keepPreviousData` for smooth page transitions.
- [x] **Axios client** — `services/api.ts` with request interceptor (Bearer auth header + `traceparent`) and response interceptor (global 401 → single-flight refresh → retry, error message extraction, sanitised logging).
- [x] **Optimistic updates** — `useMovies.ts` uses `onMutate` / `onError` / `onSettled` for create / update / delete. Placeholders use negative IDs and are swapped when the server response arrives.
- [x] **Pagination** — `useMovies` exposes `page`, `pageSize`, `total`, `hasMore`, `isLoadingMore`, `loadMore`.
- [x] **Search Debounce** — `App.tsx` now feeds `useMovies` a 300ms-debounced term via the generic `useDebounce` hook in `src/hooks/`. The input stays bound to the raw value so typing is instant.

## 3. Form Management & Validation

- [x] **React Hook Form** — uncontrolled components; less re-render churn than `useState`-driven forms.
- [x] **Zod** — schemas live with the feature (`features/movies/types.ts`); `@hookform/resolvers/zod` wires them into `<form>`.

## 4. Styling & Design System

- [x] **Tailwind CSS v4** — `@tailwindcss/postcss` config; design tokens via `tailwind.config.js`.
- [x] **Radix UI + shadcn-style primitives** — `@radix-ui/react-dialog`, `@radix-ui/react-label`, `@radix-ui/react-slot` power accessible `Dialog`, `Label`, `Button asChild`. Components in `src/components/ui/` are owned (not installed) so we can tweak freely.
- [ ] **CSS Modules / Vanilla Extract** — for component-specific styles that need complex logic while keeping type safety. Currently Tailwind is sufficient; revisit if/when a design system has to grow.

## 5. Global State

- [x] **Zustand** — `authStore` (user, login, logout, register), `uiStore` (toasts), `movieUiStore` (filter UI). Lightweight, no provider boilerplate.
- [x] **Persist Auth Token Securely** — Bearer tokens (RFC 6750). `/api/login` / `/api/register` / `/api/refresh` return `{access_token, refresh_token, token_type: "Bearer", expires_at, user}` in the body; every protected request stamps `Authorization: Bearer <accessToken>`. The SPA stores tokens in a module-level `tokenStore` (in-memory + `sessionStorage` for per-tab reload persistence) — tokens are NEVER persisted to localStorage. The Zustand `authStore` `persist` carries only `user`, and the v3 `migrate` strips any pre-Bearer token fields from old localStorage. The wire contract is identical for native clients (iOS / Android / Unity / Unreal / console SDKs) — only the token storage differs (Keychain / Keystore / Windows Credential Manager / platform OAuth exchange). Cross-origin (SPA at `app.nyx.com`, API at `api.nyx.com`) is fine: Bearer is a custom Authorization header, which forces a CORS preflight — that's the natural CSRF defence, so no CSRF token flow is needed.
- [x] **Token Refresh** — `api.ts` single-flight refresh-on-401 driven by the backend's `WWW-Authenticate: Bearer error="invalid_token", error_description="expired"` signal. Bare `error="invalid_token"` (no `expired`) keeps the immediate-logout path so a tampered token never silently retries. The refresh POST sends `{refresh_token}` in the body to `/api/refresh`; on success the `tokenStore` swaps in the fresh pair and the original request is replayed with the new Bearer header. Concurrent 401s collapse into a single refresh via the `refreshing: Promise<boolean> | null` single-flight pattern.

## 6. Testing & Quality

- [x] **Component tests** — Vitest + React Testing Library + `@testing-library/user-event`. `IntersectionObserver` polyfill in `test/setup.js`.
- [x] **Playwright E2E** — `frontend/tests/e2e` covers login → create → delete a movie, etc. Auto-starts `vite dev`.
- [x] **CI lint + test + build** — `frontend-test` job in `.github/workflows/ci.yml`.
- [ ] **Storybook** — develop components in isolation for visual consistency and design docs.
- [ ] **Accessibility (a11y) auditing** — `eslint-plugin-jsx-a11y` and automated a11y assertions in tests.

## 7. Performance

- [x] **Loading skeletons** — `SkeletonCard`, `MovieListSkeleton` in `MovieList.tsx` replace plain "Loading…" text.
- [x] **Confirm dialog** — Radix `Dialog` replaces `window.confirm()` in `App.tsx` (accessible, keyboard-navigable, non-blocking).
- [ ] **Code Splitting** — `React.lazy` + dynamic imports for route-based chunking (currently single bundle).
- [ ] **Image Optimization** — responsive images (WebP/AVIF) for hero/posters when the asset pipeline grows.

## 8. SEO

- [x] **Dynamic Metadata** — per-view `<title>` and meta description via React 19's native metadata hoisting (`<PageMeta>` wrapper in `src/components/ui/`). `index.html` retains a static title + description as the pre-JS crawler fallback.

## 9. Observability

- [x] **OpenTelemetry Tracing** — `@opentelemetry/sdk-trace-web` initialised in `frontend/src/services/telemetry.ts`. When `VITE_OTEL_ENABLED=true`, builds a `WebTracerProvider` with a `BatchSpanProcessor` + `OTLPTraceExporter` pointed at the same-origin `/otlp/v1/traces` (the SPA container's nginx proxies that path to `JAEGER_HOST:4318`, bypassing Jaeger's missing CORS without an OTel Collector). Registers `FetchInstrumentation` (axios uses fetch internally) and `XMLHttpRequestInstrumentation`. Default-off (`VITE_OTEL_ENABLED !== "true"`) — `initTelemetry()` is a noop, every `tracer.Start` is free. Resource `service.name = VITE_OTEL_SERVICE_NAME || "nyx-frontend"` so Jaeger groups frontend spans separately from backend.
- [x] **W3C Traceparent propagation** — axios request interceptor (`frontend/src/services/api.ts`) calls `injectTraceparent()` on every outbound request so the backend's `otelhttp` server span joins the browser-initiated trace. The `traceparent` is stamped AFTER the auth header so both end up on the wire; runs on the 401 retry path too. Verified by `api.test.ts` (regex assertion against the W3C shape).
- [x] **Structured Logger** — `frontend/src/services/logger.ts`. JSON-shaped `console.{debug,info,warn,error}` with `ts`, `level`, `msg`, `trace_id`, `span_id` pulled from `trace.getActiveSpan()`. Replaces the three pre-existing `console.error` callsites in `App.tsx` and `AuthForm.tsx`. Pure browser; no backend dependency.
- [x] **Global error capture** — `window.addEventListener('error' | 'unhandledrejection', …)` in `main.tsx` forwards uncaught errors into `logger.error` so they appear alongside the rest of the structured log surface. The HTTP error toast flow in `api.ts` is unchanged (these handlers complement, not duplicate).
- [x] **Same-origin OTLP proxy** — `frontend/nginx.conf` adds `location /otlp/` that proxies to `JAEGER_HOST:4318`. `__JAEGER_HOST__` is substituted at image build time via `sed` in the frontend `Dockerfile` (`ARG JAEGER_HOST=jaeger`). Avoids CORS entirely (browser POSTs to its own origin) and avoids adding an OTel Collector.

## 9. Developer Experience

- [x] **ESLint** — `eslint.config.js` (flat config), React + hooks plugins.
- [ ] **Prettier** — not yet introduced. Formatting relies on ESLint autofix and editor defaults.
- [x] **Husky + lint-staged** — pre-commit runs `eslint --fix` + `vitest related --run --passWithNoTests` on staged files.
