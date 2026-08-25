#!/usr/bin/env bash
set -euo pipefail

RUN_DIR="$HOME/.config/agent-tracker/run"
FILE="$RUN_DIR/latest_notified.txt"
[[ -s "$FILE" ]] || exit 0

IFS=':::' read -r sid wid pid < <(tr -d '\n' < "$FILE")
[[ -n "${sid:-}" && -n "${wid:-}" && -n "${pid:-}" ]] || exit 0

if ! tmux list-panes -a -F "#{pane_id}" 2>/dev/null | grep -qx "$pid"; then
  pid=$(tmux list-panes -t "$wid" -F "#{pane_id}" 2>/dev/null | awk 'NF {print $1; exit}')
fi
[[ -n "${pid:-}" ]] || exit 0

current=$(tmux display-message -p "#{session_id}:::#{window_id}:::#{pane_id}" 2>/dev/null || true)
if [[ -n "$current" ]]; then
  printf '%s\n' "$current" > "$RUN_DIR/jump_back.txt"
fi

SOCK="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}/agent-tracker.sock"
if [[ -S "$SOCK" ]]; then
  printf '{"kind":"command","command":"acknowledge","session_id":"%s","window_id":"%s","pane":"%s"}\n' \
    "$sid" "$wid" "$pid" | nc -U "$SOCK" >/dev/null 2>&1 || true
fi

tmux switch-client -t "$sid" \; select-window -t "$wid" \; select-pane -t "$pid"
