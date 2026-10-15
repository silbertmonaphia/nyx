#!/usr/bin/env bash
# Bump version constants across the repo.
#
# Invoked by @semantic-release/exec's `prepareCmd` after a release has
# been computed but before the release commit is created. Updates the
# source-of-truth files so the next build carries the new version:
#
#   backend/internal/platform/observability/tracing.go  → ServiceVersion
#   frontend/package.json                              → "version"
#
# Usage: bump-version.sh <semver>
#   e.g. bump-version.sh 0.4.1
#
# Idempotent: re-running on the same version is a no-op.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <semver>" >&2
  exit 64
fi

VER="$1"

# Validate: semver MAJOR.MINOR.PATCH, optional pre-release / build metadata.
# Strict enough to catch typos; loose enough to accept 1.2.3-rc.1+build.7.
if ! [[ "$VER" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]; then
  echo "error: '$VER' is not a valid semver string" >&2
  exit 65
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# backend: var ServiceVersion = "..." in tracing.go
sed -i -E "s|(var ServiceVersion = \")[^\"]*(\")|\1${VER}\2|" \
  "$REPO_ROOT/backend/internal/platform/observability/tracing.go"

# frontend: "version": "..." in package.json (first match wins)
sed -i -E "0,/(\"version\": \")[^\"]*(\")/s||\1${VER}\2|" \
  "$REPO_ROOT/frontend/package.json"

echo "bumped to ${VER}"
