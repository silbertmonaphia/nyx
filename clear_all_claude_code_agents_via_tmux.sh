#!/bin/sh
set -eu

MODE="${1:-run}"

case "$MODE" in
    run|-n|-a) ;;
    *)
        echo "usage: $0 [-n dry-run | -a include the invoking pane]"
        echo "  default: send /clear to every tmux pane running claude, except the pane this script runs in"
        exit 1
        ;;
esac

SELF_PANE=$(tmux display-message -p '#{pane_id}' 2>/dev/null || true)

panes=$(tmux list-panes -a -F '#{pane_id}|#{pane_current_command}' 2>/dev/null | grep -E '\|(claude|node)$' || true)

if [ -z "$panes" ]; then
    echo "no claude panes found"
    exit 0
fi

for entry in $panes; do
    pane=${entry%%|*}
    cmd=${entry##*|}
    if [ "$MODE" != "-a" ] && [ -n "$SELF_PANE" ] && [ "$pane" = "$SELF_PANE" ]; then
        echo "skipped (invoking pane): $pane ($cmd)"
        continue
    fi
    if [ "$MODE" = "-n" ]; then
        echo "would clear: $pane ($cmd)"
    else
        tmux send-keys -t "$pane" '/clear' Enter
        echo "cleared: $pane ($cmd)"
    fi
done
