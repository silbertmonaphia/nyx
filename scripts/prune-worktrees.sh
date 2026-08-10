#!/usr/bin/env bash
# Remove Claude Code worktrees under .claude/worktrees/ that are safe to delete:
# a worktree qualifies only if its tree is clean AND its branch is already an
# ancestor of a base branch. Anything else is left alone and reported.
#
# Run manually, or via the SessionStart hook in .claude/settings.json.
# Pass --dry-run to report without deleting.
#
# SAFETY: no -e so a per-worktree failure doesn't abort the whole sweep.

set -uo pipefail

# Edit BASE_BRANCHES if you branch from something else; unmerged branches
# will otherwise always read as unsafe and accumulate forever.

BASE_BRANCHES=(main mvp)
DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
worktree_dir="$repo_root/.claude/worktrees"
[[ -d "$worktree_dir" ]] || exit 0

# The worktree the current shell sits in must never be removed.
current=$(git rev-parse --show-toplevel 2>/dev/null)

removed=() kept=()

while IFS= read -r wt; do
    [[ "$wt" == "$worktree_dir"/* ]] || continue
    [[ "$wt" == "$current" ]] && continue

    name=${wt#"$worktree_dir"/}
    branch=$(git -C "$wt" symbolic-ref --short -q HEAD)

    # Ignored files (node_modules, dist) don't count as dirty; tracked edits and
    # untracked sources do.
    if [[ -n $(git -C "$wt" status --porcelain 2>/dev/null) ]]; then
        kept+=("$name — uncommitted changes")
        continue
    fi

    if [[ -z "$branch" ]]; then
        kept+=("$name — detached HEAD")
        continue
    fi

    merged=0
    for base in "${BASE_BRANCHES[@]}"; do
        git -C "$repo_root" rev-parse --verify -q "$base" >/dev/null || continue
        if git -C "$repo_root" merge-base --is-ancestor "$branch" "$base"; then
            merged=1
            break
        fi
    done

    if (( ! merged )); then
        kept+=("$name — $branch not merged into ${BASE_BRANCHES[*]}")
        continue
    fi

    if (( DRY_RUN )); then
        removed+=("$name (would remove)")
        continue
    fi

    if git -C "$repo_root" worktree remove "$wt" 2>/dev/null; then
        git -C "$repo_root" branch -d "$branch" >/dev/null 2>&1
        removed+=("$name")
    else
        kept+=("$name — git refused to remove it")
    fi
done < <(git -C "$repo_root" worktree list --porcelain | awk '/^worktree /{print $2}')

(( ${#removed[@]} == 0 && ${#kept[@]} == 0 )) && exit 0

echo "worktrees:"
for r in ${removed+"${removed[@]}"}; do echo "  removed  $r"; done
for k in ${kept+"${kept[@]}"}; do echo "  kept     $k"; done
if (( ${#kept[@]} > 0 )); then
    echo "  (stale ones you no longer want: git worktree remove --force <path>)"
fi
