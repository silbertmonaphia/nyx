#!/usr/bin/env bash
# Print a one-line reminder to run /review when the working tree or HEAD
# has changes relative to the project base. Wired into the Stop hook in
# .claude/settings.json — fires at the end of every Claude turn.
#
# We deliberately do NOT spawn the test-reviewer agent from the hook.
# Spawning another Claude session on every turn burns tokens and adds
# latency; the slash command `/review` is the manual trigger for the
# heavy review pass. The hook just nudges.
#
# Exit: 0 always. Non-zero exits would block the turn, which would be
# hostile UX for trivial diffs (comments, formatting).

set -uo pipefail

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
cd "$repo_root" || exit 0

# Pick the first base branch that exists locally.
base=""
for b in main mvp; do
    if git rev-parse --verify -q "$b" >/dev/null 2>&1; then
        base=$b
        break
    fi
done

# Working tree dirty?
dirty=$(git diff --name-only HEAD 2>/dev/null | head -1)

# Unpushed HEAD-ahead changes vs base?
unpushed=""
if [ -n "$base" ]; then
    unpushed=$(git diff --name-only "$base"...HEAD 2>/dev/null | head -1)
fi

if [ -n "$dirty$unpushed" ]; then
    echo ""
    echo "→ Changes detected. Run /review (test-reviewer agent) before committing or pushing."
    echo ""
fi

exit 0
