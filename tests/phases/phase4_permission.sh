#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Phase 4: PermissionRequest test ---"

ensure_infrastructure

# Record log position
LOG_BEFORE_PERM=$(wc -l < "$LOG_FILE")

# Send command that triggers Bash permission, with explicit instruction to output text first
pane_log "[Phase 4] BEFORE permission prompt"
inject_prompt "First write a brief paragraph explaining what you are about to do, then run this exact bash command: echo perm_test_ok > /tmp/tg-cli-perm-test.txt. Run only this one command and nothing else, do not verify or cat the file."
pane_log "[Phase 4] AFTER sending permission prompt"

# Wait for permission request in bot log
ELAPSED=0
PERM_FOUND=false
PERM_MSG_ID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PERM" ]; then
    if tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep "Permission request sent" > /dev/null 2>&1; then
      PERM_FOUND=true
      PERM_MSG_ID=$(tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep -oPm1 'msg_id=\K[0-9]+' || true)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

wait_for_cc_idle
pane_log "[Phase 4] AFTER permission detected (idle)"

if [ "$PERM_FOUND" = true ] && [ -n "$PERM_MSG_ID" ]; then
  pass "PermissionRequest TG notification sent (msg_id=$PERM_MSG_ID)"

  # Verify Update notification sent BEFORE PermissionRequest
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE")
  UPDATE_LINE=$(awk '/Notification sent.*PreToolUse/{print NR; exit}' <<< "$NEW_LOGS")
  PERM_LINE=$(awk '/Permission request sent/{print NR; exit}' <<< "$NEW_LOGS")
  if [ -n "$UPDATE_LINE" ] && [ -n "$PERM_LINE" ]; then
    if [ "$UPDATE_LINE" -lt "$PERM_LINE" ]; then
      pass "Update sent BEFORE PermissionRequest (line $UPDATE_LINE < $PERM_LINE)"
    else
      fail "Update sent AFTER PermissionRequest (line $UPDATE_LINE >= $PERM_LINE)"
    fi
  else
    [ -z "$UPDATE_LINE" ] && fail "PreToolUse intermediate notification not found before PermissionRequest"
    [ -z "$PERM_LINE" ] && fail "Permission request log line not found"
  fi

  # Approve via API endpoint
  pane_log "[Phase 4] BEFORE approve API call"
  API_URL="http://127.0.0.1:$TEST_PORT/permission/decide?msg_id=$PERM_MSG_ID&decision=allow"
  echo "  API call: GET $API_URL"
  DECIDE_RESP=$(curl -s "$API_URL")
  DECIDE_BEHAVIOR=$(echo "$DECIDE_RESP" | jq -r '.behavior // empty' 2>/dev/null)
  if [ "$DECIDE_BEHAVIOR" = "allow" ]; then
    pass "Permission approved via /permission/decide API (behavior=allow)"
  else
    fail "Permission decide API returned unexpected: $DECIDE_RESP"
  fi
  wait_for_cc_idle
  pane_log "[Phase 4] AFTER approve API call (idle)"

  # Wait for Stop notification (Claude completes after permission approved)
  wait_for_cc_idle
  LOG_AFTER_PERM=$(wc -l < "$LOG_FILE")
  if tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep "Permission resolved" > /dev/null 2>&1; then
    pass "Permission resolved and logged"
  else
    fail "Permission resolution not found in log"
  fi

  # Verify permission debug logging (full payload)
  if tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep "Permission payload: toolInput=" > /dev/null 2>&1; then
    pass "Permission debug log includes full payload (toolInput + suggestions)"
  else
    fail "Permission debug log not found (expected 'Permission payload: toolInput=')"
  fi
  # Wait for CC to complete the full turn (Bash execution + Stop hook)
  ELAPSED=0
  STOP4_FOUND=false
  while [ $ELAPSED -lt $TIMEOUT ]; do
    if tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
      STOP4_FOUND=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done
  if [ "$STOP4_FOUND" = true ]; then
    pass "Phase 4 Stop notification received (CC turn complete)"
  else
    fail "Phase 4 Stop notification not received within ${TIMEOUT}s"
  fi
else
  fail "PermissionRequest not triggered within ${TIMEOUT}s"
fi
