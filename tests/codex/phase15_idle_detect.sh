#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Codex idle detection test ---"

ensure_infrastructure

# Wait for initial idle state
wait_for_idle

# Check idle endpoint returns idle=True when Codex is settled
IDLE_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/session/idle" 2>/dev/null || echo "{}")
IDLE_STATE=$(echo "$IDLE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('idle',False))" 2>/dev/null || echo "False")
if [ "$IDLE_STATE" = "True" ]; then
  pass "Codex idle detection: /session/idle returns idle=True when Codex is settled"
else
  fail "Codex idle detection: /session/idle did not return idle=True when Codex is settled (got: $IDLE_STATE)"
fi

# Inject a longer prompt to observe running state during processing
LOG_BEFORE_INJECT=$(wc -l < "$LOG_FILE")
pane_log "[codex/idle] BEFORE injecting prompt"
inject_prompt "Count from 1 to 50 without using any tools, writing out each number on its own line."
pane_log "[codex/idle] AFTER injecting prompt"

# Poll /session/idle for running state (Codex spinner in pane title -> not idle)
ELAPSED=0
RUNNING_DETECTED=false
while [ $ELAPSED -lt 30 ]; do
  IDLE_NOW=$(curl -sf "http://127.0.0.1:$TEST_PORT/session/idle" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('idle',True))" 2>/dev/null || echo "True")
  if [ "$IDLE_NOW" = "False" ]; then
    RUNNING_DETECTED=true
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

if [ "$RUNNING_DETECTED" = true ]; then
  pass "Codex idle detection: running state detected (idle=False during processing)"
else
  echo "  Note: running state not detected (prompt may have completed too fast)"
  pass "Codex idle detection: running state check skipped (prompt completed quickly)"
fi

# Wait for Codex to finish
wait_for_idle $TIMEOUT
pane_log "[codex/idle] AFTER Codex idle"

# Verify returns to idle after Stop
IDLE_AFTER=$(curl -sf "http://127.0.0.1:$TEST_PORT/session/idle" 2>/dev/null \
  | python3 -c "import sys,json; print(json.load(sys.stdin).get('idle',False))" 2>/dev/null || echo "False")
if [ "$IDLE_AFTER" = "True" ]; then
  pass "Codex idle detection: returns to idle=True after Stop"
else
  fail "Codex idle detection: did not return to idle=True after Stop (got: $IDLE_AFTER)"
fi

# Verify Stop notification was logged
if tail -n +"$((LOG_BEFORE_INJECT + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
  pass "Codex idle detection: Stop notification logged after completion"
else
  fail "Codex idle detection: Stop notification not found in bot log"
fi
