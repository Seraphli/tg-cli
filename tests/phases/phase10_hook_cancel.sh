#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Hook cancel (TUI answer) test ---"

ensure_infrastructure

# Record log position
LOG_BEFORE_CANCEL=$(wc -l < "$LOG_FILE")

# Send command that triggers a PermissionRequest (same pattern as permission)
pane_log "[hook_cancel] BEFORE permission prompt"
inject_prompt "First write a brief paragraph explaining what you are about to do, then run this exact bash command: echo hook_cancel_test_ok > /tmp/tg-cli-hook-cancel-test.txt. Run only this one command and nothing else, do not verify or cat the file."
pane_log "[hook_cancel] AFTER sending permission prompt"

# Wait for PermissionRequest notification in bot log (pending file created, hook blocking)
ELAPSED=0
PERM_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_CANCEL" ]; then
    if tail -n +"$((LOG_BEFORE_CANCEL + 1))" "$LOG_FILE" | grep "Permission request sent" > /dev/null 2>&1; then
      PERM_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for PermissionRequest... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[hook_cancel] AFTER permission detected"

if [ "$PERM_FOUND" = false ]; then
  fail "PermissionRequest not triggered within ${TIMEOUT}s"
  exit 1
fi
pass "PermissionRequest triggered (hook blocking, pending file created)"

# Instead of approving via API, approve via TUI: press Enter in Claude pane
# This simulates user answering in TUI while hook is still blocking
pane_log "[hook_cancel] BEFORE TUI Enter (approve in TUI)"
$TMUX_TEST send-keys -t "$CLAUDE_SESSION" Enter
pane_log "[hook_cancel] AFTER TUI Enter"

# Wait for CC to continue and reach idle state (Stop hook fired)
wait_for_cc_idle
pane_log "[hook_cancel] AFTER CC reached idle"

# Check bot log for "Cancelled pending file" — proves cancelPendingFilesBySession ran
ELAPSED=0
CANCEL_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_CANCEL + 1))" "$LOG_FILE" | grep -E "Cancelled pending file|Removed orphan pending file" > /dev/null 2>&1; then
    CANCEL_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for pending file cancel/cleanup log... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$CANCEL_FOUND" = true ]; then
  pass "Pending file cancel/cleanup log found (cancelPendingFilesBySession ran)"
else
  fail "Pending file cancel/cleanup log not found within ${TIMEOUT}s"
fi

# Check that pending files were cleaned up (removed by hook.go after status=cancelled)
TEST_PENDING_DIR="/tmp/.tg-cli-test/pending"
PENDING_COUNT=$(find "$TEST_PENDING_DIR" -maxdepth 1 -name "*.json" 2>/dev/null | wc -l)
if [ "$PENDING_COUNT" -eq 0 ]; then
  pass "Pending files cleaned up from $TEST_PENDING_DIR"
else
  fail "Pending files still exist after cancel: $PENDING_COUNT file(s) in $TEST_PENDING_DIR"
fi

# Wait for Stop notification to confirm CC completed the turn
ELAPSED=0
STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_CANCEL + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
    STOP_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for Stop notification... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$STOP_FOUND" = true ]; then
  pass "Stop notification received (CC turn complete after TUI answer)"
else
  fail "Stop notification not received within ${TIMEOUT}s"
fi
