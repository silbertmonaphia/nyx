# Tasks

## 1. i18n core

- [x] 1.1 Add `frontend/src/i18n/`: `zh.ts` as the key source of truth (nested by surface), `en.ts` typed with a recursive parity type, `i18next.d.ts` declaration merging for compile-checked `t()` keys, i18next init (`lng` = stored `nyx.locale` ?? `zh`, `fallbackLng: 'zh'`, `escapeValue: false`), `languageChanged` subscriber persisting the choice (try/catch) and syncing `<html lang>`. Verify: unit tests — default zh, switch applies immediately, persistence across reload, interpolation, key-set parity and non-empty values; the type guarantee verified by tsc probes (bogus key → TS2345, extra `en` key → TS2353, missing → TS2741). Note: `npm run build` is `vite build` (no typecheck) and CI has no `tsc` gate — the compile-time guarantee is local/IDE-only; a CI `tsc --noEmit` gate is a flagged follow-up, not built here.
- [x] 1.2 Localize the chat sidebar's `relativeTime` buckets via i18next `count` interpolation (zh carries `_one`/`_other` forms for the recursive parity type) and format the absolute timestamp with `Intl.DateTimeFormat(locale, …).formatToParts()` reassembled to the pinned `MM-DD HH:mm` pattern — the pinned format in the sidebar's unit + e2e assertions is preserved (kept pattern, locale-aware parts). Verify: unit tests assert zh and en outputs.

## 2. Test scaffolding

- [x] 2.1 Pin English for both suites: `frontend/src/test/setup.js` initializes i18n + `changeLanguage('en')` per test; a new shared e2e fixture (`tests/e2e/fixtures/e2e-fixtures.ts`) overrides `context` with an `addInitScript` pinning `nyx.locale=en`, imported by all 15 existing specs (import-only diffs). Verify: the existing RTL suite and the e2e set run green with zero assertion edits.

## 3. App chrome and auth extraction

- [x] 3.1 Extract `App.tsx` chrome (navbar, auth/delete dialogs, empty states) and add `LocaleToggle` beside the theme switch (localized accessible name, keyboard reachable); theme-toggle aria-labels extracted too. Verify: unit tests render the navbar and date buckets in both locales; `npm run lint` clean.
- [x] 3.2 Extract the `features/auth` surfaces (login/register form labels, placeholders, validation and error copy). Verify: auth unit tests green.

## 4. Feeds extraction

- [x] 4.1 Extract `features/feeds` strings (list/actions/filters, toasts, confirmations, empty states; interpolated names for delete confirmations). Verify: feeds unit tests green; grep checklist shows no residual user-visible literals under `features/feeds` outside tests/testids.

## 5. Chat extraction

- [x] 5.1 Extract `features/chat` sidebar strings (title placeholder, timestamps, running indicator labels, history aria-label as its own key). Verify: `ChatSidebar` unit tests green; zh placeholder asserted in a unit test.
- [x] 5.2 Extract the chat panel, banners, approval/confirmation cards, composer hints, and store/service toast copy (including `PartialFailureBanner`'s previously TODO-deferred strings). Verify: chat unit tests green; grep checklist shows no residual user-visible literals under `features/chat` outside tests/testids.

## 6. Integration and docs

- [x] 6.1 Add `frontend/tests/e2e/i18n.spec.ts`: Chinese default with an English browser, switch applies immediately, persistence across reload, per-surface zh sweep (sidebar, composer, login, feed actions); independent of the English-pinning fixture. Verify: green headless (backend-free subset ran 12 passed together with theme-toggle against a scratch vite; the full agent-free run is the post-merge gate).
- [x] 6.2 Run the suites on the committed tree: lint clean; `npm test` 25 files / 366 passed (baseline 24/347); `npm run build` ok; `check_no_comments.py --range` clean; independent `test-reviewer` pass approved with all 123 used `t()` keys resolvable. The full e2e gate runs post-merge (main session).
- [x] 6.3 Update user-facing docs (root `README.md` frontend section + `frontend/README.md`) with the bilingual behavior and the `nyx.locale` contract. Verify: diff reviewed.
- [x] 6.4 Mirror the bilingual bullet into the zh doc surface (`README.zh.md` 「底层的 App」, main-session follow-up). Verify: diff reviewed.
