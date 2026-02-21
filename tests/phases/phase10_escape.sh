#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Phase 10: Escape command test ---"

ensure_infrastructure

TARGET="$CLAUDE_PANE"
ENCODED_TARGET=$(printf '%s' "$TARGET" | jq -sRr @uri)

LOG_BEFORE_ESC=$(wc -l < "$LOG_FILE")

pane_log "[Phase 10] BEFORE sending AskUserQuestion prompt for escape test"

# 1. Inject prompt to trigger AskUserQuestion
inject_prompt "Ask me a question using AskUserQuestion tool with header 'Escape Test' and two options: 'Yes' with description 'Confirm', 'No' with description 'Deny'. Question: 'Continue?'"

pane_log "[Phase 10] AFTER sending prompt"

# 2. Wait for AskUserQuestion notification in bot log
ELAPSED=0
AQ_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_ESC" ]; then
    NEW_LOGS=$(tail -n +"$((LOG_BEFORE_ESC + 1))" "$LOG_FILE")
    if echo "$NEW_LOGS" | grep "AskUserQuestion sent" > /dev/null 2>&1; then
      AQ_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

if [ "$AQ_FOUND" != true ]; then
  fail "AskUserQuestion not triggered for escape test within ${TIMEOUT}s"
  exit 0
fi

pass "AskUserQuestion triggered for escape test"

wait_for_cc_idle

# 3. Capture pane BEFORE escape — verify AskUserQuestion UI is visible
BEFORE_ESCAPE=$(curl -s "http://127.0.0.1:$TEST_PORT/capture?target=$ENCODED_TARGET" | jq -r '.content // empty')

pane_log "[Phase 10] Pane BEFORE escape captured"

if echo "$BEFORE_ESCAPE" | grep "Esc to cancel" > /dev/null 2>&1; then
  pass "Pane contains AskUserQuestion content before escape"
else
  fail "Pane does not contain AskUserQuestion content before escape"
fi

# 4. Send Escape via /escape API
RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/escape?target=$ENCODED_TARGET")
STATUS=$(echo "$RESP" | jq -r '.status // empty' 2>/dev/null)

if [ "$STATUS" = "ok" ]; then
  pass "/escape API returned ok"
else
  fail "/escape API failed: $RESP"
fi

# 5. Wait for TUI to update, then capture pane AFTER escape
wait_for_cc_idle
wait_for_pane_content "User declined"
pane_log "[Phase 10] AFTER escape (idle)"

AFTER_ESCAPE=$(curl -s "http://127.0.0.1:$TEST_PORT/capture?target=$ENCODED_TARGET" | jq -r '.content // empty')

pane_log "[Phase 10] Pane AFTER escape captured"

# Verify AskUserQuestion UI is gone
if echo "$AFTER_ESCAPE" | grep "Esc to cancel" > /dev/null 2>&1; then
  fail "AskUserQuestion dialog still active after escape"
else
  pass "AskUserQuestion dialog dismissed after escape"
fi

if echo "$AFTER_ESCAPE" | grep "User declined" > /dev/null 2>&1; then
  pass "CC shows 'User declined to answer questions' after escape"
else
  fail "CC did not show decline message after escape"
fi
