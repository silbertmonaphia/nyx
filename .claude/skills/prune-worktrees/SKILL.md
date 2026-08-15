---
name: prune-worktrees
description: Prune Claude Code worktrees under .claude/worktrees/ that are clean and already merged into a base branch. Invoke when the user wants to clean up stale worktrees, or when worktree count is visibly growing.
---

# Prune Worktrees

Remove Claude Code worktrees under `.claude/worktrees/` that are safe to delete:
a worktree qualifies only if its tree is clean AND its branch is already an
ancestor of a base branch. Anything else is left alone and reported.

## Usage

Run the script from the repo root:

```bash
bash scripts/prune-worktrees.sh
```

Add `--dry-run` to report without deleting:

```bash
bash scripts/prune-worktrees.sh --dry-run
```

## What it does

- Iterates every worktree under `.claude/worktrees/`
- Skips the current shell's worktree (never removes the one you're in)
- Keeps worktrees with uncommitted changes, detached HEAD, or branches not merged into `main` or `mvp`
- For the rest, runs `git worktree remove` followed by `git branch -d`
- Prints a summary of removed vs kept worktrees

If anything is kept, the output reminds you to use `git worktree remove --force <path>` for stale ones you no longer want.

## Customize

Edit `BASE_BRANCHES` at the top of `scripts/prune-worktrees.sh` if you branch from a different base — unmerged branches will otherwise always read as unsafe and accumulate forever.
