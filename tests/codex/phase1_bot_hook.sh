#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Codex bot notification test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE")
start_codex "e2e-codex-1"

# Check SessionStart notification fired during startup
LOG_AFTER_START=$(wc -l < "$LOG_FILE")
if [ "$LOG_AFTER_START" -gt "$LOG_BEFORE" ]; then
  if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "SessionStart" > /dev/null 2>&1; then
    pass "Codex SessionStart hook triggered and logged"
  else
    fail "Codex SessionStart hook not found in bot log"
  fi
else
  fail "No new log entries after Codex start"
fi

# Wait for Codex to be fully idle before injecting
echo "Waiting for Codex to be idle..."
wait_for_idle

# Inject a simple prompt and wait for Stop notification
LOG_BEFORE_HELLO=$(wc -l < "$LOG_FILE")
pane_log "[codex/bot_hook] BEFORE 'say hello' prompt"
inject_prompt "Reply with exactly one word: hello"
echo "Command sent, waiting for Stop hook to trigger..."

ELAPSED=0
FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_HELLO" ]; then
    if tail -n +"$((LOG_BEFORE_HELLO + 1))" "$LOG_FILE" | grep "Notification sent" > /dev/null 2>&1; then
      FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[codex/bot_hook] AFTER hook triggered"

if [ "$FOUND" = true ]; then
  pass "Codex TG notification sent (hook -> bot -> TG)"
else
  fail "Codex TG notification not received within ${TIMEOUT}s"
fi

# Verify hook HTTP POST received by bot
NEW_LOGS=$(tail -n +"$((LOG_BEFORE_HELLO + 1))" "$LOG_FILE")
if echo "$NEW_LOGS" | grep "Raw hook payload" > /dev/null 2>&1; then
  pass "Codex hook HTTP POST received by bot"
else
  fail "Codex hook HTTP POST not found in bot log"
fi

# Verify backend=codex is recorded in session (JSON format in raw payload)
if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep '"backend":"codex"\|backend.*codex' > /dev/null 2>&1; then
  pass "Session registered with backend=codex"
else
  fail "Session backend=codex not found in bot log"
fi

# Typing continuity: skipped for phase1 (short "say hello" runs < 3s)
echo "  Note: typing continuity skipped (short prompt, < tick interval)"
