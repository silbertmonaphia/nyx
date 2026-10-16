# Proposal

## Why

The SPA is English-only with strings hardcoded across components — the codebase notes it itself ("the codebase has no localisation framework today", `frontend/src/features/chat/components/PartialFailureBanner.tsx:46`; no i18n dependency exists). The user asked for bilingual support with Chinese as the default and a switch to English (bugs.md 前端优化①).

## What Changes

- **react-i18next on typed resource bundles** (user decision, 2026-10-16): the SPA adds `i18next` + `react-i18next` with static `zh`/`en` resources; typed resources (declaration merging) keep key completeness compiled-checked; interpolation and plurals use i18next's built-ins; non-component code (stores/services) uses the initialized `t()`.
- **Locale selection**: default Chinese on first load regardless of browser language; the user's explicit choice is remembered across reloads and later visits (`localStorage` `nyx.locale`); a switcher in the navbar applies changes immediately without reload; `<html lang>` follows the active locale.
- **Full chrome extraction** into the dictionaries: navigation/app chrome, auth forms, feeds surfaces, chat surfaces (sidebar, panel, banners, approval/confirmation cards, toasts, empty states), plus all `aria-label`/`title`/`placeholder` strings.
- **Locale-aware time**: relative labels ("3 hr ago" / "3 小时前") come from the dictionaries; absolute timestamps format via `Intl.DateTimeFormat`.
- **Tests**: dictionary completeness + module unit tests; existing RTL and Playwright suites pinned to English so current assertions stand; a new e2e spec asserts the Chinese default, switching, persistence, and key Chinese surfaces.

Out of scope: backend- and model-produced strings (error details, SSE content) render as received; assistant replies and stored session titles are never translated; no third locale; brand name "Nyx" and the page title stay as-is.

No breaking changes; no API surface.

## Capabilities

### New Capabilities

- `frontend-i18n`: locale selection (Chinese default, English switch), typed dictionaries covering all SPA chrome, and locale-aware date/time formatting.

### Modified Capabilities

None — the English literals inside other specs' requirement text are treated as the English-locale rendering; no requirement semantics change.

## Impact

- New dependencies `i18next` + `react-i18next` (added per the repo's npm-workspaces convention: `npm install --workspaces=false --package-lock-only`, `frontend/package-lock.json` committed alongside) and a new `frontend/src/i18n/` module (i18next init, `zh`/`en` resources, typed-resource declaration, persistence); a locale switcher top-left in the navbar (`App.tsx`, right of the brand, left of the theme toggle); string extraction across `frontend/src/features/{auth,feeds,chat}/`; `ChatSidebar`'s `relativeTime` and timestamp formatting localized.
- Tests: `frontend/src/test/setup.js` pins English for the RTL suite; e2e fixtures gain an init script pinning `nyx.locale=en`; new `frontend/tests/e2e/i18n.spec.ts` (Chinese default + switching + persistence + surface sweep); module unit tests.
- Overlap/sequencing: touches chat components also modified by `openspec/changes/agent-chat-fixes/` and the navbar also touched by `openspec/changes/frontend-theme-toggle/` — land those first (or rebase) to avoid churn.
- Rollout: none foreseen — every task completes in the dev/CI environment.
