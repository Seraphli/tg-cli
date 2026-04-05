#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Codex PostToolUse notification test ---"

ensure_infrastructure

# Wait for Codex to settle
wait_for_idle $TIMEOUT

LOG_BEFORE_TOOL=$(wc -l < "$LOG_FILE")

pane_log "[codex/post_tool] BEFORE PostToolUse prompt"
inject_prompt "Run this exact bash command and report the output: echo codex_post_tool_ok"
pane_log "[codex/post_tool] AFTER sending PostToolUse prompt"

# Wait for PostToolUse log entry in bot log
ELAPSED=0
POST_TOOL_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_TOOL" ]; then
    if tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep "PostToolUse" > /dev/null 2>&1; then
      POST_TOOL_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for PostToolUse log entry... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[codex/post_tool] AFTER PostToolUse check"

if [ "$POST_TOOL_FOUND" = true ]; then
  pass "Codex PostToolUse hook log entry found"
else
  fail "Codex PostToolUse log entry not found within ${TIMEOUT}s"
fi

# Wait for Stop notification
wait_for_idle $TIMEOUT
ELAPSED=0
STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
    STOP_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

if [ "$STOP_FOUND" = true ]; then
  pass "Codex Stop notification received after PostToolUse"

  # Verify ordering: PostToolUse before Stop
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE")
  POST_LINE=$(awk '/PostToolUse/{print NR; exit}' <<< "$NEW_LOGS")
  STOP_LINE=$(awk '/Notification sent.*Stop/{print NR; exit}' <<< "$NEW_LOGS")
  if [ -n "$POST_LINE" ] && [ -n "$STOP_LINE" ] && [ "$POST_LINE" -lt "$STOP_LINE" ]; then
    pass "Codex PostToolUse occurs before Stop (line $POST_LINE < $STOP_LINE)"
  else
    fail "Codex PostToolUse ordering issue (post=$POST_LINE stop=$STOP_LINE)"
  fi
else
  fail "Codex Stop notification not received within ${TIMEOUT}s"
fi

pane_log "[codex/post_tool] AFTER Codex idle"
