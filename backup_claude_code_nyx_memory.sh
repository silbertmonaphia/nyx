#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SRC="$HOME/.claude/projects/-home-smona-nyx/memory"
DEST="${1:-$SCRIPT_DIR/backups_claude_code_memory}"
KEEP=5
STAMP=$(date +%Y%m%dT%H%M%S)

[ -d "$SRC" ] || {
    echo "memory dir missing: $SRC" >&2
    exit 1
}

mkdir -p "$DEST"
OUT="$DEST/nyx-memory-$STAMP.tar.gz"

tar -czf "$OUT" -C "$(dirname "$SRC")" "$(basename "$SRC")"
tar -tzf "$OUT" >/dev/null

echo "backup ok: $OUT ($(du -h "$OUT" | cut -f1))"

ls -1t "$DEST"/nyx-memory-*.tar.gz 2>/dev/null | tail -n +"$((KEEP + 1))" | while IFS= read -r old; do
    rm -f "$old"
    echo "pruned: $old"
done
