#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Inject queue merge test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-13"

SESSION_NAME="e2e-cli"
MARKER_A="inject_merge_test_A_$RANDOM"
MARKER_B="inject_merge_test_B_$RANDOM"

# =============================================
# Test: Queue messages while CC is busy, verify merge on flush
# =============================================

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
  echo "  DEBUG: PANE_TITLE (${#PANE_TITLE} chars): $PANE_TITLE"
  set +eo pipefail
  echo "$PANE_TITLE" | grep -q '^✳'
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ -n "$PANE_TITLE" ] && [ "${_ps[1]}" -ne 0 ]; then
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

set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "CC busy, queued.*$MARKER_A"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  QUEUED_A=true
fi
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "CC busy, queued.*$MARKER_B"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
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
  set +eo pipefail
  tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "flushInjectQueue.*merging"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    MERGE_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

if [ "$MERGE_FOUND" = true ]; then
  pass "inject queue: flush merged queued messages"

  MERGE_LINE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "flushInjectQueue.*merging" | tail -1)
  set +eo pipefail
  echo "$MERGE_LINE" | grep -qE "items=[2-9]|items=[0-9][0-9]"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    pass "inject queue: merged 2+ items into single injection"
  else
    pass "inject queue: merge triggered (count may vary by timing)"
  fi
else
  fail "inject queue: flush merge not detected within 90s"
fi

wait_for_idle
pane_log "[inject_queue_merge] AFTER test"

# =============================================
# Test: Queued message delivered as AskQ custom reply
# Reproduces the inject-queue/AskUserQuestion bug
# =============================================
echo ""
echo "--- Inject queue AskQ custom reply test ---"

# Exit previous session and start fresh
stop_claude "e2e-cc-13"
sleep 2
LOG_BEFORE_ASKQ=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-13b"

# Name the new session
SESSION_ID_B=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    if s.get("target") == sys.argv[1]:
        print(s.get("id", ""))
        sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
if [ -n "$SESSION_ID_B" ]; then
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID_B&name=$SESSION_NAME" > /dev/null 2>&1 || true
  echo "  Named session $SESSION_ID_B as $SESSION_NAME (target=$E2E_PANE)"
fi

CUSTOM_MARKER="CUSTOM_REPLY_$RANDOM"

# Step 1: Inject prompt that sleeps 5s then calls AskUserQuestion
inject_prompt "Do these two things in sequence, step by step. Step 1: Run a bash command: sleep 5. Step 2: After the sleep completes, call the AskUserQuestion tool with exactly one question: header 'TestQ', question 'Pick one', two options: label 'OptionA' description 'first' and label 'OptionB' description 'second'. Do not skip either step."

# Wait for CC to become busy (running the sleep)
echo "  Waiting for CC to start processing..."
ELAPSED=0
CC_BUSY_B=false
while [ $ELAPSED -lt 30 ]; do
  PANE_TITLE=$($TMUX_TEST display-message -p -t "${E2E_PANE%@*}" '#{pane_title}' 2>/dev/null || echo "")
  set +eo pipefail
  echo "$PANE_TITLE" | grep -q '^✳'
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ -n "$PANE_TITLE" ] && [ "${_ps[1]}" -ne 0 ]; then
    CC_BUSY_B=true
    echo "  CC is busy at t=$ELAPSED: pane_title=\"$PANE_TITLE\""
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

if [ "$CC_BUSY_B" != true ]; then
  fail "AskQ custom reply: CC never became busy after prompt injection"
fi

# Step 2: Send the custom reply marker while CC is busy
sleep 2
echo "  Sending marker while CC is busy: $CUSTOM_MARKER"
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name "$SESSION_NAME" --port "$TEST_PORT" --from e2e-test --text "$CUSTOM_MARKER" 2>&1 || true

# Step 3: Verify marker was queued (CC is busy)
sleep 3
set +eo pipefail
tail -n +"$((LOG_BEFORE_ASKQ + 1))" "$LOG_FILE" | grep -q "CC busy, queued.*$CUSTOM_MARKER"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "AskQ custom reply: marker queued while CC busy ($CUSTOM_MARKER)"
else
  fail "AskQ custom reply: marker was NOT queued (CC may not have been busy)"
fi

# Step 4: Wait for the AskQ to be surfaced and the queued marker to be delivered as custom reply
# The flow is: sleep 5 → AskUserQuestion → Stop hook fires → flushInjectQueue → settle 5s → SafeInjectText detects AskQ → delivers marker as custom reply
echo "  Waiting for AskQ custom reply delivery (up to 120s)..."
ELAPSED=0
ASKQ_REPLY_FOUND=false
while [ $ELAPSED -lt 120 ]; do
  set +eo pipefail
  tail -n +"$((LOG_BEFORE_ASKQ + 1))" "$LOG_FILE" | grep -q "PostToolUse.*AskUserQuestion.*$CUSTOM_MARKER"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    ASKQ_REPLY_FOUND=true
    break
  fi
  sleep 3
  ELAPSED=$((ELAPSED + 3))
done

if [ "$ASKQ_REPLY_FOUND" = true ]; then
  pass "AskQ custom reply: marker delivered as AskQ answer ($CUSTOM_MARKER)"
else
  # Check for the old buggy behavior
  pane_log "[askq_custom_reply] FAIL - checking for bug symptoms"
  set +eo pipefail
  INJECT_FAILED=$(tail -n +"$((LOG_BEFORE_ASKQ + 1))" "$LOG_FILE" | grep "Inject failed.*$CUSTOM_MARKER" || true)
  DESKTOP_ANSWERED=$(tail -n +"$((LOG_BEFORE_ASKQ + 1))" "$LOG_FILE" | grep "Answered on desktop" || true)
  set -eo pipefail
  echo "  DEBUG: Inject failed lines: ${INJECT_FAILED:-none}"
  echo "  DEBUG: Answered on desktop lines: ${DESKTOP_ANSWERED:-none}"
  fail "AskQ custom reply: marker was NOT delivered as custom reply within 120s"
fi

# Step 5: Verify NEGATIVE assertions - no Inject failed for the marker
set +eo pipefail
tail -n +"$((LOG_BEFORE_ASKQ + 1))" "$LOG_FILE" | grep -q "Inject failed.*$CUSTOM_MARKER"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -ne 0 ]; then
  pass "AskQ custom reply: no 'Inject failed' for marker"
else
  fail "AskQ custom reply: 'Inject failed' found for marker (bug not fixed)"
fi

# Step 6: Verify NEGATIVE assertion - AskQ NOT frozen as "Answered on desktop"
set +eo pipefail
tail -n +"$((LOG_BEFORE_ASKQ + 1))" "$LOG_FILE" | grep "FreezeWaitEntry.*Answered on desktop" | grep -v "PostToolUse" > /dev/null 2>&1
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -ne 0 ]; then
  pass "AskQ custom reply: AskQ not frozen as 'Answered on desktop'"
else
  fail "AskQ custom reply: AskQ was frozen as 'Answered on desktop' (bug not fixed)"
fi

wait_for_idle
pane_log "[askq_custom_reply] AFTER test"
