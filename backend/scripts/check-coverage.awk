#!/usr/bin/awk -f
#
# Parses `go test -cover` output and exits non-zero if any non-ignored
# package drops below the --min threshold. Used by `make coverage-check`.
#
# Usage:
#   go test -cover ./... | awk -v min=70 -v ignore="pkg1 pkg2" -f check-coverage.awk
#
# Exits 0 if every measured package meets the floor (or is in the
# ignore list). Exits 1 if any package regresses.

BEGIN {
    if (min == "") min = 70
    n = split(ignore, arr, " ")
    for (i = 1; i <= n; i++) excluded[arr[i]] = 1
}

# Match the success line for a package that has test files. Other
# lines (`?` no-tests, `FAIL`, build errors) carry no coverage
# measurement, so we skip them — build errors are surfaced via
# pipefail in the Make recipe's SHELLFLAGS.
$0 ~ /^ok[[:space:]]/ {
    pkg = $2
    pct = ""
    # `go test -cover` prints `coverage: NN.N% of statements` as
    # separate tab/whitespace-separated fields after the package.
    for (i = 1; i <= NF; i++) {
        if ($i == "coverage:") {
            pct = $(i+1)
            sub(/%$/, "", pct)
            break
        }
    }

    if (excluded[pkg]) {
        printf("skip    %-50s %s%%  (in COVERAGE_IGNORE)\n", pkg, pct)
        next
    }
    if (pct == "" || pct + 0 < min + 0) {
        printf("REGRESS %-50s %s%% < %d%%\n", pkg, pct, min)
        failed = 1
    } else {
        printf("ok      %-50s %.1f%%\n", pkg, pct)
    }
}

END { exit failed + 0 }