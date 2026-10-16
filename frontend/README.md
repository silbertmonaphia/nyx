# React + Vite

This template provides a minimal setup to get React working in Vite with HMR and some ESLint rules.

Currently, two official plugins are available:

- [@vitejs/plugin-react](https://github.com/vitejs/vite-plugin-react/blob/main/packages/plugin-react) uses [Oxc](https://oxc.rs)
- [@vitejs/plugin-react-swc](https://github.com/vitejs/vite-plugin-react/blob/main/packages/plugin-react-swc) uses [SWC](https://swc.rs/)

## Theming

The theme toggle lives in the navbar immediately right of the `Nyx` wordmark (hidden below the
`md` breakpoint).

- No stored choice: the app mirrors the OS `prefers-color-scheme`, including live changes while
  the app is open.
- A stored choice wins over the OS, across reloads and later OS changes.
- Storage contract: `localStorage["nyx.theme"]` is `"light"` or `"dark"`; an absent or
  unrecognized value means "no choice". Storage access is best-effort — if it throws (private
  mode, quota) the toggle still works for the session and the next load falls back to the OS.
- Two call sites own the contract: the inline boot script in `index.html` applies the choice
  before first paint (no flash) and re-checks it on every OS change, and `src/theme/theme.ts`
  (`apply()`) mirrors it onto the document element, including `color-scheme` so native widgets
  and scrollbars match. The app also publishes the in-session choice on `window.__nyxTheme`, so
  the boot script's listener honors a choice that storage could not persist.

## Localization

The locale toggle sits between the `Nyx` wordmark and the theme toggle (hidden below the `md`
breakpoint). Both dictionaries live in `src/i18n/`: `zh.ts` is the source of truth and `en.ts`
is held to it by a recursive parity type, so a key present in one and missing in the other is a
type error; the declaration merging in `src/i18n/i18next.d.ts` compile-checks the keys passed to
`t()`, and a runtime test asserts leaf-path and placeholder parity.

- Chinese is the default on every first visit, whatever the browser language is.
- Storage contract: `localStorage["nyx.locale"]` is `"zh"` or `"en"`; an absent or unrecognized
  value means "no choice" (→ Chinese). Storage access is best-effort — if it throws, the switch
  still works for the session and the next load falls back to Chinese.
- `<html lang>` follows the active locale, and an explicit pick is remembered across reloads.
- The e2e suite pins `nyx.locale=en` via `tests/e2e/fixtures/e2e-fixtures.ts` so the existing
  English assertions stand unchanged; `tests/e2e/i18n.spec.ts` deliberately skips that fixture
  and covers the Chinese default, the switch, and persistence.

## React Compiler

The React Compiler is not enabled on this template because of its impact on dev & build performances. To add it, see [this documentation](https://react.dev/learn/react-compiler/installation).

## Expanding the ESLint configuration

If you are developing a production application, we recommend using TypeScript with type-aware lint rules enabled. Check out the [TS template](https://github.com/vitejs/vite/tree/main/packages/create-vite/template-react-ts) for information on how to integrate TypeScript and [`typescript-eslint`](https://typescript-eslint.io) in your project.
