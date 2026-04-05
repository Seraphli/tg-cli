#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Exit + SessionEnd verification ---"

ensure_infrastructure

LOG_BEFORE_EXIT=$(wc -l < "$LOG_FILE")
pane_log "[session_end] BEFORE /exit"
inject_prompt "/exit"
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
