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
- [ ] **Search Debounce** — `App.tsx` passes `searchTerm` directly to `useMovies(searchTerm)` on every keystroke. Add a 300ms debounce (custom hook or `use-debounce`) to cut request volume.

## 3. Form Management & Validation

- [x] **React Hook Form** — uncontrolled components; less re-render churn than `useState`-driven forms.
- [x] **Zod** — schemas live with the feature (`features/movies/types.ts`); `@hookform/resolvers/zod` wires them into `<form>`.

## 4. Styling & Design System

- [x] **Tailwind CSS v4** — `@tailwindcss/postcss` config; design tokens via `tailwind.config.js`.
- [x] **Radix UI + shadcn-style primitives** — `@radix-ui/react-dialog`, `@radix-ui/react-label`, `@radix-ui/react-slot` power accessible `Dialog`, `Label`, `Button asChild`. Components in `src/components/ui/` are owned (not installed) so we can tweak freely.
- [ ] **CSS Modules / Vanilla Extract** — for component-specific styles that need complex logic while keeping type safety. Currently Tailwind is sufficient; revisit if/when a design system has to grow.

## 5. Global State

- [x] **Zustand** — `authStore` (token, login, logout, register), `uiStore` (toasts), `movieUiStore` (filter UI). Lightweight, no provider boilerplate.
- [ ] **Persist Auth Token Securely** — `authStore.ts` uses Zustand `persist` which writes the JWT to `localStorage`. Consider httpOnly cookies, or at minimum document the XSS risk in the README.
- [ ] **Token Refresh** — `api.ts` logs out on 401 but does not attempt a refresh. Pair with the backend refresh-token endpoint once §2 (JWT Refresh Tokens) lands in `FUTURE_BACKEND.md`.

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

- [ ] **Dynamic Metadata** — per-page `<title>` and meta description (`react-helmet-async` or standard DOM updates). Currently only a static title in `index.html`.

## 9. Developer Experience

- [x] **ESLint + Prettier** — `eslint.config.js` (flat config), React + hooks plugins.
- [x] **Husky + lint-staged** — pre-commit runs `eslint --fix` + `vitest related --run --passWithNoTests` on staged files.
