#!/bin/sh
set -eu

MODE="${1:-run}"

case "$MODE" in
    run|-n|-a) ;;
    *)
        echo "usage: $0 [-n dry-run | -a include the invoking pane]"
        echo "  default: send /clear to every tmux pane running claude, except the pane on this script's own TTY"
        exit 1
        ;;
esac

SELF_TTY=$(ps -o tty= -p $$ 2>/dev/null | tr -d ' ')

panes=$(tmux list-panes -a -F '#{pane_id}|#{pane_tty}|#{pane_current_command}' 2>/dev/null | grep -E '\|(claude|node)$' || true)

if [ -z "$panes" ]; then
    echo "no claude panes found"
    exit 0
fi

for entry in $panes; do
    pane=${entry%%|*}
    rest=${entry#*|}
    tty=${rest%%|*}
    cmd=${rest##*|}
    if [ "$MODE" != "-a" ] && [ -n "$SELF_TTY" ] && [ "$SELF_TTY" != "?" ] && [ "$tty" = "$SELF_TTY" ]; then
        echo "skipped (own tty): $pane ($cmd)"
        continue
    fi
    if [ "$MODE" = "-n" ]; then
        echo "would clear: $pane ($cmd)"
    else
        tmux send-keys -t "$pane" '/clear' Enter
        echo "cleared: $pane ($cmd)"
    fi
done
