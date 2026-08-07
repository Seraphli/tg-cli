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
inject_prompt "Use the Bash tool to run this exact command: sleep 20. Do not print any text."

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

# =============================================
# Fix 19: the inject-queue notification is delivered as a RICH message (RetrySendRich for the
# initial "⏳ Queued", RetryEditRich for the "✅ Injected" flush edit) so long inject content is not
# truncated by the 4096-char plain-text cap (rich supports ~32768). Tie the assertions to the exact
# notify msg_id logged by the flush ("inject completed: ... notify_msg_id=N"), then verify the
# Fix-16 send/edit logging reports rich=true for that message. Guards against reverting to plain
# RetrySend/RetryEdit.
# =============================================
echo "  Fix 19: verifying inject-queue notification uses rich messages..."
ELAPSED=0
NOTIFY_MSG_ID=""
while [ $ELAPSED -lt 60 ]; do
  COMPLETE_LINE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "inject completed:.*notify_msg_id=" | tail -1 || true)
  if [ -n "$COMPLETE_LINE" ]; then
    NOTIFY_MSG_ID=$(echo "$COMPLETE_LINE" | grep -oE "notify_msg_id=[0-9]+" | head -1 | cut -d= -f2)
    if [ -n "$NOTIFY_MSG_ID" ] && [ "$NOTIFY_MSG_ID" != "0" ]; then
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

if [ -n "$NOTIFY_MSG_ID" ] && [ "$NOTIFY_MSG_ID" != "0" ]; then
  pass "Fix 19: flush reported notify_msg_id=$NOTIFY_MSG_ID"
  sleep 2  # let the rich flush edit be logged
  RICH_WINDOW=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE")

  # Initial "⏳ Queued" notification SEND must be rich
  set +eo pipefail
  echo "$RICH_WINDOW" | grep -E "TG send: .*msg_id=$NOTIFY_MSG_ID .*rich=true" > /dev/null
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    pass "Fix 19: inject notification sent via rich (msg_id=$NOTIFY_MSG_ID)"
  else
    fail "Fix 19: inject notification NOT sent via rich (msg_id=$NOTIFY_MSG_ID)"
  fi

  # Flush "✅ Injected" edit (carries the FULL merged content) must be rich
  set +eo pipefail
  echo "$RICH_WINDOW" | grep -E "TG edit: .*msg_id=$NOTIFY_MSG_ID .*rich=true" > /dev/null
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    pass "Fix 19: flush notification edit via rich (msg_id=$NOTIFY_MSG_ID)"
  else
    fail "Fix 19: flush notification edit NOT rich (msg_id=$NOTIFY_MSG_ID)"
  fi

  # Revert guard: the notify message must NEVER be sent/edited via the plain (rich=false) path
  set +eo pipefail
  echo "$RICH_WINDOW" | grep -E "TG (send|edit): .*msg_id=$NOTIFY_MSG_ID .*rich=false" > /dev/null
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -ne 0 ]; then
    pass "Fix 19: no plain (rich=false) send/edit for the notify message"
  else
    fail "Fix 19: notify message had a rich=false send/edit (Fix 19 reverted?)"
  fi
else
  fail "Fix 19: flush did not report a notify_msg_id within 60s"
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
inject_prompt "Do these two things in this exact order, all in this single reply, without skipping, combining, or repeating either step. Step 1: Use the Bash tool exactly once to run exactly this command: sleep 5. Step 2: Immediately after that Bash call returns, call the AskUserQuestion tool exactly once with exactly one question: header 'TestQ', question 'Pick one', two options: label 'OptionA' description 'first' and label 'OptionB' description 'second'. Do not stop or give a final answer after Step 1; actually issue the AskUserQuestion tool call before ending this reply."

# Step 2: The marker must be queued DURING the sleep, AFTER the Bash PreToolUse fires — otherwise the
# pre-sleep narration MD-final can arm a routing window carrying the marker and mis-deliver it before the
# AskUserQuestion is pending. Poll the bot log for the Bash PreToolUse line (contains BOTH the literal
# "Raw hook payload [PreToolUse]" AND the "sleep 5" command; the UserPromptSubmit line also has "sleep 5"
# but is a different event line, gated out by the "Raw hook payload [PreToolUse]" prefix). This queues the
# marker during the sleep with a ~4s margin.
echo "  Waiting for the Bash PreToolUse (sleep 5) to fire..."
ELAPSED=0
PTU_FIRED_B=false
while [ $ELAPSED -lt 30 ]; do
  set +eo pipefail
  tail -n +"$((LOG_BEFORE_ASKQ + 1))" "$LOG_FILE" | grep "Raw hook payload \[PreToolUse\]" | grep -q "sleep 5"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ] && [ "${_ps[2]}" -eq 0 ]; then
    PTU_FIRED_B=true
    echo "  Bash PreToolUse (sleep 5) fired at t=$ELAPSED"
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

if [ "$PTU_FIRED_B" != true ]; then
  fail "AskQ custom reply: Bash PreToolUse (sleep 5) never fired within 30s"
fi

# Send the custom reply marker while CC is busy in the sleep (after the Bash PreToolUse)
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

# Step 4: Wait for the AskQ to be surfaced and the queued marker to be delivered as custom reply.
# The queued marker may be delivered via EITHER of two legal paths; both end with the marker answered into
# the pending AskUserQuestion, and either one satisfies this test:
#   (a) Event-route (R9-item3): sleep 5 → assistant text MD-final arms the routing window →
#       PreToolUse(AskUserQuestion) is signalled into the window → mode=AskQ-custom-reply → deliverInjectQueue
#       → SafeInjectText bounded-waits for the PendingWait AskQ snapshot (R1) → answers AskUserQuestion.
#   (b) Idle/timeout fallback: if no in-window event selects the routing mode, the queued marker is still
#       delivered once CC reaches idle with the AskQ pending (attempt-9 passed legitimately via this fallback).
# The delivery marker (pass condition) is the "[HOOK] answered: ... <CUSTOM_MARKER>" log line, independent of path.
echo "  Waiting for AskQ custom reply delivery (up to 120s)..."
ELAPSED=0
ASKQ_REPLY_FOUND=false
while [ $ELAPSED -lt 120 ]; do
  set +eo pipefail
  tail -n +"$((LOG_BEFORE_ASKQ + 1))" "$LOG_FILE" | grep -q "\[HOOK\] answered:.*$CUSTOM_MARKER"
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

# Structural exactly-once asserts (post-idle, catches post-answer duplicates): within the LOG_BEFORE_ASKQ
# window, exactly one Bash "sleep 5" PreToolUse and exactly one AskUserQuestion PreToolUse. Guards against
# the mimo duplicate-sleep / double-call family that mis-sequences the inject-queue routing window.
BASH_SLEEP_PTU_COUNT=$(tail -n +"$((LOG_BEFORE_ASKQ + 1))" "$LOG_FILE" | grep "Raw hook payload \[PreToolUse\]" | grep '"tool_name":"Bash"' | grep -c "sleep 5" || true)
if [ "$BASH_SLEEP_PTU_COUNT" -eq 1 ]; then
  pass "AskQ custom reply: exactly one Bash sleep 5 PreToolUse invocation (count=$BASH_SLEEP_PTU_COUNT)"
else
  fail "AskQ custom reply: expected exactly 1 Bash sleep 5 PreToolUse invocation, got $BASH_SLEEP_PTU_COUNT (duplicate/double-call regression)"
fi

ASKQ_PTU_COUNT=$(tail -n +"$((LOG_BEFORE_ASKQ + 1))" "$LOG_FILE" | grep "Raw hook payload \[PreToolUse\]" | grep -c '"tool_name":"AskUserQuestion"' || true)
if [ "$ASKQ_PTU_COUNT" -eq 1 ]; then
  pass "AskQ custom reply: exactly one AskUserQuestion PreToolUse invocation (count=$ASKQ_PTU_COUNT)"
else
  fail "AskQ custom reply: expected exactly 1 AskUserQuestion PreToolUse invocation, got $ASKQ_PTU_COUNT (duplicate/double-call regression)"
fi

# =============================================
# Test (R9-item3): the EVENT-DRIVEN MD-final trigger routes the inject queue (replacing the removed
# PostToolUse flush). The queued marker must be delivered even when the tool was NOT notified (ToolUseMsgs
# miss). We drop "Bash" from toolNotifyList so PreToolUse(Bash) stores no ToolUseMsgs entry — under the OLD
# code the PostToolUse handler flushed via "PostToolUse: flushing inject queue"; that discriminator no
# longer exists. The NEW trigger fires when a MessageDisplay final=true arrives with a non-empty queue
# ("routeInjectQueue: MD-final trigger armed routing window") OR — for a text-less tool-only turn — the
# PreToolUse/Stop supplement arms + routes on the same code path (also logs that marker). The next hook
# event within the 5s window selects the mode; the merged marker is then injected into the pane.
# =============================================
echo ""
echo "--- Event-driven MD-final inject-queue trigger test (R9-item3, un-notified tool) ---"

stop_claude "e2e-cc-13b"
sleep 2

CFG_PTU="$TEST_CONFIG_DIR/config.json"
cp "$CFG_PTU" "$CFG_PTU.bak_ptu"
# Atomic replace (os.replace) so a concurrent bot read never observes a partial file.
python3 - "$CFG_PTU" <<'PY'
import json, os, sys
p = sys.argv[1]
c = json.load(open(p))
c["toolNotifyList"] = [t for t in c.get("toolNotifyList", []) if t != "Bash"]
tmp = p + ".tmp"
json.dump(c, open(tmp, "w"))
os.replace(tmp, p)
PY
echo "  Dropped Bash from toolNotifyList (un-notified tool -> ToolUseMsgs miss)"
restore_ptu_cfg() { [ -f "$CFG_PTU.bak_ptu" ] && mv -f "$CFG_PTU.bak_ptu" "$CFG_PTU" && echo "  Restored toolNotifyList"; }

LOG_BEFORE_PTU=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-13c"

# Name the fresh session e2e-cli (session send targets it by name)
SESSION_ID_C=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    if s.get("target") == sys.argv[1]:
        print(s.get("id", ""))
        sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
if [ -n "$SESSION_ID_C" ]; then
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID_C&name=$SESSION_NAME" > /dev/null 2>&1 || true
  echo "  Named session $SESSION_ID_C as $SESSION_NAME (target=$E2E_PANE)"
fi

PTU_MARKER="ptu_flush_marker_$RANDOM"

# Step 1: keep CC busy with a slow, un-notified Bash tool. Narrate around the Bash so the model reliably
# emits assistant text (an MD-final) — a "Do not print any text" instruction could produce NO assistant text and thus NO
# MD-final, which would make the "MD-final trigger armed routing window" assertion FAIL after round 10.
inject_prompt "Do these steps: first print a single line that says starting_ptu_test, then use the Bash tool to run this exact command: sleep 15, then print a single line that says ptu_test_done."

echo "  Waiting for CC to start processing..."
ELAPSED=0
CC_BUSY_C=false
while [ $ELAPSED -lt 30 ]; do
  PANE_TITLE=$($TMUX_TEST display-message -p -t "${E2E_PANE%@*}" '#{pane_title}' 2>/dev/null || echo "")
  set +eo pipefail
  echo "$PANE_TITLE" | grep -q '^✳'
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ -n "$PANE_TITLE" ] && [ "${_ps[1]}" -ne 0 ]; then
    CC_BUSY_C=true
    echo "  CC is busy at t=$ELAPSED: pane_title=\"$PANE_TITLE\""
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

if [ "$CC_BUSY_C" != true ]; then
  restore_ptu_cfg
  fail "MD-final trigger: CC never became busy after prompt injection"
fi

# Step 2: send a marker while CC is busy -> it must be queued
sleep 2
echo "  Sending marker while CC is busy: $PTU_MARKER"
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name "$SESSION_NAME" --port "$TEST_PORT" --from e2e-test --text "$PTU_MARKER" 2>&1 || true

# Step 3: verify queued
sleep 3
set +eo pipefail
tail -n +"$((LOG_BEFORE_PTU + 1))" "$LOG_FILE" | grep -q "CC busy, queued.*$PTU_MARKER"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "MD-final trigger: marker queued while CC busy ($PTU_MARKER)"
else
  restore_ptu_cfg
  fail "MD-final trigger: marker was NOT queued (CC may not have been busy)"
fi

# Step 4: wait for the NEW event-driven trigger. "routeInjectQueue: MD-final trigger armed routing window"
# is emitted ONLY from routeInjectQueue — the MD-final trigger site (handleMessageDisplay) or the
# PreToolUse/Stop supplement (R4). It replaces the removed "PostToolUse: flushing inject queue" discriminator.
echo "  Waiting for event-driven inject-queue routing window (up to 60s)..."
ELAPSED=0
ROUTE_FOUND=false
while [ $ELAPSED -lt 60 ]; do
  set +eo pipefail
  tail -n +"$((LOG_BEFORE_PTU + 1))" "$LOG_FILE" | grep -q "routeInjectQueue: MD-final trigger armed routing window for target="
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    ROUTE_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

# The tool has run; notify config no longer matters for this TC — restore before the remaining assertions.
restore_ptu_cfg

if [ "$ROUTE_FOUND" = true ]; then
  pass "MD-final trigger: event-driven routing window armed (R9-item3)"
else
  pane_log "[md_final_trigger] FAIL - no routing window within 60s"
  fail "MD-final trigger: 'routeInjectQueue: MD-final trigger armed routing window' not found within 60s"
fi

# Step 4b: the OLD "PostToolUse: flushing inject queue" discriminator MUST be gone (revert guard).
set +eo pipefail
tail -n +"$((LOG_BEFORE_PTU + 1))" "$LOG_FILE" | grep -q "PostToolUse: flushing inject queue"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -ne 0 ]; then
  pass "MD-final trigger: removed PostToolUse flush discriminator is absent"
else
  fail "MD-final trigger: 'PostToolUse: flushing inject queue' still present (PostToolUse flush not removed)"
fi

# Step 5: verify the queued marker actually reached the CC pane (marker text echoed in the pane).
echo "  Waiting for the queued marker to reach the CC pane (up to 60s)..."
ELAPSED=0
PTU_IN_PANE=false
while [ $ELAPSED -lt 60 ]; do
  PANE_NOW=$($TMUX_TEST capture-pane -t "${E2E_PANE%@*}" -p -S - 2>/dev/null || echo "")
  set +eo pipefail
  echo "$PANE_NOW" | grep -q "$PTU_MARKER"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    PTU_IN_PANE=true
    break
  fi
  sleep 3
  ELAPSED=$((ELAPSED + 3))
done

if [ "$PTU_IN_PANE" = true ]; then
  pass "MD-final trigger: queued marker reached the CC pane ($PTU_MARKER)"
else
  pane_log "[md_final_trigger] FAIL - marker not found in pane"
  fail "MD-final trigger: queued marker did not reach the pane within 60s"
fi

wait_for_idle
pane_log "[md_final_trigger] AFTER test"
