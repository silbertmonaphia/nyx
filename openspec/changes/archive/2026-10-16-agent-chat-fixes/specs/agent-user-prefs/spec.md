# Spec Delta

## MODIFIED Requirements

### Requirement: Planner prompt injection with empty-set parity

Before each run, the service SHALL load the user's preferences (`updated_at DESC`, at most `PYTHON_AGENT_PREFS_MAX_INJECTED` rows) and, when non-empty, append a rendered preferences block to BOTH the planner system prompt and the executor prompts (the `llm` step path and the `react` step path — the node that writes the user-visible answer). When the user has no preferences, or `PYTHON_AGENT_PREFS_ENABLED=false`, every prompt path SHALL be byte-identical to its pre-injection form.

#### Scenario: Preferences reach the planner

- **WHEN** the user has `{snack: 鱿鱼丝}` and sends a message
- **THEN** the planner system prompt contains the rendered preferences block with that entry

#### Scenario: Empty prefs leave the prompt untouched

- **WHEN** the user has zero preferences
- **THEN** the planner and executor prompt bytes are identical to their pre-injection forms (regression-test pinned for every path)

#### Scenario: Kill switch disables extraction and injection together

- **WHEN** `PYTHON_AGENT_PREFS_ENABLED=false`
- **THEN** no extraction task fires and the planner and executor prompts omit the block, even for users with stored preferences (rows remain in the table)

#### Scenario: Preferences reach the executor that writes the reply

- **WHEN** the user has `{snack: 鱿鱼丝}` and sends a message
- **THEN** the executor's system message on both the `llm` step path and the `react` step path contains the rendered preferences block, so the node that writes the user-visible answer carries the stored preference

#### Scenario: A recall question is answered from the same prompt that writes the reply

- **WHEN** the user previously stated a preference (stored) and now asks a recall question ("我爱吃什么" / "what do I like to eat?")
- **THEN** the preferences block is present in the executor prompt that produces the reply, without the user restating the preference (model compliance remains best-effort; prompt plumbing is what is asserted)
