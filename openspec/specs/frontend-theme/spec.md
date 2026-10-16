# frontend-theme Specification

## Purpose
Lets the user choose a light or dark theme explicitly, while the OS preference remains the default until a choice is made, so the app looks right on first visit and stays right after the user decides.

## Requirements

### Requirement: Theme resolution: explicit choice over system preference

When no explicit choice is stored, the app SHALL follow the OS color scheme, including live changes while the app is open. Once the user makes an explicit choice, it SHALL persist across reloads and SHALL take precedence over the OS setting; an OS change SHALL NOT override a stored choice.

#### Scenario: First visit follows the OS

- **WHEN** a user with a dark OS scheme opens the app with no stored choice
- **THEN** the app renders in dark theme (and in light theme for a light OS scheme)

#### Scenario: OS change honored while no choice is stored

- **WHEN** the OS scheme flips while the app is open and no choice has been made
- **THEN** the app switches theme without a reload

#### Scenario: Explicit choice survives reloads and OS changes

- **WHEN** the user toggles to dark, reloads, and the OS scheme flips to light
- **THEN** the app renders dark after each step

### Requirement: Pre-paint application without flash

The stored choice SHALL be applied before the first paint on every load, so no frame renders with the non-selected theme.

#### Scenario: Reload renders the stored choice immediately

- **WHEN** the user reloads with a stored dark choice
- **THEN** the document root carries the `dark` class before the app's first paint (no light flash)

### Requirement: Theme toggle control

The app SHALL render a theme toggle in the navbar whose accessible name reflects the next action (e.g. "Switch to light theme" while dark is active). Activating it SHALL flip the theme immediately without a reload and persist the choice; the control's state SHALL reflect the resolved theme, including changes driven by the OS while no choice is stored.

#### Scenario: Toggle flips and persists

- **WHEN** the user activates the toggle from light to dark
- **THEN** the app renders dark immediately, the stored choice is `dark`, and a reload keeps dark

#### Scenario: Accessible label tracks state

- **WHEN** the resolved theme is dark
- **THEN** the toggle offers switching to light in its accessible name (and vice versa)

#### Scenario: Storage unavailable degrades to system-follow

- **WHEN** `localStorage` is unavailable (private mode restrictions, storage full)
- **THEN** the app still toggles in-session and never throws; on reload it falls back to the OS scheme
