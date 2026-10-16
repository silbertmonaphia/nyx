# Proposal

## Why

The SPA currently follows the OS color scheme only — `frontend/index.html:69-74` mirrors `prefers-color-scheme` onto the `.dark` class and there is no manual control (bugs.md 前端优化②). Tailwind v4's `dark` variant and the token overrides already exist (`frontend/src/index.css:5,99`), so this is a control + persistence gap, not a theming build-out.

## What Changes

- **Explicit theme choice with a system default.** A stored choice (`light` | `dark`, `localStorage` key `nyx.theme`) takes precedence; absence keeps the current OS-following behavior, live-updated while the app is open.
- **Pre-paint application.** The `index.html` boot script honors the stored choice before first paint (no flash of the wrong theme) and stops mirroring the OS once a choice exists.
- **Toggle control** top-left in the navbar beside the `Nyx` brand (`App.tsx:157-161`): flips and persists immediately, no reload, accessible name reflecting the next action, state reflecting the resolved theme (including OS-driven changes).
- No new dependency — `lucide-react` (already in the stack) supplies sun/moon icons.

No breaking changes; no API surface.

## Capabilities

### New Capabilities

- `frontend-theme`: explicit light/dark selection with system-preference fallback, pre-paint application, and a toggle control.

### Modified Capabilities

None — this is the first spec for theming behavior.

## Impact

- `frontend/index.html` (boot script), `frontend/src/App.tsx` (navbar control), a new `frontend/src/theme/` module.
- Tests: unit tests for resolution/persistence; new e2e `frontend/tests/e2e/theme-toggle.spec.ts` (OS-follow first visit, toggle + persistence across reload, no-flash, OS change while unset).
- Overlap: touches the `App.tsx` navbar, which `openspec/changes/frontend-i18n/` also touches for its locale switcher — land one before starting the other (or rebase) to avoid churn.
- Rollout: none foreseen — every task completes in the dev/CI environment.
