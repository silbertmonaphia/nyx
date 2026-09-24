---
description: Spawn the test-reviewer agent against the current branch diff vs the project base (main, fallback mvp). Use after a feature/fix lands, before committing or pushing, to catch correctness / security / CLAUDE.md violations.
allowed-tools: ["Agent", "Bash", "Read", "Glob", "Grep"]
---

# /review

You are running the project's review pass. Use the Agent tool to spawn the
`test-reviewer` sub-agent against the current branch.

## Steps

1. Pick the base ref:
   - `main` if it exists locally (`git rev-parse --verify main`).
   - else `mvp` (the current branch's parent in this project's CI config).
2. Spawn the `test-reviewer` agent via the Agent tool with the prompt:

   > Review the diff against the base ref. Scope is `git diff <base>...HEAD`.
   > Read `.claude/agents/test-reviewer.md` for the full protocol.
   > Report findings using `ReportFindings`. If CONFIRMED issues remain,
   > the verdict must be `request changes` — do not silently approve.

3. After the agent returns, surface the verdict + CONFIRMED findings to
   the user. If the verdict is `request changes`, list the must-fix items
   inline so the user can act without opening the report separately.

## When to use

- Right before `git commit` for a non-trivial change.
- Right before `git push` if the branch already has commits but no review yet.
- After a `/clear` to re-baseline the project against accumulated work.

## When NOT to use

- Trivial changes (typo, version bump, single-line fix) — overkill.
- Frontend-only formatting churn — `npm run lint` is enough.
- For harness / docs work (this file is harness work itself).

See Harness.md #3 for the design rationale.
