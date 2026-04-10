#!/usr/bin/env sh
if [ -z "$husky_skip_init" ]; then
  readonly debug="${HUSKY_DEBUG:-0}"

  husky_debug() {
    if [ "$debug" = "1" ]; then
      echo "husky:debug $1"
    fi
  }

  trap "exit 1" TERM INT

  if [ "$HUSKY" = "0" ]; then
    husky_debug "HUSKY env var is set to 0, skipping hooks"
    exit 0
  fi

  if [ -f ~/.huskyrc ]; then
    husky_debug "sourcing ~/.huskyrc"
    . ~/.huskyrc
  fi

  export husky_skip_init=1
  sh -e "$0" "$@"
  exitCode="$?"

  if [ "$exitCode" != "0" ]; then
    echo "husky - $0 hook exited with code $exitCode (error)"
  fi

  exit "$exitCode"
fi
