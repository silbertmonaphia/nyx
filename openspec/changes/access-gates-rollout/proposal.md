# Proposal

## Why

The `access-gates` implementation ships defaults that preserve the current open behavior. Closing or narrowing registration and AI access happens by flipping env vars on a live instance, which is an operator action against production and needs its own smoke, rollback, and record — kept out of the implementation change per `openspec/config.yaml`.

## What Changes

- Domestic instance: set `REGISTRATION_ALLOWLIST` and `AI_ALLOWLIST` in `backend/.env`, restart the backend, and verify the UI and API gates end-to-end. `REGISTRATION_ENABLED` stays at its default `true`; the allowlist is what closes public sign-up.
- Overseas instance: deploy the same code with no new env; verify registration and AI stay fully open.
- Record the closed-mode drill, the eval accommodation (`EVAL_USERNAME`), and the rollback procedure.

## Impact

- Configuration only; no code changes in this sibling.
- The frontend does not need a rebuild: it reads `GET /api/config` and `GET /api/features` from the backend.
