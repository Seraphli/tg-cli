#!/bin/bash
# Codex-specific test common: start_codex function
CODEX_COMMON_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${CODEX_COMMON_DIR}/../e2e_common.sh"

start_codex() {
  # Clean stale session state so bot doesn't confuse Codex pane with prior CC session
  rm -f "$TEST_CONFIG_DIR/sessions.json" 2>/dev/null || true
  $TMUX_TEST kill-session -t "=$E2E_SESSION" 2>/dev/null || true
  $TMUX_TEST new-session -d -s "$E2E_SESSION"
  E2E_PANE=$($TMUX_TEST list-panes -t "$E2E_SESSION" -F '#{pane_id}@#{socket_path}')
  export E2E_PANE
  $TMUX_TEST send-keys -t "$E2E_SESSION" \
    "CODEX_HOME=$CODEX_HOME codex --yolo --enable codex_hooks"
  sleep 1
  $TMUX_TEST send-keys -t "$E2E_SESSION" Enter
  echo "Waiting for Codex to start..."
  local elapsed=0
  local dialog_handled=false
  while [ $elapsed -lt 60 ]; do
    PANE_CONTENT=$($TMUX_TEST capture-pane -t "${E2E_PANE%@*}" -p -S - 2>/dev/null || true)
    echo "  DEBUG: PANE_CONTENT (${#PANE_CONTENT} chars): $PANE_CONTENT"
    set +eo pipefail
    echo "$PANE_CONTENT" | grep -qi "trust\|continue\|Press enter"
    _ps=("${PIPESTATUS[@]}")
    set -eo pipefail
    echo "  DEBUG: grep 'trust|continue|Press enter' PIPESTATUS=${_ps[*]}"
    if [ "$dialog_handled" = false ] && [ "${_ps[1]}" -eq 0 ]; then
      $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
      dialog_handled=true
      echo "Codex dialog detected and confirmed at t=$elapsed"
      sleep 2
      elapsed=$((elapsed + 2))
      continue
    fi
    local title
    title=$($TMUX_TEST display-message -t "${E2E_PANE%@*}" -p '#{pane_title}' 2>/dev/null || true)
    local expected_title
    expected_title=$(basename "$(pwd)")
    if [ "$title" = "$expected_title" ]; then
      echo "Codex idle detected (title: $title)"
      sleep 2
      break
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  if [ $elapsed -ge 60 ]; then
    echo "WARN: Codex idle detection timed out after 60s"
  fi
  pane_log "[start_codex] after idle detection"
  echo "Codex warmup: injecting 'say hello' to register session..."
  local log_before_warmup
  log_before_warmup=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
  inject_prompt "say hello"
  elapsed=0
  while [ $elapsed -lt 90 ]; do
    if tail -n +"$((log_before_warmup + 1))" "$LOG_FILE" | grep "Session tracked" > /dev/null 2>&1; then
      echo "Codex warmup: session registered"
      break
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  if [ $elapsed -ge 90 ]; then
    echo "WARN: Codex warmup session registration not detected within 90s"
  fi
  elapsed=0
  while [ $elapsed -lt 180 ]; do
    if tail -n +"$((log_before_warmup + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
      echo "Codex warmup: Stop received, warmup complete"
      break
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  if [ $elapsed -ge 180 ]; then
    echo "WARN: Codex warmup Stop not received within 180s (continuing anyway)"
  fi
  sleep 3
  pane_log "[start_codex] after warmup"
}
