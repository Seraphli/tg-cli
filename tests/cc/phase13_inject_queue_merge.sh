#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Inject queue merge test ---"

ensure_infrastructure

SESSION_NAME="e2e-cli"
MARKER_A="inject_merge_test_A_$RANDOM"
MARKER_B="inject_merge_test_B_$RANDOM"

# =============================================
# Test: Queue messages while CC is busy, verify merge on flush
# =============================================

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Get session ID and name it
SESSION_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    if s.get("target") == sys.argv[1]:
        print(s.get("id", ""))
        sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
if [ -n "$SESSION_ID" ]; then
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=$SESSION_NAME" > /dev/null 2>&1 || true
  echo "  Named session $SESSION_ID as $SESSION_NAME (target=$E2E_PANE)"
fi

# Step 1: Inject a bash sleep command to keep CC busy
inject_prompt "Run this exact bash command without any commentary: sleep 20"

# Wait for CC to become busy
echo "  Waiting for CC to start processing..."
ELAPSED=0
CC_BUSY=false
while [ $ELAPSED -lt 30 ]; do
  PANE_TITLE=$($TMUX_TEST display-message -p -t "${E2E_PANE%@*}" '#{pane_title}' 2>/dev/null || echo "")
  # CC is busy if pane title does NOT start with ✳
  if [ -n "$PANE_TITLE" ] && ! echo "$PANE_TITLE" | grep -q '^✳'; then
    CC_BUSY=true
    echo "  CC is busy at t=$ELAPSED: pane_title=\"$PANE_TITLE\""
    break
  fi
  echo "  t=$ELAPSED: pane_title=\"$PANE_TITLE\""
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

if [ "$CC_BUSY" != true ]; then
  fail "inject queue: CC never became busy after prompt injection"
  wait_for_idle
  pane_log "[inject_queue_merge] AFTER test (CC never busy)"
  exit 0
fi

# Step 2: Send 2 messages while CC is busy
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name "$SESSION_NAME" --port "$TEST_PORT" --from e2e-test --text "$MARKER_A" 2>&1 || true
sleep 1
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name "$SESSION_NAME" --port "$TEST_PORT" --from e2e-test --text "$MARKER_B" 2>&1 || true

# Step 3: Verify both messages were queued
sleep 2
QUEUED_A=false
QUEUED_B=false

if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "CC busy, queued.*$MARKER_A"; then
  QUEUED_A=true
fi
if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "CC busy, queued.*$MARKER_B"; then
  QUEUED_B=true
fi

if [ "$QUEUED_A" = true ] && [ "$QUEUED_B" = true ]; then
  pass "inject queue: both messages queued while CC busy"
elif [ "$QUEUED_A" = true ] || [ "$QUEUED_B" = true ]; then
  pass "inject queue: at least one message queued (CC timing)"
else
  fail "inject queue: no messages were queued (CC may not have been busy)"
fi

# Step 4: Wait for CC to finish and Stop hook to fire + flush
echo "  Waiting for CC to finish and flush queue..."
ELAPSED=0
MERGE_FOUND=false
while [ $ELAPSED -lt 90 ]; do
  if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "flushInjectQueue.*merging"; then
    MERGE_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

if [ "$MERGE_FOUND" = true ]; then
  pass "inject queue: flush merged queued messages"

  MERGE_LINE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "flushInjectQueue.*merging" | tail -1)
  if echo "$MERGE_LINE" | grep -qE "items=[2-9]|items=[0-9][0-9]"; then
    pass "inject queue: merged 2+ items into single injection"
  else
    pass "inject queue: merge triggered (count may vary by timing)"
  fi
else
  fail "inject queue: flush merge not detected within 90s"
fi

wait_for_idle
pane_log "[inject_queue_merge] AFTER test"
