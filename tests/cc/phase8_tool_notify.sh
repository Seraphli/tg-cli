#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Tool notification test ---"

ensure_infrastructure

# Verify toolNotifyList is configured in test config
TEST_APP_CONFIG="$TEST_CONFIG_DIR/config.json"
if [ -f "$TEST_APP_CONFIG" ] && grep -q "Bash" "$TEST_APP_CONFIG" 2>/dev/null; then
  pass "toolNotifyList includes Bash in test config"
else
  fail "toolNotifyList not configured in test config ($TEST_APP_CONFIG)"
fi

# Record log position before triggering a Bash tool call
LOG_BEFORE_TOOL=$(wc -l < "$LOG_FILE")

pane_log "[tool_notify] BEFORE tool notify prompt"
# Inject a prompt that causes Claude to run a Bash command
inject_prompt "Run this exact bash command and report the output: echo tool_notify_test_ok"
pane_log "[tool_notify] AFTER sending tool notify prompt"

# Wait for ToolUse notification in bot log
ELAPSED=0
TOOL_NOTIFY_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_TOOL" ]; then
    if tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep "Notification sent.*: ToolUse " > /dev/null 2>&1; then
      TOOL_NOTIFY_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for ToolUse notification... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[tool_notify] AFTER tool notification check"

if [ "$TOOL_NOTIFY_FOUND" = true ]; then
  pass "ToolUse notification sent for Bash tool call"

  # Verify the full TG message contains "🔧 Bash" header (in DEBUG full_text log)
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE")
  if echo "$NEW_LOGS" | grep -A2 "TG message sent \[ToolUse\] full_text" | grep "🔧.*Bash" > /dev/null 2>&1; then
    pass "ToolUse TG message contains '🔧 Bash' header"
  else
    fail "ToolUse TG message does not contain '🔧 Bash' header"
  fi
else
  fail "ToolUse notification not received within ${TIMEOUT}s"
fi

# Wait for Stop notification to verify ordering and independence
LOG_BEFORE_STOP13=$(wc -l < "$LOG_FILE")
ELAPSED=0
STOP13_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_TOOL" ]; then
    if tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
      STOP13_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for Stop notification... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$STOP13_FOUND" = true ]; then
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE")

  # Extract line numbers for ToolUse and Stop notifications
  TOOL_LINE=$(awk '/Notification sent.*ToolUse/{print NR; exit}' <<< "$NEW_LOGS")
  STOP_LINE=$(awk '/Notification sent.*Stop/{print NR; exit}' <<< "$NEW_LOGS")

  # Verify both lines exist and are different (independence)
  if [ -n "$TOOL_LINE" ] && [ -n "$STOP_LINE" ] && [ "$TOOL_LINE" != "$STOP_LINE" ]; then
    pass "ToolUse and Stop are independent notifications (line $TOOL_LINE != $STOP_LINE)"
  else
    fail "ToolUse and Stop notifications not independent (tool=$TOOL_LINE stop=$STOP_LINE)"
  fi

  # Verify ToolUse line < Stop line (ordering)
  if [ -n "$TOOL_LINE" ] && [ -n "$STOP_LINE" ] && [ "$TOOL_LINE" -lt "$STOP_LINE" ]; then
    pass "ToolUse sent BEFORE Stop (line $TOOL_LINE < $STOP_LINE)"
  else
    fail "ToolUse not sent before Stop (tool=$TOOL_LINE stop=$STOP_LINE)"
  fi

  # Verify ToolUse full TG message contains 🔧 header (not ✅ Task Completed)
  TOOL_FULL_TEXT=$(echo "$NEW_LOGS" | grep -A2 "TG message sent \[ToolUse\] full_text" || true)
  if echo "$TOOL_FULL_TEXT" | grep "🔧" > /dev/null 2>&1; then
    pass "ToolUse notification contains correct 🔧 header"
  else
    fail "ToolUse notification missing 🔧 header (may show wrong header like '✅ Task Completed')"
  fi
  if echo "$TOOL_FULL_TEXT" | grep "✅ Task Completed" > /dev/null 2>&1; then
    fail "ToolUse notification incorrectly shows '✅ Task Completed' header"
  else
    pass "ToolUse notification does not show '✅ Task Completed' header"
  fi
else
  fail "Stop notification not received within ${TIMEOUT}s for ordering verification"
fi

# Wait for CC to finish the turn
wait_for_idle
pane_log "[tool_notify] AFTER CC idle"

# Verify PostToolUse message update occurred (Bash tool call above should trigger it)
if tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep -q "PostToolUse: updated msg_id="; then
  pass "PostToolUse message update detected in bot log"
else
  # Not a hard failure — PostToolUse only fires if tool is in toolNotifyList and msg was sent
  fail "PostToolUse message update not detected in bot log"
fi

# Verify PostToolUse result has content (not empty)
POST_RESULT_LEN=$(tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep -o "result_len=[0-9]*" | head -1 | cut -d= -f2)
if [ -n "$POST_RESULT_LEN" ] && [ "$POST_RESULT_LEN" -gt 0 ] 2>/dev/null; then
  pass "PostToolUse result has content (result_len=$POST_RESULT_LEN)"
else
  fail "PostToolUse result empty or not found (result_len=$POST_RESULT_LEN)"
fi

# Verify builtinTools expansion includes Skill and Other (static code check)
if grep -q '"Skill"' cmd/handlers/register.go && grep -q '"Other"' cmd/handlers/register.go; then
  pass "builtinTools includes Skill and Other"
else
  fail "builtinTools missing Skill or Other in cmd/handlers/register.go"
fi

# Verify safeInjectText decision path logging exists in bot log (inject_prompt above triggers it)
if tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep -q "safeInjectText:"; then
  pass "safeInjectText decision path logging detected"
else
  fail "safeInjectText decision path logging not detected in bot log"
fi
