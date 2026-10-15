# chat-approval-card Specification

## Purpose
The SPA-side approval card for a paused mutating agent tool call: it presents the pending confirmation inside the chat turn, records the user's decision, and resolves to a terminal state — including the deadline countdown that closes an unanswered request.

## Requirements

### Requirement: Approval card represents a paused confirmation request

The SPA SHALL render one approval card per `confirm_request` received on the live stream, inside the chat turn and actionable without expanding any disclosure. The card MUST carry the tool name and its (producer-masked) arguments, and MUST present Approve and Deny affordances while the request is pending. The card is ephemeral: replaying a stored session does not restore it.

#### Scenario: Pending card is actionable in place

- **WHEN** the stream delivers a `confirm_request` for a mutating tool call
- **THEN** a card renders in the turn showing the tool and its arguments, with Approve and Deny buttons

#### Scenario: Replay does not restore cards

- **WHEN** a stored session is replayed after the fact
- **THEN** no approval card is rendered for its historical `confirm_request` frames

### Requirement: Decisions are recorded through the approval route

Approving or denying SHALL submit the decision to the approval route; the card SHALL resolve to `approved` or `denied` from the recorded outcome. A failed submission MUST keep the card pending with a static client-side hint so the user can retry. The route's 404 answer for an approval id that can no longer resolve SHALL resolve the card as `expired` — a retry could never succeed, so no retry affordance is offered.

#### Scenario: Approve resolves the card

- **WHEN** the user approves and the decision is recorded
- **THEN** the card shows the approved state and no longer offers Approve/Deny

#### Scenario: Failed submission keeps the card retryable

- **WHEN** the decision submission fails with a retryable error
- **THEN** the card stays pending, shows a static hint, and the buttons remain available

#### Scenario: Dead approval id resolves as expired

- **WHEN** the decision submission answers the route's 404
- **THEN** the card resolves as expired without a retry affordance

### Requirement: The pending card shows the time remaining until expiry

While a card is pending, it SHALL display a live countdown of the time remaining until the frame's `expires_at`, formatted in minutes and seconds and updating at least once per second. The countdown MUST NOT itself transmit anything to the server.

#### Scenario: Countdown ticks while pending

- **WHEN** a card is pending with an expiry 300 seconds away
- **THEN** it displays `Expires in 5:00` and the displayed time decreases as the window elapses

### Requirement: An unanswered request expires at its deadline

When `expires_at` passes with no recorded decision, the card SHALL resolve to `expired` — the countdown runs out, the Approve/Deny affordances are no longer offered, and the expired copy is shown. The expiry MUST NOT submit a decision to the server: the agent-side gate closes the unanswered approval as denied at the same deadline and the run continues. A decision recorded before the deadline is authoritative — a local expiry MUST NOT overwrite a recorded `approved`/`denied` outcome, whichever of the two answers arrives first.

#### Scenario: Deadline passes unanswered

- **WHEN** the countdown reaches zero with no decision recorded
- **THEN** the card resolves as expired without transmitting a decision, and no further Approve/Deny affordance is shown

#### Scenario: Recorded decision is authoritative

- **WHEN** a decision is recorded around the deadline
- **THEN** the card's final state reflects the recorded outcome, never a local-only expiry

### Requirement: Runs without a rendered card expire server-side

Where no card is rendered — a detached run drops stream frames by design, and replay restores no cards — the SPA SHALL NOT fabricate a countdown or an expiry event. The agent-side TTL remains the sole authority: the approval closes as denied at `expires_at` and the run proceeds.

#### Scenario: Detached run's approval closes server-side

- **WHEN** a run is detached while its approval is pending
- **THEN** no countdown or expiry UI exists for it, and the approval is closed as denied by the agent at its deadline
