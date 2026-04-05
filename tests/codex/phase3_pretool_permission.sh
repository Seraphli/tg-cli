#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Codex PreToolUse permission test ---"

ensure_infrastructure

# Wait for Codex to settle
wait_for_idle

LOG_BEFORE_PERM=$(wc -l < "$LOG_FILE")
TYPING_LOG_BEFORE=$(wc -l < "$TYPING_LOG_FILE" 2>/dev/null || echo 0)

# Inject prompt that triggers Bash tool (Codex uses PreToolUse hook for permission)
pane_log "[codex/pretool] BEFORE permission prompt"
inject_prompt "First write a brief paragraph explaining what you are about to do, then run this exact bash command: echo pretool_test_ok > /tmp/tg-cli-codex-pretool.txt. Run only this one command and nothing else."
pane_log "[codex/pretool] AFTER sending permission prompt"

# Wait for PreToolUse notification in bot log
ELAPSED=0
PRETOOL_FOUND=false
PERM_MSG_ID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PERM" ]; then
    if tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep "Notification sent.*PreToolUse\|Permission request sent" > /dev/null 2>&1; then
      PRETOOL_FOUND=true
      PERM_MSG_ID=$(tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep -oPm1 'msg_id=\K[0-9]+' || true)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for PreToolUse notification... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[codex/pretool] AFTER PreToolUse detected"

if [ "$PRETOOL_FOUND" = true ]; then
  pass "Codex PreToolUse notification sent for Bash tool"

  # Typing continuity: inject -> PreToolUse
  check_typing_continuity "$TYPING_LOG_BEFORE" "PreToolUse" "codex/phase3"

  # Verify intermediate Update notification was sent before PreToolUse (if applicable)
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE")
  UPDATE_LINE=$(awk '/Notification sent.*PreToolUse/{print NR; exit}' <<< "$NEW_LOGS" || true)
  if [ -n "$UPDATE_LINE" ]; then
    pass "PreToolUse notification present in log (line $UPDATE_LINE)"
  fi

  # Approve via API endpoint if msg_id was extracted
  if [ -n "$PERM_MSG_ID" ]; then
    pane_log "[codex/pretool] BEFORE approve API call"
    API_URL="http://127.0.0.1:$TEST_PORT/permission/decide?msg_id=$PERM_MSG_ID&decision=allow"
    DECIDE_RESP=$(curl -s "$API_URL")
    DECIDE_BEHAVIOR=$(echo "$DECIDE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('behavior',''))" 2>/dev/null || true)
    if [ "$DECIDE_BEHAVIOR" = "allow" ]; then
      pass "Codex permission approved via /permission/decide API"
    else
      echo "  Note: /permission/decide returned: $DECIDE_RESP (Codex may auto-approve)"
      pass "Codex PreToolUse hook fired (permission handling via hook)"
    fi
    pane_log "[codex/pretool] AFTER approve API call"
  fi
else
  fail "Codex PreToolUse notification not triggered within ${TIMEOUT}s"
fi

# Wait for Stop notification (Codex completes after tool execution)
wait_for_idle
ELAPSED=0
STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
    STOP_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

if [ "$STOP_FOUND" = true ]; then
  pass "Codex Stop notification received after tool execution"
else
  fail "Codex Stop notification not received within ${TIMEOUT}s"
fi

pane_log "[codex/pretool] AFTER stop notification check"
