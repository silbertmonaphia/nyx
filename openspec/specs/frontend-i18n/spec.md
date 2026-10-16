# frontend-i18n Specification

## Purpose
Makes the SPA's user-facing chrome bilingual — Chinese by default, English on request — through typed dictionaries and a persistent locale choice, while backend- and model-produced content always renders verbatim.

## Requirements

### Requirement: Locale selection with Chinese default

On first load with no stored choice, the app SHALL render in Chinese regardless of the browser's language. The app SHALL provide a control to switch to English, SHALL apply a change immediately without a reload, SHALL persist the choice in `localStorage` (`nyx.locale`), and SHALL reflect the active locale in the document's `lang` attribute. The control's accessible name SHALL itself be localized. The control follows the navbar's responsive pattern (hidden below the `md` breakpoint, matching the theme toggle); the below-`md` reachability gap is a recorded limitation, not an accident.

#### Scenario: First load is Chinese

- **WHEN** a fresh browser profile (empty storage) opens the app
- **THEN** the chrome renders in Chinese and `<html lang>` reflects Chinese, even when the browser language is English

#### Scenario: Switching applies immediately

- **WHEN** the user activates the locale control
- **THEN** buttons, labels, placeholders, empty states, and the sidebar render in English without a reload

#### Scenario: Choice persists

- **WHEN** the user switches to English and reloads (or opens the app later)
- **THEN** the app renders in English; after clearing storage it renders Chinese again

### Requirement: Typed dictionaries cover all SPA chrome

Every user-visible string the SPA renders SHALL come from the active locale's dictionary — navigation, forms, buttons, labels, placeholders, empty states, banners and toasts, `aria-label` and `title` attributes, and time labels. A key present in one dictionary but missing from the other SHALL fail type-checking; dictionaries SHALL be unit-verified non-empty for every key. Strings produced by the backend or the model — API error details, assistant replies, stored session titles — SHALL render exactly as received and MUST NOT be routed through the dictionaries.

#### Scenario: Chinese surfaces render from the dictionary

- **WHEN** the app runs in Chinese and the user visits the feeds list, the login form, and the chat surfaces
- **THEN** sampled strings on each surface are the Chinese dictionary entries (e.g. the empty-history placeholder, the composer hint), with no raw English chrome text remaining

#### Scenario: Dictionaries cannot drift

- **WHEN** a key is added to the Chinese dictionary but not to English (or vice versa)
- **THEN** the recursive parity type fails type-checking under `tsc`/IDE, and the unit test — the CI-enforced gate, since CI runs `vite build` without a typecheck — asserts key-set equality and non-empty values; adding a CI `tsc --noEmit` step is a recorded follow-up

#### Scenario: Server and model content stays verbatim

- **WHEN** a stored session title or an API error detail is rendered under either locale
- **THEN** the string appears exactly as received, untranslated

### Requirement: Locale-aware date and time formatting

Relative time labels SHALL come from the active locale's dictionary (e.g. "3 hr ago" / "3 小时前"), and absolute timestamps SHALL be formatted with `Intl.DateTimeFormat` for the active locale. The full ISO timestamp in tooltips stays locale-independent.

#### Scenario: Relative labels localize

- **WHEN** a session's `last_active_at` is three hours ago
- **THEN** the Chinese locale renders the Chinese relative label and the English locale the English one

#### Scenario: Absolute timestamps follow the locale

- **WHEN** an absolute timestamp is rendered in either locale
- **THEN** it is produced by `Intl.DateTimeFormat` for that locale (unit-asserted against the formatter's output)
