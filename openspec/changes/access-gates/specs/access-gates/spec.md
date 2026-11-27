# Spec Delta

## Purpose

Deployment-level registration and AI access gates: one backend env contract that can close or narrow public sign-up and restrict AI surfaces per instance, with identical rejection semantics, an anonymous registration-mode probe, a per-user `chat_allowed` flag, and SPA affordances that adapt without weakening the server-side enforcement.

## ADDED Requirements

### Requirement: Registration policy and mode

The service SHALL allow registration only when `REGISTRATION_ENABLED` is true and `REGISTRATION_ALLOWLIST` is empty or contains the exact, case-sensitive submitted username. Anonymous callers SHALL be able to read the derived mode from `GET /api/config`: `closed` when disabled, `open` when enabled with an empty list, `invite_only` when enabled with a non-empty list. The probe SHALL NOT include usernames or allowlist contents. Changes SHALL apply on the next backend start.

#### Scenario: Open mode

- **WHEN** `REGISTRATION_ENABLED` is unset and `REGISTRATION_ALLOWLIST` is empty
- **THEN** `GET /api/config` returns `{"registration_mode":"open"}` and any valid username can register

#### Scenario: Invite-only mode

- **WHEN** registration is enabled with a non-empty allowlist
- **THEN** `GET /api/config` returns `invite_only`, a listed username can register, and an unlisted username is rejected

#### Scenario: Closed mode

- **WHEN** `REGISTRATION_ENABLED=false`
- **THEN** `GET /api/config` returns `closed` and every registration attempt is rejected, while existing accounts can still log in

#### Scenario: Malformed allowlist fails startup

- **WHEN** an allowlist contains an empty segment, an entry shorter than 3 or longer than 50 characters, or a control character
- **THEN** `config.Load()` returns an error naming the variable and the process does not start

### Requirement: Registration rejection is indistinguishable

A rejected registration SHALL return the same HTTP status (403), the same static error message, and no details, whether registration is disabled or the username is not listed. The rejection SHALL occur before the bcrypt work (HTTP schema validation still runs at the edge first, so an out-of-schema body may return 400), and SHALL NOT echo the submitted username.

#### Scenario: Disabled and unlisted look identical

- **WHEN** one request hits a closed deployment and another hits an invite-only deployment with an unlisted username
- **THEN** both responses carry identical status, error text, and an empty details field

### Requirement: AI allowlist enforcement

When `AI_ALLOWLIST` is non-empty, every AI surface SHALL require the authenticated caller's username to be listed: all `/api/chat*` routes, `POST /api/chat/approvals`, and all `/api/me/llm-config*` operations. A non-listed caller SHALL receive 403 with a static message; an unauthenticated caller SHALL receive 401 before the gate is consulted. An empty allowlist SHALL allow every authenticated user. `GET /api/features` SHALL remain available to authenticated callers so it can report the decision.

#### Scenario: Non-listed user is denied on every AI route

- **WHEN** a user outside `AI_ALLOWLIST` calls a chat, approvals, or LLM-config route
- **THEN** the response is 403 and the underlying handler is not invoked

#### Scenario: Listed and empty-list users are allowed

- **WHEN** the caller is listed, or the allowlist is empty
- **THEN** the request passes the gate and reaches the existing handler

### Requirement: chat_allowed reflects deployment and caller

`GET /api/features` SHALL include `chat_allowed`, true only when `LLM_ENABLED` is true and the caller passes `AI_ALLOWLIST`. `agent_enabled` SHALL remain independent of it.

#### Scenario: LLM disabled

- **WHEN** `LLM_ENABLED=false`
- **THEN** `chat_allowed` is false even for a listed user

#### Scenario: Listed vs unlisted

- **WHEN** `LLM_ENABLED=true` and a non-empty allowlist is configured
- **THEN** `chat_allowed` is true for a listed caller and false for an unlisted caller

### Requirement: SPA adapts and fails open

The SPA SHALL hide the register affordance and show a closed hint when the public mode is `closed`; it SHALL keep the register form and show an invite hint when `invite_only`; a register 403 SHALL render a dedicated inline error. When `chat_allowed` is false, the SPA SHALL hide the chat entry point, chat panel, LLM gear, LLM config modal, and the Knowledge tab (KB embeddings are configured through the same BYOK settings) and SHALL NOT auto-open the chat. If either probe is missing, unreachable, or lacks the field, the SPA SHALL behave as the permissive pre-gate UI; the backend remains the enforcement point.

#### Scenario: Closed registration UI

- **WHEN** `GET /api/config` reports `closed`
- **THEN** the register toggle is absent and the closed hint is visible while login still works

#### Scenario: Probe failure

- **WHEN** the features or public-config probe fails or the field is absent
- **THEN** the SPA shows the permissive UI and sends requests that the server may reject with 403

#### Scenario: Denied AI user

- **WHEN** `chat_allowed=false`
- **THEN** the chat button, chat panel, LLM gear, LLM config modal, and Knowledge tab are not rendered

### Requirement: Eval compatibility

The golden-eval provider SHALL accept an optional fixed `EVAL_USERNAME`; when set, it SHALL replace the per-run generated username, so a gated deployment can allowlist that account. The default behavior SHALL remain the per-run stamped username.

#### Scenario: Fixed eval username

- **WHEN** `EVAL_USERNAME=eval-golden-svc` is set
- **THEN** the provider authenticates or registers that username instead of the stamped one
