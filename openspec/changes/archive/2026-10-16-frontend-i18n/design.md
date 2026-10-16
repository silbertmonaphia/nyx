# Design

## Context

See proposal.md — Why. Current state: 19 non-test `.tsx` components across `frontend/src/features/{auth,chat,feeds}/` plus `App.tsx` (navbar at `App.tsx:157-200`); strings are inline English literals, including `aria-label`/`title` attributes and error/toast copy inside stores and services (e.g. `chatStore.ts` decision errors, `PartialFailureBanner.tsx`). Tests: RTL via vitest with `frontend/src/test/setup.js` (jsdom + IntersectionObserver stub); Playwright specs assert English literals throughout `frontend/tests/e2e/` with a shared `fixtures/` directory and a `use` block in `frontend/playwright.config.ts:19`. Sequencing: `agent-chat-fixes` and `frontend-theme-toggle` touch chat components and the navbar respectively — this change lands after (or rebases over) both.

## Goals / Non-Goals

**Goals:** a type-safe bilingual layer on the standard i18next stack; Chinese default with the user's choice remembered; mechanical, verifiable extraction of every chrome string; existing test suites keep their English assertions with minimal edits.

**Non-Goals:** a third locale, lazy-loaded locale bundles, RTL, currency/number localization beyond what `Intl` gives for dates, translating backend/model content, translating brand tokens ("Nyx").

## Decisions

### D1 — react-i18next over a hand-rolled module (user decision, 2026-10-16)

The reviewer chose the standard stack: `i18next` + `react-i18next` with static resources. Rationale: mature interpolation/plural/formatting built-ins, the `useTranslation()` idiom, an active ecosystem, and typed keys preserved via declaration merging so key completeness stays compile-checked. Alternatives considered: a hand-rolled typed module (fewer dependencies, full control — set aside in review in favor of ecosystem tooling). Extra plugins stay out: no language detector (the resolver is explicit, D3), no async JSON loading (static TS resource modules keep keys typed and diffs reviewable).

### D2 — Module shape and typing

`frontend/src/i18n/index.ts` initializes i18next: `resources: { zh, en }`, `lng` = stored `nyx.locale` ?? `'zh'`, `fallbackLng: 'zh'`, `supportedLngs: ['zh', 'en']`, `interpolation.escapeValue: false` (React escapes). `zh.ts` is the key source of truth (nested by surface); `en.ts` is typed with a recursive parity type (`{ [K in keyof T]: T[K] extends string ? string : … }`) so a missing or extra key fails `tsc` before declaration merging can mask it; `frontend/src/i18n/i18next.d.ts` merges `CustomTypeOptions.resources = typeof zh` so `t()` keys are compile-checked in components and stores. Persistence and `<html lang>` sync ride one `i18n.on('languageChanged', …)` subscriber with try/catch storage access. Components use `useTranslation()`; stores/services import the initialized instance's `t`. All resource values must be non-empty (unit-checked, alongside a runtime key-set parity check).

### D3 — Default Chinese, explicitly ignoring navigator.language

The user requirement is a hard default: `zh` regardless of browser language; `en` only ever comes from an explicit choice — and that choice is remembered (resolver: `localStorage["nyx.locale"] ?? "zh"`, kept in sync on every switch). No language detection, no `Accept-Language` handling, no browser-preference sniffing.

### D4 — Extraction strategy

Per feature directory, walk components mechanically: JSX text nodes, `placeholder`/`aria-label`/`title`/`alt` attributes, toast/banner strings in stores and services, and the e2e-visible copy. Strings carrying dynamic content use interpolation keys (e.g. `feeds.deleteConfirm` with `{name}`). Schema-adjacent literals (CSS classnames, testids, test-only strings, `data-*` values) are NOT extracted — the checklist in tasks names the grep patterns so the boundary is reviewable.

### D5 — Test strategy: pin English, add one Chinese spec

- RTL: `frontend/src/test/setup.js` initializes the i18n instance and calls `i18n.changeLanguage('en')` before each test, so the existing English assertions in the 20+ test files stand unchanged; the i18n tests (and any component test asserting zh) override explicitly.
- Playwright: the shared fixtures gain an init script (`addInitScript` setting `nyx.locale=en`) applied to every existing spec — zero assertion edits; the new `i18n.spec.ts` uses a raw context to exercise the Chinese default, switching, persistence, and a zh surface sweep. The live-stack `@live` specs inherit the same fixture.
- The zh sweep asserts a small, stable set of per-surface strings (sidebar placeholder, composer hint, login labels, feed actions) rather than snapshotting the whole app — churn-resistant, still catches "forgot to extract this surface".

### D6 — Locale-aware time

`relativeTime` moves into resource keys using i18next's `count` interpolation per bucket (`time.minutesAgo`) so English plurals come from i18next's plural rules while Chinese needs none; the absolute part of the sidebar timestamp uses `Intl.DateTimeFormat(locale)` (after `agent-chat-fixes` lands its `absolute · relative` format, i18n swaps the absolute formatter). The `title` tooltip keeps the raw ISO string.

### D7 — Switcher placement

Top-left of the navbar, inside the brand cell immediately right of the `Nyx` wordmark (`App.tsx:158-161`), directly left of the theme toggle — final order [Nyx] [中/EN] [☀/🌙]. This change lands after `frontend-theme-toggle`, so the switcher inserts itself between the brand and the existing toggle. A two-state text control (`中 / EN`) with a localized `aria-label`; controls hide or shrink below the `md` breakpoint so the brand cell never crowds the centered search field. `document.documentElement.lang` is synced by the locale store on every change and at boot.

## Risks / Trade-offs

- [Extraction misses strings → mixed-language UI] → grep-pattern checklist per feature group + the zh e2e sweep per surface; missed strings are additive fixes, not schema churn.
- [Massive diff touching nearly every component] → grouped per feature so reviews stay scoped; dictionary keys are the only new contract, and they are compile-checked.
- [Test churn] → D5's pinning keeps existing specs untouched; only fixtures and setup gain one hook each.
- [Merge conflicts with the two sibling changes] → landing order stated in all three proposals; the conflict surface is three files (`App.tsx`, `ChatSidebar.tsx`, `test/setup.js`).
- [Translation quality] → dictionaries are plain files in the PR; the reviewer is a native Chinese speaker; keys are grouped by surface for reviewability.

## Migration Plan

No data migration; `nyx.locale` is additive; rollback = revert (strings return to English literals). No rollout gates.
