# Design

## Context

See proposal.md for motivation. Constraints that shape the approach:

- `ChatStepConfirm` already carries `expiresAt` (epoch seconds, straight off the `confirm_request` frame) and an `ApprovalOutcome` union that already includes `expired`; the expired presentation and copy exist today via the 404 path.
- The agent gate closes an unanswered approval as denied at the same `expires_at` and the run continues; the denial reaches the UI through the paused call's regular `tool_result` frame.
- The store is ephemeral Zustand; the card is the only renderer of a confirm record, and only while its stream is attached (`agent-chat-background-runs` gates stream writes on attachment).

## Goals / Non-Goals

**Goals:**
- Make the deadline visible on the pending card and have the card close itself at the deadline with no user action.
- Keep the recorded decision authoritative in every race.
- Zero wire/backend/agent changes.

**Non-Goals:**
- Any server push of expiry state, new frame fields, or TTL changes.
- Countdown/expiry for runs without a rendered card (detached runs; replay does not restore cards).
- Restoring confirm cards on replay — a separate pre-existing follow-up.

## Decisions

**The countdown timer lives in the card, not the store.** The display need is per-card and ends when the card unmounts; the card derives remaining time from the absolute `expiresAt` on each one-second tick. A store-side timer registry was considered (schedule on `confirm_request`) and rejected: it adds lifecycle/janitor semantics to the store for a display-only concern, while the card already re-renders on store changes.

**Expiry is a local presentation flip, not a decision submission.** At zero the card calls a new pending-only store action (`expireApproval`), which patches the confirm record to `expired` exactly as the 404 path does. Submitting an explicit deny on expiry was rejected: the gate already closes the unanswered approval server-side, and a timer-driven network call would add a second decision source for the same outcome.

**Authority order: recorded outcome > local expiry.** The local flip is guarded to only ever touch a `pending` record, and a decision response overwrites the record when it lands. So the boundary race — a decision recorded just before the deadline while the countdown also fires — converges on the recorded `approved`/`denied`, which is what the server actually executed.

**Remaining time derives from the absolute `expiresAt`, never from a decremented local counter.** Browsers throttle timers in backgrounded tabs and suspend them across sleep; a decremented counter would drift, while re-deriving from `Date.now()` self-corrects on the first tick after wake. Trade-off: browser/server clock skew shifts the displayed value; the server's typed 404 remains the backstop, so skew is cosmetic.

**The countdown is announced as a timer, not a live region.** A one-second `aria-live` region would spam screen readers; `role="timer"` without live announcements keeps the deadline accessible without noise.

## Risks / Trade-offs

- [A backgrounded tab's flip is late until the next tick] → cosmetic: the server-side TTL already closed the call; the card catches up on wake and the denial arrives via `tool_result`.
- [Clock skew between browser and agent shifts the countdown] → cosmetic for display; any actually-late or actually-early decision is resolved by the server's expiry check (404 → expired, recorded → outcome).
- [The local flip could visually preempt a decision that is in flight] → the pending-only guard plus the decision-success overwrite resolve to the recorded outcome; the visual state converges within the same second.

## Migration Plan

None required: frontend-only diff, no persistence, no flag. Rollback is reverting the commit.

## Open Questions

None blocking. The replay-restores-cards gap is named in the proposal as a separate follow-up.
