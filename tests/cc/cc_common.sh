#!/bin/bash
# CC-specific test common: start_claude function
CC_COMMON_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${CC_COMMON_DIR}/../e2e_common.sh"

start_claude() {
  $TMUX_TEST kill-session -t "=$E2E_SESSION" 2>/dev/null || true
  $TMUX_TEST new-session -d -s "$E2E_SESSION"
  E2E_PANE=$($TMUX_TEST list-panes -t "$E2E_SESSION" -F '#{pane_id}')
  export E2E_PANE
  $TMUX_TEST send-keys -t "$E2E_SESSION" \
    "BROWSER=none CLAUDE_CONFIG_DIR=$TEST_CLAUDE_CONFIG_DIR claude --model sonnet --allow-dangerously-skip-permissions"
  sleep 1
  $TMUX_TEST send-keys -t "$E2E_SESSION" Enter
  echo "Waiting for Claude to start..."
  sleep 5
  pane_log "[start_claude] after 5s sleep, before trust check"
  PANE_CONTENT=$($TMUX_TEST capture-pane -t "$E2E_PANE" -p -S - 2>/dev/null || true)
  if echo "$PANE_CONTENT" | grep -qi "Bypass Permissions"; then
    $TMUX_TEST send-keys -t "$E2E_SESSION" Down
    sleep 1
    $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
    echo "Bypass Permissions dialog detected, accepted."
  elif echo "$PANE_CONTENT" | grep -qi "trust"; then
    $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
    echo "Trust dialog detected, confirmed."
  else
    echo "No dialog detected, skipping."
  fi
  pane_log "[start_claude] after trust dialog handling"
  echo "Waiting for Claude to reach idle state..."
  pane_log "[start_claude] before wait_for_idle"
  wait_for_idle || true
  pane_log "[start_claude] after wait_for_idle"
}
