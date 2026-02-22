#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Phase 2: Bot notification test ---"

ensure_infrastructure

# Verify PreToolUse hook registration
if grep "PreToolUse" "$TEST_SETTINGS" > /dev/null 2>&1; then
  pass "PreToolUse hook registered in settings"
else
  fail "PreToolUse hook not found in settings"
fi

if grep "UserPromptSubmit" "$TEST_SETTINGS" > /dev/null 2>&1; then
  pass "UserPromptSubmit hook registered in settings"
else
  fail "UserPromptSubmit hook not found in settings"
fi

# Check SessionStart notification
LOG_AFTER_START=$(wc -l < "$LOG_FILE")
if [ "$LOG_AFTER_START" -gt "$LOG_BEFORE" ]; then
  if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "SessionStart" > /dev/null 2>&1; then
    pass "SessionStart hook triggered and logged"
  else
    fail "SessionStart hook not found in bot log"
  fi
else
  fail "No new log entries after Claude start"
fi

# Send a simple command to trigger hook
LOG_BEFORE_HELLO=$(wc -l < "$LOG_FILE")
pane_log "[Phase 2] BEFORE 'say hello' prompt"
inject_prompt "say hello"
echo "Command sent, waiting for hook to trigger..."

# Wait for notification with polling
ELAPSED=0
FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_AFTER=$(wc -l < "$LOG_FILE")
  if [ "$LOG_AFTER" -gt "$LOG_BEFORE_HELLO" ]; then
    if tail -n +"$((LOG_BEFORE_HELLO + 1))" "$LOG_FILE" | grep "Notification sent" > /dev/null 2>&1; then
      FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[Phase 2] AFTER hook triggered"

if [ "$FOUND" = true ]; then
  pass "1st TG notification sent (hook → bot → TG)"
else
  fail "1st TG notification (no notification within ${TIMEOUT}s)"
fi

# Verify hook included tmux target in debug log
NEW_LOGS=$(tail -n +"$((LOG_BEFORE_HELLO + 1))" "$LOG_FILE")
if echo "$NEW_LOGS" | grep "Raw hook payload" > /dev/null 2>&1; then
  pass "Hook HTTP POST received by bot"
else
  fail "Hook HTTP POST not found in bot log"
fi

if tail -n +"$((LOG_BEFORE_HELLO + 1))" "$LOG_FILE" | grep "UserPromptSubmit position" > /dev/null 2>&1; then
  pass "UserPromptSubmit position recorded"
else
  fail "UserPromptSubmit position not found in bot log"
fi

# Lenient context window check: if statusline fired during this session,
# verify the notification text includes a correctly formatted Context line.
# Short E2E sessions may not trigger statusline — absence is acceptable.
CONTEXT_LOG=$(tail -n +"$((LOG_BEFORE_HELLO + 1))" "$LOG_FILE" | grep "Context:" || true)
if [ -n "$CONTEXT_LOG" ]; then
  # Context data present — verify format matches "Context: N% (Xk/Xk)" or "Context: N% (X.XM/X.XM)"
  if echo "$CONTEXT_LOG" | grep -E "Context: [0-9]+% \([0-9]+(\.[0-9]+)?[kM]/[0-9]+(\.[0-9]+)?[kM]\)" > /dev/null 2>&1; then
    pass "Context window usage present and correctly formatted in notification"
  else
    fail "Context window usage found but format is incorrect: $CONTEXT_LOG"
  fi
else
  pass "Context window usage absent (statusline not triggered in short session — OK)"
fi
