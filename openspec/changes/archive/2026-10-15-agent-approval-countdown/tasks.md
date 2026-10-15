# Tasks

## 1. Store

- [x] 1.1 Add the pending-only guard to the approval-card patch path and the new `expireApproval(approvalId)` action; verify with chatStore tests that a pending card flips to `expired` and that decided cards (`approved` / `denied` / `expired`) are left untouched
- [x] 1.2 Verify a decision recorded after a local expiry still wins (submitApproval success overwrites the record), pinned in a chatStore test

## 2. Card UI

- [x] 2.1 Add the countdown display (`Expires in m:ss`, `role="timer"`, no live region) plus the one-second card-scoped timer deriving remaining time from the absolute `expiresAt`; wire the expiry callback through `ChatPanel`; verify with a component test (fake timers) that a card 300 seconds out renders `Expires in 5:00` and that the displayed time decreases
- [x] 2.2 Add the auto-flip: when the deadline passes with no decision the card invokes the expiry callback exactly once and the Approve/Deny affordances are gone once the store record flips; verify with component tests (fake timers) for the pending→expired transition and that a decided card never invokes the callback
- [x] 2.3 Update the existing ConfirmationCard tests for the new required prop and confirm the full vitest suite still passes (`cd frontend && npm test`)

## 3. E2E

- [x] 3.1 Extend the canned-SSE approval spec with a short-`expires_at` case: the card shows the countdown, flips to the expired state at the deadline, and the in-spec mock server observes NO `POST /api/chat/approvals` from the expiry; verify by running the spec against the e2e side stack (`PLAYWRIGHT_BASE_URL=http://localhost:5274 npx playwright test tests/e2e/chat-feed-approval.spec.ts`)

## 4. Docs

- [x] 4.1 Locate the approval-card doc surface (frontend roadmap / chat feature docs mentioning the confirmation card) and record the countdown + auto-expiry behavior there; verify by grepping the touched doc for the countdown description — surface verified absent (grep across *.md outside openspec/ finds no approval-card section and there is no frontend chat README); the `chat-approval-card` capability spec created by this change is the recorded home

## 5. Integration

- [x] 5.1 Run the full frontend gates on the final revision: `cd frontend && npm test`, `npm run lint`, `npm run build`, and the canned approval e2e once more; verify all pass
