# Proposal

## Why

The agent's confirmation gate closes an unanswered approval as denied at `expires_at` (default 300s, `PYTHON_AGENT_APPROVAL_TTL_SECONDS`). The SPA card, however, renders a pending state with live Approve/Deny buttons for the whole window and only reveals the expiry after a late click bounces off the endpoint's 404. With long runs now surviving to their natural end (the chat-proxy stream-cap fix, 2026-10-14), the 300s window is reachable in normal use — the deadline needs to be visible and self-closing.

## What Changes

- The pending confirmation card shows a live countdown ("Expires in m:ss") derived from the frame's existing `expires_at`.
- When `expires_at` passes with no decision, the card flips to its existing `expired` state (no Approve/Deny buttons, expired copy) without any server call — the agent closes the approval as denied at the same deadline and the run continues.
- New store action `expireApproval(approvalId)` (pending-only guarded) is the single writer for the local flip; a decision POST that lands first always wins (a recorded `approved`/`denied` is never overwritten).
- Scope is the rendered card only. A detached run renders no card (attachment-gated stream, `agent-chat-background-runs`), so there is nothing to count down — the server-side TTL remains the authority there. Replay does not restore confirm cards (pre-existing gap); restoring them is a separate follow-up, out of scope.
- No wire, backend, Go proxy, or agent changes: `expires_at` is already on the `confirm_request` frame.

## Capabilities

### New Capabilities

- `chat-approval-card`: the SPA-side approval-card lifecycle for a paused mutating tool call — render from `confirm_request`, decision recording via `POST /api/chat/approvals`, terminal outcomes (approved / denied / expired), and the deadline countdown with auto-flip.

### Modified Capabilities

(none)

## Impact

- **Code**: `frontend/src/features/chat/components/ConfirmationCard.tsx` (countdown UI + card-scoped timer), `frontend/src/features/chat/store/chatStore.ts` (`expireApproval` + pending-only patch guard), `frontend/src/features/chat/components/ChatPanel.tsx` (wiring), plus their vitest suites and one canned-SSE case in `frontend/tests/e2e/chat-feed-approval.spec.ts` (short `expires_at` → flip + no decision POST).
- **Wire/backend**: none. The `confirm_request` frame already carries `expires_at`; the agent-side TTL and decision semantics are unchanged.
- **Docs**: the frontend roadmap entry for the approval UI, if present.
- **Dependencies**: none new.
- **Rollback**: revert the commit (no flag; no rollout gates foreseen — the change is completable entirely in the dev/CI environment, so no sibling rollout change).
