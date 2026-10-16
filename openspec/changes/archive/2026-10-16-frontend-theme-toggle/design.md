# Design

## Context

See proposal.md — Why. Current state: `frontend/index.html:63-75` runs an inline boot script that toggles `.dark` on `<html>` from `matchMedia('(prefers-color-scheme: dark)')` and live-updates on OS changes; `frontend/src/index.css:5` defines the Tailwind v4 dark variant (`@custom-variant dark (&:is(.dark *))`) with token overrides in `.dark` (`index.css:99`). Navbar markup lives at `App.tsx:157-200`. Repo precedent for tiny module-level state: `tokenStore` (module-level, not a Zustand store); `lucide-react` is already a dependency.

## Goals / Non-Goals

**Goals:** a persistent explicit choice layered over the existing OS-following; pre-paint correctness; a small, testable module; zero new dependencies.

**Non-Goals:** a "system" option in the UI beyond the implicit default (no stored choice), per-component theme overrides, theme sync across devices, `theme-color` meta tuning.

## Decisions

### D1 — Module-level theme store, not a Zustand store

`frontend/src/theme/theme.ts` exposes `getChoice()`, `setChoice(v)`, `resolve()`, `apply()`, and `subscribe()`; a `useTheme()` hook uses React's `useSyncExternalStore`. Rationale: the state is a single string consumed in two places (boot script reads storage directly; the component subscribes) — mirrors the `tokenStore` precedent and keeps the boot path dependency-free. Alternative rejected: Zustand store (heavier than the concern; no persist middleware needed).

### D2 — Storage contract

`localStorage["nyx.theme"]` = `"light" | "dark"`; any other value or absence = system-follow. All storage access is try/catch-wrapped (private-mode degradation, spec scenario). The boot script reads the same key, so script and app share one contract; the key name joins the existing `nyx.*` namespace (`nyx.chat.useFeeds` being removed by `agent-chat-fixes`, `nyx.locale` planned by `frontend-i18n`).

### D3 — Boot script update (pre-paint)

`index.html` becomes: read the stored choice (plus the in-session `window.__nyxTheme` override the app publishes); if present, apply it at parse time. The OS `change` listener is registered unconditionally but re-reads the choice at fire time — a stored or in-session choice outranks later OS flips, and no teardown bookkeeping is needed (this wording reconciles a contradiction between this decision and the Risks register; the agent implemented the fire-time re-read and proved it with e2e — 2026-10-16). The app-side `apply()` re-applies the resolved value on mount (idempotent). Skeleton dark styles are scoped to `html.dark` rather than `prefers-color-scheme`, so a stored light choice cannot flash a dark skeleton.

### D4 — Toggle placement and semantics

Top-left of the navbar, inside the brand cell immediately right of the `Nyx` wordmark (`App.tsx:158-161` — the cell becomes a flex row; the centered search field stays in the middle grid column). This change lands before `frontend-i18n`, so the toggle is initially the first control after the brand; once i18n lands, the locale switcher sits to its left ([Nyx] [中/EN] [☀/🌙]). On narrow viewports the brand cell must not crowd the search field — controls hide or shrink below the `md` breakpoint. `lucide-react` `Sun`/`Moon`; dynamic `aria-label` ("Switch to light/dark theme"); `type="button"`. No confirmation, no menu — a single flip. Optionally set `color-scheme: dark|light` on the document element as part of `apply()` so native widgets (scrollbars, form controls) match; verified by a computed-style e2e assertion.

### D5 — Verification

Unit: resolution matrix (stored × OS), persistence, storage-unavailable degrade, `useTheme` re-render on change. E2E (`theme-toggle.spec.ts`): first-visit OS follow via `emulateMedia({ colorScheme })`, toggle flips + survives reload, `dark` class present pre-paint after reload, OS change while unset, aria-label tracking. The "no flash" scenario is asserted by checking the class immediately on load after setting the stored value via `addInitScript` (a true first-paint race cannot be observed from Playwright; this pins the boot-script contract).

## Risks / Trade-offs

- [Boot script and app logic drift] → one shared storage key string, duplicated deliberately in the inline script (it cannot import modules); both covered by e2e.
- [OS-follow listener leaks across choice changes] → the listener is registered unconditionally at module load and checks `getChoice()` at fire time (no teardown bookkeeping); spec scenario pins behavior.
- [Navbar merge conflict with `frontend-i18n`] → sequencing note in both proposals; whichever lands second rebases.
- [jsdom cannot exercise the boot script] → boot correctness lives in e2e only; unit tests cover the module.

## Migration Plan

No data migration; rollback = revert the change (stored keys are ignored by old code, the class just stops persisting). No rollout gates.
