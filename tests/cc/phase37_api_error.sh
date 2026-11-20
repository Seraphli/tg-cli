#!/bin/bash
# A real cc turn that ends on an API error sends ONE Run Error notification (StopFailure hook, not Stop).
# The model name does not exist, so the real API answers model_not_found at once. The name is an argument of
# start_claude (not ANTHROPIC_MODEL), so the cc process holds no ANTHROPIC_ variable and stays on the subscription login.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- API error turn: Run Error notification ---"

ensure_infrastructure

if grep "StopFailure" "$TEST_SETTINGS" > /dev/null 2>&1; then
  pass "StopFailure hook registered in settings"
else
  fail "StopFailure hook not found in settings"
fi

start_claude "e2e-cc-37" "--dangerously-skip-permissions" "true" "claude-nonexistent-0"

LOG_BEFORE_ERR=$(wc -l < "$LOG_FILE")
pane_log "[api_error] BEFORE prompt"
inject_prompt "say hello"

ELAPSED=0
FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_ERR + 1))" "$LOG_FILE" | grep -E "Notification sent to chat .*: AgentError" > /dev/null 2>&1; then
    FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting... ${ELAPSED}s / ${TIMEOUT}s"
done
pane_log "[api_error] AFTER prompt"
wait_for_idle

SLICE=$(tail -n +"$((LOG_BEFORE_ERR + 1))" "$LOG_FILE")
# The harness removes the per-run config dir (and bot.log) after a passing run: keep the two lines that prove the path in the E2E log.
echo "--- bot.log lines of the failed turn (StopFailure payload, AgentError notification) ---"
grep -E "Raw hook payload \[StopFailure\]|Notification sent to chat .*: AgentError" <<< "$SLICE" | cut -c1-1500 || true
if [ "$FOUND" = true ]; then
  pass "Run Error notification sent for the failed turn"
else
  fail "no AgentError notification within ${TIMEOUT}s"
fi
if grep -E "Notification sent to chat .*: AgentError" <<< "$SLICE" | grep "cc run error" | grep "claude-nonexistent-0" > /dev/null 2>&1; then
  pass "Run Error body holds 'cc run error' and the model name from the pane's error line"
else
  fail "AgentError body lacks 'cc run error' or the model name"
fi
if grep -E "Notification sent to chat .*: Stop " <<< "$SLICE" > /dev/null 2>&1; then
  fail "a Stop notification was sent for the failed turn"
else
  pass "no Stop notification for the failed turn"
fi
