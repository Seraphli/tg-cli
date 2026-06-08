#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Codex tool notification test ---"

ensure_infrastructure

start_codex "e2e-codex-4"

# Verify toolNotifyList is configured in test config
TEST_APP_CONFIG="$TEST_CONFIG_DIR/config.json"
if [ -f "$TEST_APP_CONFIG" ] && grep -q "Bash" "$TEST_APP_CONFIG" 2>/dev/null; then
  pass "toolNotifyList includes Bash in test config"
else
  fail "toolNotifyList not configured in test config ($TEST_APP_CONFIG)"
fi

# Wait for Codex to settle
wait_for_idle

LOG_BEFORE_TOOL=$(wc -l < "$LOG_FILE")

pane_log "[codex/tool_notify] BEFORE tool notify prompt"
inject_prompt "Run this exact bash command and report the output: echo codex_tool_notify_test_ok"
pane_log "[codex/tool_notify] AFTER sending tool notify prompt"

# Wait for ToolUse notification in bot log
ELAPSED=0
TOOL_NOTIFY_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_TOOL" ]; then
    if tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep "Notification sent.*ToolUse" > /dev/null 2>&1; then
      TOOL_NOTIFY_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for ToolUse notification... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[codex/tool_notify] AFTER tool notification check"

if [ "$TOOL_NOTIFY_FOUND" = true ]; then
  pass "Codex ToolUse notification sent for Bash tool call"

  # Retry grep for the Debug full_text block — when the full E2E suite runs,
  # tee/file-buffer delay can make the Debug line lag the Info line by 1-3s.
  HEADER_FOUND=false
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE")
    if echo "$NEW_LOGS" | grep -A2 "TG message sent \[ToolUse\] full_text" | grep "🔧.*Bash" > /dev/null 2>&1; then
      HEADER_FOUND=true
      break
    fi
    sleep 1
  done
  if [ "$HEADER_FOUND" = true ]; then
    pass "Codex ToolUse TG message contains '🔧 Bash' header"
  else
    fail "Codex ToolUse TG message does not contain '🔧 Bash' header"
  fi
else
  fail "Codex ToolUse notification not received within ${TIMEOUT}s"
fi

# Wait for Stop notification to verify ordering
ELAPSED=0
STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
    STOP_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for Stop notification... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$STOP_FOUND" = true ]; then
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE")
  TOOL_LINE=$(awk '/Notification sent.*ToolUse/{print NR; exit}' <<< "$NEW_LOGS")
  STOP_LINE=$(awk '/Notification sent.*Stop/{print NR; exit}' <<< "$NEW_LOGS")
  if [ -n "$TOOL_LINE" ] && [ -n "$STOP_LINE" ] && [ "$TOOL_LINE" -lt "$STOP_LINE" ]; then
    pass "Codex ToolUse sent BEFORE Stop (line $TOOL_LINE < $STOP_LINE)"
  else
    fail "Codex ToolUse not sent before Stop (tool=$TOOL_LINE stop=$STOP_LINE)"
  fi
else
  fail "Codex Stop notification not received within ${TIMEOUT}s"
fi

# Wait for Codex to finish the turn
wait_for_idle
pane_log "[codex/tool_notify] AFTER Codex idle"
