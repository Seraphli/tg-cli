#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Exit + SessionEnd verification ---"

ensure_infrastructure

start_claude "e2e-cc-20"

LOG_BEFORE_EXIT=$(wc -l < "$LOG_FILE")
pane_log "[session_end] BEFORE /exit"
$TMUX_TEST send-keys -t "$E2E_SESSION" "/exit"
sleep 1
$TMUX_TEST send-keys -t "$E2E_SESSION" Enter
pane_log "[session_end] AFTER /exit"

ELAPSED=0
SESSION_END_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_EXIT" ]; then
    if tail -n +"$((LOG_BEFORE_EXIT + 1))" "$LOG_FILE" | grep "SessionEnd" > /dev/null 2>&1; then
      SESSION_END_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for SessionEnd... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[session_end] AFTER SessionEnd detected"

if [ "$SESSION_END_FOUND" = true ]; then
  pass "SessionEnd notification received after /exit"
else
  fail "SessionEnd notification not received within ${TIMEOUT}s"
fi

pane_log "[session_end] AFTER /exit PASS"

# Wait for CC to fully exit and shell to be ready
_CC_PHASE_SESSION=""
sleep 5
pane_log "[session_end] AFTER wait for shell"
$TMUX_TEST send-keys -t "$E2E_SESSION" "exit"
sleep 1
pane_log "[session_end] AFTER send exit text"
$TMUX_TEST send-keys -t "$E2E_SESSION" Enter
sleep 2
pane_log "[session_end] AFTER send Enter"
if $TMUX_TEST has-session -t "=$E2E_SESSION" 2>/dev/null; then
  pane_log "[session_end] session still exists"
  $TMUX_TEST kill-session -t "=$E2E_SESSION" 2>/dev/null || true
  E2E_PANE=""
  fail "tmux session still exists after /exit + shell exit"
fi
E2E_PANE=""
