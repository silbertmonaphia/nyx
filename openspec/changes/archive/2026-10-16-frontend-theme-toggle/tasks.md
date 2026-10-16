# Tasks

## 1. Theme core

- [x] 1.1 Add `frontend/src/theme/theme.ts` (choice read/write with try/catch, `resolve()`, `apply()`, `subscribe()`) and a `useTheme()` hook (`useSyncExternalStore`, re-apply on mount, OS listener that re-reads the choice at fire time — review found the parse-time capture let the listener override a stored choice; fixed and mutation-verified). Verify: new unit tests — resolution matrix (stored light/dark × OS light/dark), persistence, storage-unavailable degrade never throws, subscriber notified on change (`npm test`).
- [x] 1.2 Update the `index.html` boot script to honor `nyx.theme` before first paint (plus the in-session `window.__nyxTheme` publish), with the OS listener honoring stored/in-session choices; skeleton dark styles re-scoped to `html.dark` so a stored light choice cannot flash a dark skeleton. Verify: exercised by the e2e scenarios in 2.2 (jsdom cannot run the inline script).

## 2. Toggle control + e2e

- [x] 2.1 Add the toggle to the navbar's brand cell, immediately right of the `Nyx` wordmark (`App.tsx`; `Sun`/`Moon` from `lucide-react`, dynamic `aria-label`, `type="button"`, hidden below `md`); set `color-scheme` on the document element as part of `apply()`. Verify: unit tests for the control (flips store, label tracks state) and `npm run lint` clean.
- [x] 2.2 Add `frontend/tests/e2e/theme-toggle.spec.ts`: first-visit OS follow (`emulateMedia`), toggle flips + persists across reload, `dark` class present pre-paint on reload with stored choice (proven by aborting `/src/main.*`), OS change honored while unset, stored choice outranks a later OS flip, in-session choice survives an OS flip when storage writes throw, aria-label tracking, computed `color-scheme`, hidden below `md`. Verify: 8/8 green standalone with no backend running (`npm run test:e2e -- theme-toggle`).

## 3. Documentation

- [x] 3.1 Document the theme behavior and the `nyx.theme` storage contract where frontend features are described (`README.md` bullet + `frontend/README.md` "Theming" section). Verify: diff reviewed; no stale "follows the OS only" claim remains.
- [x] 3.2 Mirror the theme bullet into the zh/full doc surfaces (`README.zh.md` 「底层的 App」 bullet + `README_FULL.md` frontend bullet; main-session follow-up, since the lane's scope guard stopped at `frontend/**` + root README.md). Verify: diff reviewed.
