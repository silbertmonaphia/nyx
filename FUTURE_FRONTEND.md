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
- [x] **Axios client** — `services/api.ts` with request interceptor (auth header) and response interceptor (global 401 → logout, error message extraction).
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

- [x] **Zustand** — `authStore` (token, login, logout, register), `uiStore` (toasts), `movieUiStore` (filter UI). Lightweight, no provider boilerplate.
- [x] **Persist Auth Token Securely** — `authStore.ts` no longer persists tokens. Both the access JWT and the opaque refresh token ride httpOnly `__Host-` cookies set by the backend; the JSON envelope on auth endpoints is `{user, expires_at}` only. Frontend axios drops the Authorization interceptor and gains `withCredentials: true`; refresh-on-401 still works via the `WWW-Authenticate: expired` challenge. `authStore` carries only `user` (via Zustand `persist`, partialize strips everything else) with a `migrate` that discards any pre-cookie token fields from old localStorage. Same-origin topology is the prerequisite: Vite dev server proxies `/api`, prod nginx adds `location /api/`, k8s ingress already routes by path. CSRF: SameSite=Lax + same-origin only — revisit if the API ever moves to a separate subdomain.
- [x] **Token Refresh** — `api.ts` single-flight refresh-on-401 driven by the backend's `WWW-Authenticate: Bearer error="invalid_token", error_description="expired"` signal. Bare `error="invalid_token"` (no `expired`) keeps the immediate-logout path so a tampered token never silently retries. Refresh tokens persist alongside access tokens in the `authStore` (Zustand `persist`, key `nyx-auth-storage`).

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
