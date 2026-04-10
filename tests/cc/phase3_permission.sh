#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- PermissionRequest test ---"

ensure_infrastructure

# Record log position
LOG_BEFORE_PERM=$(wc -l < "$LOG_FILE")
TYPING_LOG_BEFORE=$(wc -l < "$TYPING_LOG_FILE" 2>/dev/null || echo 0)

# Send command that triggers Bash permission, with explicit instruction to output text first
pane_log "[permission] BEFORE permission prompt"
inject_prompt "Answer this question first in 2 sentences: what does the bash redirect operator '>' do when used with echo? After answering, run this exact bash command: echo perm_test_ok > /tmp/tg-cli-perm-test.txt. Run only this one command and nothing else, do not verify or cat the file."
pane_log "[permission] AFTER sending permission prompt"

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

wait_for_idle
pane_log "[permission] AFTER permission detected (idle)"

if [ "$PERM_FOUND" = true ] && [ -n "$PERM_MSG_ID" ]; then
  pass "PermissionRequest TG notification sent (msg_id=$PERM_MSG_ID)"

  # Typing continuity: inject → PreToolUse (text generation before tool call)
  check_typing_continuity "$TYPING_LOG_BEFORE" "PreToolUse" "phase3"

  # Verify Update notification sent BEFORE PermissionRequest (tolerant to Claude skipping intermediate text)
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE")
  UPDATE_LINE=$(awk '/Notification sent.*PreToolUse/{print NR; exit}' <<< "$NEW_LOGS")
  PERM_LINE=$(awk '/Permission request sent/{print NR; exit}' <<< "$NEW_LOGS")
  if [ -z "$PERM_LINE" ]; then
    fail "Permission request log line not found"
  elif [ -n "$UPDATE_LINE" ]; then
    if [ "$UPDATE_LINE" -lt "$PERM_LINE" ]; then
      pass "Update sent BEFORE PermissionRequest (line $UPDATE_LINE < $PERM_LINE)"
    else
      fail "Update sent AFTER PermissionRequest (line $UPDATE_LINE >= $PERM_LINE)"
    fi
  elif [[ "$NEW_LOGS" == *"PreToolUse Update skipped"* ]]; then
    pass "PreToolUse Update path exercised (skipped: no new assistant text — vacuous pass)"
  else
    fail "Neither PreToolUse Update sent nor skipped log found — code path not exercised"
  fi

  # Approve via API endpoint
  pane_log "[permission] BEFORE approve API call"
  API_URL="http://127.0.0.1:$TEST_PORT/permission/decide?msg_id=$PERM_MSG_ID&decision=allow"
  echo "  API call: GET $API_URL"
  DECIDE_RESP=$(curl -s "$API_URL")
  DECIDE_BEHAVIOR=$(echo "$DECIDE_RESP" | jq -r '.behavior // empty' 2>/dev/null)
  if [ "$DECIDE_BEHAVIOR" = "allow" ]; then
    pass "Permission approved via /permission/decide API (behavior=allow)"
  else
    fail "Permission decide API returned unexpected: $DECIDE_RESP"
  fi
  wait_for_idle
  pane_log "[permission] AFTER approve API call (idle)"

  # Wait for Stop notification (Claude completes after permission approved)
  wait_for_idle
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
    pass "Stop notification received (CC turn complete)"
  else
    fail "Stop notification not received within ${TIMEOUT}s"
  fi
else
  fail "PermissionRequest not triggered within ${TIMEOUT}s"
fi

# --- Permission Cancel button test ---
echo ""
echo "--- Permission Cancel button test ---"

wait_for_idle
LOG_BEFORE_PCANCEL=$(wc -l < "$LOG_FILE")

pane_log "[perm_cancel] BEFORE permission cancel prompt"
inject_prompt "First write a brief paragraph explaining what you are about to do, then run this exact bash command: echo permission_cancel_test > /tmp/tg-cli-perm-cancel-test.txt. Run only this one command and nothing else, do not verify or cat the file."
pane_log "[perm_cancel] AFTER sending permission cancel prompt"

# Wait for PermissionRequest notification
ELAPSED=0
PCANCEL_FOUND=false
PCANCEL_UUID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PCANCEL" ]; then
    if tail -n +"$((LOG_BEFORE_PCANCEL + 1))" "$LOG_FILE" | grep "Permission request sent" > /dev/null 2>&1; then
      PCANCEL_FOUND=true
      PCANCEL_UUID=$(tail -n +"$((LOG_BEFORE_PCANCEL + 1))" "$LOG_FILE" | grep -oPm1 'Permission request sent.*uuid=\K[^ ]+' || true)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

pane_log "[perm_cancel] AFTER permission detected"

if [ "$PCANCEL_FOUND" = true ] && [ -n "$PCANCEL_UUID" ]; then
  pass "Permission cancel test: PermissionRequest triggered (uuid=$PCANCEL_UUID)"

  # Cancel via /pending/cancel API
  pane_log "[perm_cancel] BEFORE cancel API call"
  CANCEL_URL="http://127.0.0.1:$TEST_PORT/pending/cancel?uuid=$PCANCEL_UUID"
  echo "  API call: POST $CANCEL_URL"
  CANCEL_RESP=$(curl -s -X POST "$CANCEL_URL")
  pane_log "[perm_cancel] AFTER cancel API call"

  # Wait for cancel confirmation in log
  ELAPSED=0
  PCANCEL_LOGGED=false
  while [ $ELAPSED -lt $TIMEOUT ]; do
    if tail -n +"$((LOG_BEFORE_PCANCEL + 1))" "$LOG_FILE" | grep "Permission cancelled: msg_id=" > /dev/null 2>&1; then
      PCANCEL_LOGGED=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  if [ "$PCANCEL_LOGGED" = true ]; then
    pass "Permission cancelled via /pending/cancel API"
  else
    fail "Permission cancel log not found within ${TIMEOUT}s"
  fi

  wait_for_idle
  pane_log "[perm_cancel] AFTER CC idle"
else
  fail "Permission cancel test: PermissionRequest not triggered within ${TIMEOUT}s"
fi

# --- Permission text reply cancel test ---
echo ""
echo "--- Permission text reply cancel test ---"

wait_for_idle
LOG_BEFORE_PTXTCANCEL=$(wc -l < "$LOG_FILE")

pane_log "[perm_txt_cancel] BEFORE permission text cancel prompt"
inject_prompt "First write a brief paragraph explaining what you are about to do, then run this exact bash command: echo permission_text_cancel_test > /tmp/tg-cli-perm-txtcancel-test.txt. Run only this one command and nothing else, do not verify or cat the file."
pane_log "[perm_txt_cancel] AFTER sending permission text cancel prompt"

# Wait for PermissionRequest notification
ELAPSED=0
PTXT_FOUND=false
PTXT_TARGET=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PTXTCANCEL" ]; then
    if tail -n +"$((LOG_BEFORE_PTXTCANCEL + 1))" "$LOG_FILE" | grep "Permission request sent" > /dev/null 2>&1; then
      PTXT_FOUND=true
      PTXT_TARGET=$(tail -n +"$((LOG_BEFORE_PTXTCANCEL + 1))" "$LOG_FILE" | grep -oPm1 'Permission request sent.*tmux=\K[^ ]+' || true)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

pane_log "[perm_txt_cancel] AFTER permission detected"

if [ "$PTXT_FOUND" = true ] && [ -n "$PTXT_TARGET" ]; then
  pass "Permission text cancel test: PermissionRequest triggered (tmux=$PTXT_TARGET)"

  # Send text reply via /group/text API (this cancels permission and injects text)
  pane_log "[perm_txt_cancel] BEFORE group text API call"
  ENCODED_TARGET=$(printf '%s' "$PTXT_TARGET" | jq -sRr @uri)
  GTXT_URL="http://127.0.0.1:$TEST_PORT/group/text?target=$ENCODED_TARGET&text=test_cancel_reply"
  echo "  API call: GET $GTXT_URL"
  GTXT_RESP=$(curl -s "$GTXT_URL")
  pane_log "[perm_txt_cancel] AFTER group text API call"

  # Wait for log showing ESC sent + text injected
  ELAPSED=0
  PTXT_LOGGED=false
  while [ $ELAPSED -lt $TIMEOUT ]; do
    if tail -n +"$((LOG_BEFORE_PTXTCANCEL + 1))" "$LOG_FILE" | grep -E "Permission cancelled: msg_id=|Permission cancelled via group text API" > /dev/null 2>&1; then
      PTXT_LOGGED=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  if [ "$PTXT_LOGGED" = true ]; then
    pass "Permission text cancel: ESC sent and text injected via /group/text"
  else
    fail "Permission text cancel log not found within ${TIMEOUT}s"
  fi

  wait_for_idle
  pane_log "[perm_txt_cancel] AFTER CC idle"
else
  fail "Permission text cancel test: PermissionRequest not triggered within ${TIMEOUT}s"
fi
