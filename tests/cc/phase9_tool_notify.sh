#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Tool notification test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-9"

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
inject_prompt "Use the Bash tool to run this exact command and report the output: echo tool_notify_test_ok"
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

  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE")

  # Verify the full TG message contains "🔧 Bash" header.
  # The full_text is multi-line; awk extracts all lines after the marker until the next timestamp line.
  TOOL_FULL_TEXT_REGION=$(printf '%s\n' "$NEW_LOGS" | awk '
    /TG message sent \[ToolUse\] full_text:/ { capture=1; next }
    capture && /^\[[0-9]{4}-/ { capture=0 }
    capture { print }
  ')
  if printf '%s\n' "$TOOL_FULL_TEXT_REGION" | grep -q "🔧.*Bash" 2>/dev/null; then
    pass "ToolUse TG message contains '🔧 Bash' header"
  else
    fail "ToolUse TG message does not contain '🔧 Bash' header"
  fi

  # TC8: ToolUse body uses collapsed <details> (no 'open' attribute) — rich format
  if printf '%s\n' "$TOOL_FULL_TEXT_REGION" | grep -q "<details>" 2>/dev/null; then
    pass "TC8: ToolUse notification uses <details> (collapsed rich details block)"
  else
    fail "TC8: ToolUse notification missing <details> element"
  fi
  if printf '%s\n' "$TOOL_FULL_TEXT_REGION" | grep -q "<details open" 2>/dev/null; then
    fail "TC8: ToolUse <details> has 'open' attribute — expected collapsed (no 'open')"
  else
    pass "TC8: ToolUse <details> is collapsed (no 'open' attribute)"
  fi
else
  fail "ToolUse notification not received within ${TIMEOUT}s"
fi

# Wait for Stream relabel ✅ to verify ordering: ToolUse notification before stream finalize
LOG_BEFORE_STOP13=$(wc -l < "$LOG_FILE")
ELAPSED=0
STOP13_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_TOOL" ]; then
    # Accept EITHER the StreamFlush relabel path (Stream relabel ✅:) OR the dump-at-Stop
    # delivery path (: Stop [ / Stop terminal: outcome=direct_send). They are mutually
    # exclusive per turn — a dump-at-Stop turn never emits the relabel line.
    if tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
      STOP13_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for Stream relabel ✅ (or Stop-delivery)... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$STOP13_FOUND" = true ]; then
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE")

  # Extract line numbers for ToolUse notification and the finalize anchor.
  # FINALIZE line = relabel line if present, ELSE the Stop-delivery marker line.
  TOOL_LINE=$(awk '/Notification sent.*ToolUse/{print NR; exit}' <<< "$NEW_LOGS")
  RELABEL_LINE=$(awk '/Stream relabel ✅:/{print NR; exit}' <<< "$NEW_LOGS")
  if [ -z "$RELABEL_LINE" ]; then
    RELABEL_LINE=$(awk '/: Stop \[|Stop terminal: outcome=direct_send/{print NR; exit}' <<< "$NEW_LOGS")
  fi

  # Verify both lines exist and are different (independence)
  if [ -n "$TOOL_LINE" ] && [ -n "$RELABEL_LINE" ] && [ "$TOOL_LINE" != "$RELABEL_LINE" ]; then
    pass "ToolUse notification and Stream relabel are independent log lines (line $TOOL_LINE != $RELABEL_LINE)"
  else
    fail "ToolUse and Stream relabel not independent (tool=$TOOL_LINE relabel=$RELABEL_LINE)"
  fi

  # Verify ToolUse line < Stream relabel line (ordering: tool card before finalize)
  if [ -n "$TOOL_LINE" ] && [ -n "$RELABEL_LINE" ] && [ "$TOOL_LINE" -lt "$RELABEL_LINE" ]; then
    pass "ToolUse sent BEFORE Stream relabel ✅ (line $TOOL_LINE < $RELABEL_LINE)"
  else
    fail "ToolUse not sent before Stream relabel (tool=$TOOL_LINE relabel=$RELABEL_LINE)"
  fi

  # Verify ToolUse full TG message contains 🔧 header (not ✅ Message)
  TOOL_FULL_TEXT=$(echo "$NEW_LOGS" | grep -A2 "TG message sent \[ToolUse\] full_text" || true)
  if echo "$TOOL_FULL_TEXT" | grep "🔧" > /dev/null 2>&1; then
    pass "ToolUse notification contains correct 🔧 header"
  else
    fail "ToolUse notification missing 🔧 header"
  fi
  if echo "$TOOL_FULL_TEXT" | grep "✅ Task Completed" > /dev/null 2>&1; then
    fail "ToolUse notification incorrectly shows '✅ Task Completed' header"
  else
    pass "ToolUse notification does not show '✅ Task Completed' header"
  fi
else
  fail "Neither Stream relabel ✅ nor Stop-delivery received within ${TIMEOUT}s for ordering verification"
fi

# Wait for CC to finish the turn
wait_for_idle
pane_log "[tool_notify] AFTER CC idle"

# Verify PostToolUse message update occurred (Bash tool call above should trigger it)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep -q "PostToolUse: updated msg_id="
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "PostToolUse message update detected in bot log"
else
  # Not a hard failure — PostToolUse only fires if tool is in toolNotifyList and msg was sent
  fail "PostToolUse message update not detected in bot log"
fi

# Verify PostToolUse result has content (not empty)
set +eo pipefail
POST_RESULT_LEN=$(tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep -o "result_len=[0-9]*" | head -1 | cut -d= -f2)
set -eo pipefail
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
# Use grep -c instead of grep -q to avoid SIGPIPE under pipefail
SAFE_INJECT_COUNT=$(tail -n +"$((LOG_BEFORE_TOOL + 1))" "$LOG_FILE" | grep -c "safeInjectText:" || true)
if [ "$SAFE_INJECT_COUNT" -gt 0 ]; then
  pass "safeInjectText decision path logging detected"
else
  fail "safeInjectText decision path logging not detected in bot log"
fi
