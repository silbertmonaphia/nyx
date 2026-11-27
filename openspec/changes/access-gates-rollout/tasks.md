# Tasks

> Production env flips for `access-gates`. Implementation is code-complete and archives independently; these gates run against the deployed instances. Do not check a box without recording the observed result.

## 1. Preconditions

- [ ] 1.1 Collect the exact, case-sensitive usernames that must register and use AI. Include the operator's own existing account in `AI_ALLOWLIST` — otherwise the operator loses the chat surface too.
- [ ] 1.2 Confirm the domestic instance has at least one existing account (registration gates never block login). For a fresh database, register the first account before closing registration.
- [ ] 1.3 Review `backend/.env.example` → "Access gates" for the empty-list semantics and the restart requirement.

## 2. Domestic instance (gated)

- [ ] 2.1 Add to `backend/.env`:
      `REGISTRATION_ALLOWLIST=<operator,friend,...>`
      `AI_ALLOWLIST=<operator,friend,...>`
      (`REGISTRATION_ENABLED` stays unset/true; leave the lists empty to keep the instance open.)
- [ ] 2.2 Restart the backend (`make stop && make dev-backend DETACH=1`, or the compose equivalent). Confirm the boot log prints `registration_mode=invite_only` and the allowlist sizes, never usernames.
- [ ] 2.3 API smoke: `GET /api/config` = `{"registration_mode":"invite_only"}`; register a listed username → 201 (or 409 if already registered); register an unlisted username → 403 with the static message and no details; `GET /api/features` for a listed caller → `chat_allowed:true`; for an unlisted caller → `false`; unlisted `POST /api/chat` and `GET /api/me/llm-config` → 403.
- [ ] 2.4 Browser smoke (anonymous): the login dialog shows the invite-only hint, the register form is present, and an unlisted username gets the inline "Registration is not available." error. As an unlisted signed-in user: chat button, panel, and LLM gear are absent; as a listed user they are present.
- [ ] 2.5 Confirm no frontend rebuild was needed (the SPA adapted after reload through the backend probes).

## 3. Overseas instance (open)

- [ ] 3.1 Deploy the same code with no `REGISTRATION_*` / `AI_ALLOWLIST` variables set.
- [ ] 3.2 Verify `GET /api/config` = `{"registration_mode":"open"}`, a fresh username can register, and `chat_allowed` is true for any authenticated user.

## 4. Closed-mode drill (optional, staging or side instance)

- [ ] 4.1 Start with `REGISTRATION_ENABLED=false`; verify `GET /api/config` = `closed`, the SPA hides the register toggle behind the closed hint, and every `POST /api/register` returns 403.
- [ ] 4.2 Restore the previous env and restart.

## 5. Evals

- [ ] 5.1 For `make eval-golden` against a gated stack: set `EVAL_USERNAME=<fixed username>` and add it to both allowlists (the registration allowlist is only needed for its first creation). Otherwise run evals against a non-gated stack and record that choice.

## 6. Rollback

- [ ] 6.1 Remove `REGISTRATION_ALLOWLIST` and `AI_ALLOWLIST` from `backend/.env` and restart. Verify open registration returns, `chat_allowed` is true for every authenticated user, and `/api/config` reports `open`.
- [ ] 6.2 If the instance must stay closed at the network edge instead, record the web-server/proxy rule and note that it is outside this change.

## 7. Record

- [ ] 7.1 Paste the smoke outputs (mode, status codes, UI observations) into this file or the ops log, then archive this change (operator / PM owns the archive).
