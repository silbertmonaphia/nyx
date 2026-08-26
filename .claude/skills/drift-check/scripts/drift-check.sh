#!/usr/bin/env bash
# Backend drift checks: gofmt + sqlc-diff + openapi-diff + golangci-lint.
# Skips frontend-only commits entirely.
#
# Usage: bash .claude/skills/drift-check/scripts/drift-check.sh
# Exit:  0 if nothing backend was touched or all checks passed,
#        1 if any check failed.
#
# SAFETY: no -e — one failing check must not abort the rest. Aggregate and report.

set -uo pipefail

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || {
    echo "error: not a git repository" >&2
    exit 1
}
cd "$repo_root"

# Pick the first base branch that exists locally.
base=""
for b in main mvp; do
    if git rev-parse --verify -q "$b" >/dev/null 2>&1; then
        base=$b
        break
    fi
done

# Detect backend touches from working tree + staged + (vs-base if known).
touched=0
scan() {
    while IFS= read -r f; do
        [[ "$f" == backend/* ]] && touched=1
    done
}
scan < <(git status --porcelain 2>/dev/null | awk '{print $2}')
scan < <(git diff --cached --name-only 2>/dev/null)
if [[ -n "$base" ]]; then
    scan < <(git diff --name-only "$base"...HEAD 2>/dev/null)
fi

if (( !touched )); then
    echo "no backend changes — drift check skipped"
    exit 0
fi

cd "$repo_root/backend"

# ---- run + aggregate -------------------------------------------------------
pass=0
fail=0

check() {
    local name=$1 cmd=$2
    printf '[ RUN  ] %s\n' "$name"
    if out=$(eval "$cmd" 2>&1); then
        printf '[  OK  ] %s\n' "$name"
        pass=$((pass + 1))
    else
        rc=$?
        printf '[ FAIL ] %s (exit %d)\n' "$name" "$rc"
        printf '%s\n' "$out" | sed -n '1,30p' | sed 's/^/    /'
        echo
        fail=$((fail + 1))
    fi
}

# Format: surface unformatted files explicitly so the FAIL row isn't silent.
unfmt=$(find . -name '*.go' -not -path './vendor/*' -exec gofmt -l {} +)
if [[ -n "$unfmt" ]]; then
    printf '[ FAIL ] format — gofmt drift\n'
    printf '%s\n' "$unfmt" | sed 's/^/    /'
    echo
    fail=$((fail + 1))
else
    printf '[  OK  ] format\n'
    pass=$((pass + 1))
fi

check "drift:sqlc"    "make sqlc-diff"
check "drift:openapi" "make openapi-diff"
check "lint"          "golangci-lint run --timeout=5m"

echo
echo "summary: $pass ok, $fail fail"
(( fail == 0 ))