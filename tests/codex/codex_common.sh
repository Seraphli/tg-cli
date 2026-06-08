#!/bin/bash
# Codex-specific test common: start_codex function
CODEX_COMMON_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${CODEX_COMMON_DIR}/../e2e_common.sh"

# codex_api_idle: query /session/idle for the E2E codex pane via the bot HTTP API.
# Works even before the session is registered — the bot reads the pane title
# directly (product IsSessionRunning, single source of truth). Echoes True/False.
codex_api_idle() {
  local enc
  enc=$(printf '%s' "$E2E_PANE" | python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read()))")
  curl -sf "http://127.0.0.1:$TEST_PORT/session/idle?target=$enc" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('idle',False))" 2>/dev/null || echo "False"
}

start_codex() {
  local session_name="${1:-$E2E_SESSION}"
  local install_trap="${2:-true}"
  E2E_SESSION="$session_name"
  export E2E_SESSION
  # Clean stale session state so bot doesn't confuse Codex pane with prior CC session
  rm -f "$TEST_CONFIG_DIR/sessions.json" 2>/dev/null || true
  $TMUX_TEST kill-session -t "=$E2E_SESSION" 2>/dev/null || true
  $TMUX_TEST new-session -d -s "$E2E_SESSION"
  E2E_PANE=$($TMUX_TEST list-panes -t "$E2E_SESSION" -F '#{pane_id}@#{socket_path}')
  export E2E_PANE
  $TMUX_TEST send-keys -t "$E2E_SESSION" \
    "CODEX_HOME=$CODEX_HOME codex --yolo --enable hooks"
  sleep 1
  $TMUX_TEST send-keys -t "$E2E_SESSION" Enter
  echo "Waiting for Codex to start..."
  # Initial settle, aligned with CC start_claude (cc_common.sh:21): give codex time to
  # render its first dialog before ANY detection. Without this, the loop's first
  # iteration runs at t=0 when codex has drawn nothing yet — the dialog greps miss, the
  # pane title is still the default 'seraphli-ws' (non-spinner), so /session/idle falsely
  # reports idle and warmup injects into the not-yet-shown trust dialog (HTTP 500).
  sleep 5
  local elapsed=0
  local trust_handled=false
  local hooks_handled=false
  while [ $elapsed -lt 60 ]; do
    PANE_CONTENT=$($TMUX_TEST capture-pane -t "${E2E_PANE%@*}" -p -S - 2>/dev/null || true)
    set +eo pipefail
    echo "$PANE_CONTENT" | grep -qi "Hooks need review"
    _ps_hooks=("${PIPESTATUS[@]}")
    echo "$PANE_CONTENT" | grep -qi "Do you trust"
    _ps_trust=("${PIPESTATUS[@]}")
    set -eo pipefail
    local title api_idle stage
    title=$($TMUX_TEST display-message -t "${E2E_PANE%@*}" -p '#{pane_title}' 2>/dev/null || true)
    api_idle=$(codex_api_idle)
    # Determine which startup stage this snapshot is in (the loop handles 2 dialogs, then waits for idle).
    if   [ "${_ps_trust[1]}" -eq 0 ]; then stage="trust-dialog (about to confirm 'Do you trust')"
    elif [ "${_ps_hooks[1]}" -eq 0 ]; then stage="hooks-dialog (about to select 'Trust all and continue')"
    elif [ "$api_idle" = "True" ]; then stage="idle (codex ready -> will inject warmup)"
    else stage="wait-idle (booted, /session/idle=False, codex still busy)"
    fi
    echo "  === PANE: codex startup t=$elapsed stage=[$stage] title='$title'(${#title}c) api_idle=$api_idle ==="
    echo "$PANE_CONTENT"
    echo "  === END PANE ==="
    # "Hooks need review" dialog: cursor defaults to "1. Review hooks"; move Down to
    # "2. Trust all and continue" then confirm. Checked before the trust dialog because the
    # hooks dialog text also contains the word "continue".
    if [ "$hooks_handled" = false ] && [ "${_ps_hooks[1]}" -eq 0 ]; then
      $TMUX_TEST send-keys -t "$E2E_SESSION" Down
      sleep 1
      $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
      hooks_handled=true
      echo "Codex hooks-review dialog detected, selected 'Trust all and continue' at t=$elapsed"
      sleep 2
      elapsed=$((elapsed + 2))
      continue
    fi
    # "Do you trust the contents of this directory?" dialog: default option is "Yes, continue"; confirm.
    if [ "$trust_handled" = false ] && [ "${_ps_trust[1]}" -eq 0 ]; then
      $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
      trust_handled=true
      echo "Codex trust dialog detected and confirmed at t=$elapsed"
      sleep 2
      elapsed=$((elapsed + 2))
      continue
    fi
    if [ "$api_idle" = "True" ]; then
      echo "Codex idle detected via /session/idle (title: $title)"
      sleep 5
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
  _CODEX_PHASE_SESSION="$E2E_SESSION"
  if [ "$install_trap" = "true" ]; then
    trap '_codex_phase_cleanup' EXIT
  fi
}

stop_codex() {
  local session_name="${1:-$E2E_SESSION}"
  # Codex has no SessionEnd hook, so we can't wait on a log marker like stop_claude does.
  # Codex supports the /exit slash command — quit gracefully, then kill-session as a backstop.
  if $TMUX_TEST has-session -t "=$session_name" 2>/dev/null; then
    $TMUX_TEST send-keys -t "$session_name" "/exit"
    sleep 1
    $TMUX_TEST send-keys -t "$session_name" Enter
    sleep 3
    $TMUX_TEST kill-session -t "=$session_name" 2>/dev/null || true
  fi
  E2E_PANE=""
}

_codex_phase_cleanup() {
  local rc=$?
  if [ -z "${_CODEX_PHASE_SESSION:-}" ]; then
    if [ "${E2E_ORCHESTRATED:-}" != "1" ]; then
      cleanup_sessions 2>/dev/null || true
    fi
    return $rc
  fi
  if [ $rc -eq 0 ]; then
    stop_codex "$_CODEX_PHASE_SESSION"
  else
    echo "  [cleanup] codex phase exited with rc=$rc, capturing pane and killing session $_CODEX_PHASE_SESSION"
    pane_log "[cleanup] abnormal exit rc=$rc - $_CODEX_PHASE_SESSION"
    $TMUX_TEST kill-session -t "=$_CODEX_PHASE_SESSION" 2>/dev/null || true
    E2E_PANE=""
  fi
  _CODEX_PHASE_SESSION=""
  if [ "${E2E_ORCHESTRATED:-}" != "1" ]; then
    cleanup_sessions 2>/dev/null || true
  fi
  exit $rc
}
