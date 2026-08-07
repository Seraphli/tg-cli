#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Escape command test ---"

# AskUserQuestion is CC-only; skip for Codex
if [ "${E2E_BACKEND:-}" = "codex" ]; then
  echo "  SKIP: AskUserQuestion not supported in Codex"
  exit 0
fi

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-10"

TARGET="$E2E_PANE"
ENCODED_TARGET=$(printf '%s' "$TARGET" | jq -sRr @uri)

LOG_BEFORE_ESC=$(wc -l < "$LOG_FILE")

pane_log "[escape] BEFORE sending AskUserQuestion prompt for escape test"

# 1. Inject prompt to trigger AskUserQuestion
inject_prompt "Ask me a question using AskUserQuestion tool with header 'Escape Test' and two options: 'Yes' with description 'Confirm', 'No' with description 'Deny'. Question: 'Continue?'"

pane_log "[escape] AFTER sending prompt"

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

# Rich migration (v9): the AskUserQuestion under ESC-freeze test is sent via sendRichMessage,
# so its ESC-cancel freeze edit takes the rich (RetryFreezeEditAuto→rich) path.
if tail -n +"$((LOG_BEFORE_ESC + 1))" "$LOG_FILE" | grep -q "AskUserQuestion sent:.*fmt=rich" 2>/dev/null; then
  pass "Escape-test AskUserQuestion sent via rich message path (fmt=rich)"
else
  fail "Escape-test AskUserQuestion sent marker missing fmt=rich (expected rich message path)"
fi

# 3. Capture pane BEFORE escape — verify AskUserQuestion UI is visible
BEFORE_ESCAPE=$(curl -s "http://127.0.0.1:$TEST_PORT/capture?target=$ENCODED_TARGET" | jq -r '.content // empty')

pane_log "[escape] Pane BEFORE escape captured"

echo "  DEBUG: BEFORE_ESCAPE (${#BEFORE_ESCAPE} chars): $BEFORE_ESCAPE"
if echo "$BEFORE_ESCAPE" | grep "Esc to cancel" > /dev/null 2>&1; then
  pass "Pane contains AskUserQuestion content before escape"
else
  fail "Pane does not contain AskUserQuestion content before escape"
fi

# 4. Send Escape via /escape API
RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/escape?target=$ENCODED_TARGET")
echo "  DEBUG: RESP (${#RESP} chars): $RESP"
STATUS=$(echo "$RESP" | jq -r '.status // empty' 2>/dev/null)

if [ "$STATUS" = "ok" ]; then
  pass "/escape API returned ok"
else
  fail "/escape API failed: $RESP"
fi

# 5. Wait for TUI to update, then capture pane AFTER escape
wait_for_idle
wait_for_pane_content "User declined"
pane_log "[escape] AFTER escape (idle)"

AFTER_ESCAPE=$(curl -s "http://127.0.0.1:$TEST_PORT/capture?target=$ENCODED_TARGET" | jq -r '.content // empty')

pane_log "[escape] Pane AFTER escape captured"

# Verify AskUserQuestion UI is gone
echo "  DEBUG: AFTER_ESCAPE (${#AFTER_ESCAPE} chars): $AFTER_ESCAPE"
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

# 6. Wait for CC to become idle after Esc (Stop no longer sends a separate text notification)
echo "  Waiting for CC idle after escape..."
wait_for_idle
pass "CC idle after escape (turn complete)"

# 7. Wait for CC to be idle before sending follow-up
wait_for_idle

# 8. Record log position before follow-up message
LOG_BEFORE_FOLLOWUP=$(wc -l < "$LOG_FILE")

# 9. Send follow-up message via /group/text API
echo "  Sending follow-up message via /group/text API..."
FOLLOWUP_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/group/text?target=$ENCODED_TARGET&text=post+escape+followup")

echo "  /group/text response: $FOLLOWUP_RESP"

# 10. Wait for "Group text API injected" (stale detection worked) or "AskUserQuestion resolved" (bug)
ELAPSED=0
FOLLOWUP_RESULT=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  NEW_FOLLOWUP=$(tail -n +"$((LOG_BEFORE_FOLLOWUP + 1))" "$LOG_FILE")
  if echo "$NEW_FOLLOWUP" | grep "Group text API injected" > /dev/null 2>&1; then
    FOLLOWUP_RESULT="injected"
    break
  fi
  if echo "$NEW_FOLLOWUP" | grep "AskUserQuestion resolved via group text API" > /dev/null 2>&1; then
    FOLLOWUP_RESULT="swallowed"
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

# 11. Assert follow-up message was injected (not swallowed)
if [ "$FOLLOWUP_RESULT" = "injected" ]; then
  pass "Follow-up message correctly injected into CC (stale detection worked)"
elif [ "$FOLLOWUP_RESULT" = "swallowed" ]; then
  fail "Follow-up message was swallowed as AskUserQuestion answer — stale detection did NOT work (bug)"
else
  fail "Follow-up message result unclear within ${TIMEOUT}s (API response: $FOLLOWUP_RESP)"
fi

# 12. Wait for UserPromptSubmit to confirm CC received the follow-up
ELAPSED=0
UPS_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  NEW_UPS=$(tail -n +"$((LOG_BEFORE_FOLLOWUP + 1))" "$LOG_FILE")
  if echo "$NEW_UPS" | grep "UserPromptSubmit" > /dev/null 2>&1; then
    UPS_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

if [ "$UPS_FOUND" = true ]; then
  pass "CC received follow-up message (UserPromptSubmit triggered)"
else
  fail "CC did not receive follow-up message (no UserPromptSubmit within ${TIMEOUT}s)"
fi

# Rich-freeze regression guard (v9 ESC-freeze): the AskQ freeze edit after ESC uses the rich
# (RetryFreezeEditAuto→rich) path; a rich-payload rejection would log an "EDIT failed" line.
if tail -n +"$((LOG_BEFORE_ESC + 1))" "$LOG_FILE" | grep -qE "(DoCancelAsk|CancelAskBySnapshot|CleanupPendingState): EDIT failed" 2>/dev/null; then
  fail "AskUserQuestion rich freeze edit failed after escape (rich edit rejected)"
else
  pass "AskUserQuestion rich freeze edit succeeded after escape (no EDIT failed)"
fi
