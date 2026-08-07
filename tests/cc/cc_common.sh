#!/bin/bash
# CC-specific test common: start_claude function
CC_COMMON_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${CC_COMMON_DIR}/../e2e_common.sh"

start_claude() {
  local session_name="${1:-$E2E_SESSION}"
  local perm_flag="${2:---dangerously-skip-permissions}"
  local install_trap="${3:-true}"
  E2E_SESSION="$session_name"
  export E2E_SESSION
  $TMUX_TEST kill-session -t "=$E2E_SESSION" 2>/dev/null || true
  $TMUX_TEST new-session -d -s "$E2E_SESSION" -x 220 -y 50 -c "$CC_WORKDIR"
  E2E_PANE=$($TMUX_TEST list-panes -t "$E2E_SESSION" -F '#{pane_id}@#{socket_path}')
  export E2E_PANE
  # Launch CC via the shared canonical builder cc_launch_cmd (defined in e2e_common.sh): it forwards the same
  # nine provider/model env vars (printf %q quoted so tokens/URLs with special chars cannot break or inject
  # into the command) and pins the --tools allowlist that structurally removes the Agent tool. perm_flag
  # selects this session's permission mode.
  $TMUX_TEST send-keys -t "$E2E_SESSION" \
    "$(cc_launch_cmd "$perm_flag")"
  sleep 1
  $TMUX_TEST send-keys -t "$E2E_SESSION" Enter
  echo "Waiting for Claude to start..."
  sleep 5
  pane_log "[start_claude] after 5s sleep, before trust check"
  PANE_CONTENT=$($TMUX_TEST capture-pane -t "${E2E_PANE%@*}" -p -S - 2>/dev/null || true)
  echo "  DEBUG: PANE_CONTENT (${#PANE_CONTENT} chars): $PANE_CONTENT"
  set +eo pipefail
  echo "$PANE_CONTENT" | grep -q "Bypass Permissions"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    $TMUX_TEST send-keys -t "$E2E_SESSION" Down
    sleep 1
    $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
    echo "Bypass Permissions dialog detected, accepted."
  else
    set +eo pipefail
    echo "$PANE_CONTENT" | grep -q "trust"
    _ps=("${PIPESTATUS[@]}")
    set -eo pipefail
    if [ "${_ps[1]}" -eq 0 ]; then
      $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
      echo "Trust dialog detected, confirmed."
    else
      echo "No dialog detected, skipping."
    fi
  fi
  pane_log "[start_claude] after trust dialog handling"
  echo "Waiting for Claude to reach idle state..."
  pane_log "[start_claude] before wait_for_idle"
  wait_for_idle || true
  pane_log "[start_claude] after wait_for_idle"
  _CC_PHASE_SESSION="$E2E_SESSION"
  if [ "$install_trap" = "true" ]; then
    trap '_cc_phase_cleanup' EXIT
  fi
}

stop_claude() {
  local session_name="${1:-$E2E_SESSION}"
  local target_pane
  target_pane=$($TMUX_TEST list-panes -t "$session_name" \
    -F '#{pane_id}@#{socket_path}' 2>/dev/null | head -1 || echo "")
  if [ -z "$target_pane" ]; then
    echo "ERROR: stop_claude: session $session_name not found (unexpected)"
    E2E_PANE=""
    return 1
  fi
  E2E_PANE="$target_pane"
  local log_before_exit
  log_before_exit=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
  $TMUX_TEST send-keys -t "$session_name" "/exit"
  sleep 1
  $TMUX_TEST send-keys -t "$session_name" Enter
  local elapsed=0
  local exited=false
  while [ $elapsed -lt 30 ]; do
    sleep 2
    elapsed=$((elapsed + 2))
    if tail -n +"$((log_before_exit + 1))" "$LOG_FILE" | grep -q "SessionEnd" 2>/dev/null; then
      exited=true
      break
    fi
  done
  if [ "$exited" = true ]; then
    sleep 5
    $TMUX_TEST send-keys -t "$session_name" "exit"
    sleep 1
    $TMUX_TEST send-keys -t "$session_name" Enter
    sleep 5
    if $TMUX_TEST has-session -t "=$session_name" 2>/dev/null; then
      echo "ERROR: stop_claude: tmux session $session_name still exists after shell exit"
      pane_log "[stop_claude] session residual - $session_name"
      $TMUX_TEST kill-session -t "=$session_name" 2>/dev/null || true
      E2E_PANE=""
      return 1
    fi
  else
    echo "ERROR: stop_claude: CC did not exit cleanly within 30s in $session_name"
    pane_log "[stop_claude] abnormal exit - $session_name"
    $TMUX_TEST kill-session -t "=$session_name" 2>/dev/null || true
    E2E_PANE=""
    return 1
  fi
  E2E_PANE=""
}

_cc_phase_cleanup() {
  local rc=$?
  if [ -z "${_CC_PHASE_SESSION:-}" ]; then
    if [ "${E2E_ORCHESTRATED:-}" != "1" ]; then
      cleanup_sessions 2>/dev/null || true
    fi
    return $rc
  fi
  if [ $rc -eq 0 ]; then
    # Run V1/V2/V3 log validations while the CC pane ($E2E_PANE) is still alive, before stop_claude
    # kills it, so a FAIL capture shows what CC was doing. This is the shared "end of phase, before
    # cleanup" point for all start_claude CC phases; it touches a flag so run_phase skips its fallback.
    validate_phase_inline "${E2E_PANE:-}"
    if ! stop_claude "$_CC_PHASE_SESSION"; then
      echo "ERROR: _cc_phase_cleanup: graceful stop failed for $_CC_PHASE_SESSION"
      rc=1
    fi
  else
    echo "  [cleanup] phase exited with rc=$rc, capturing pane and killing session $_CC_PHASE_SESSION"
    pane_log "[cleanup] abnormal exit rc=$rc - $_CC_PHASE_SESSION"
    $TMUX_TEST kill-session -t "=$_CC_PHASE_SESSION" 2>/dev/null || true
    E2E_PANE=""
  fi
  _CC_PHASE_SESSION=""
  if [ "${E2E_ORCHESTRATED:-}" != "1" ]; then
    cleanup_sessions 2>/dev/null || true
  fi
  exit $rc
}
