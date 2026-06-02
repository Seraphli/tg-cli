#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Compact tool notification test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-26"

pane_log "[compact] BEFORE test"

# Enable compact mode in config
python3 -c "
import json
f='$TEST_CONFIG_DIR/config.json'
d=json.load(open(f))
d['toolNotifyCompact']=True
d['toolNotifyList']=['Read','Bash','Edit','Glob','Grep']
json.dump(d,open(f,'w'))
"

# Create temp test files
echo "compact test 1" > /tmp/tg-cli-test-compact-1.txt
echo "compact test 2" > /tmp/tg-cli-test-compact-2.txt
echo "compact test 3" > /tmp/tg-cli-test-compact-3.txt

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || true)

# TC1 + TC2 + TC3: Inject prompt that triggers multiple Read tool calls
pane_log "[compact] BEFORE inject"
inject_prompt "Read these 3 files one by one and tell me their content: /tmp/tg-cli-test-compact-1.txt /tmp/tg-cli-test-compact-2.txt /tmp/tg-cli-test-compact-3.txt"
pane_log "[compact] AFTER inject"

wait_for_idle
pane_log "[compact] AFTER idle"

# TC1: Verify compact notification was sent with tool name
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "compact tool sent"
_ps_sent=("${PIPESTATUS[@]}")
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "compact tool sent.*tool=Read"
_ps_read=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps_sent[1]}" -eq 0 ] && [ "${_ps_read[1]}" -eq 0 ]; then
  pass "TC1: compact tool notification sent with Read"
else
  fail "TC1: compact tool notification NOT sent or missing Read (sent=${_ps_sent[1]} read=${_ps_read[1]})"
fi

# TC2: Verify compact mode replaced standard ToolUse notifications
set +eo pipefail
COMPACT_COUNT=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -c "compact tool sent\|compact tool edited" || true)
STANDARD_COUNT=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -v "compact" | grep -c "Notification sent.*ToolUse" || true)
set -eo pipefail
if [ "$COMPACT_COUNT" -ge 1 ] && [ "$STANDARD_COUNT" -eq 0 ]; then
  pass "TC2: compact mode active, no standard ToolUse notifications (compact=$COMPACT_COUNT standard=$STANDARD_COUNT)"
else
  fail "TC2: expected compact>=1 and standard=0, got compact=$COMPACT_COUNT standard=$STANDARD_COUNT"
fi

# TC3: Verify PostToolUse did NOT edit compact message
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "PostToolUse: updated"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -ne 0 ]; then
  pass "TC3: PostToolUse did not edit compact message"
else
  fail "TC3: PostToolUse unexpectedly edited compact message"
fi

# TC4: Whitelist filtering — enable compact with only Read, trigger a prompt that uses Bash
LOG_BEFORE_TC4=$(wc -l < "$LOG_FILE" 2>/dev/null || true)
python3 -c "
import json
f='$TEST_CONFIG_DIR/config.json'
d=json.load(open(f))
d['toolNotifyCompact']=True
d['toolNotifyList']=['Read']
json.dump(d,open(f,'w'))
"

pane_log "[compact_tc4] BEFORE inject"
inject_prompt "Run the command: echo compact_whitelist_test"
pane_log "[compact_tc4] AFTER inject"

wait_for_idle
pane_log "[compact_tc4] AFTER idle"

set +eo pipefail
tail -n +"$((LOG_BEFORE_TC4 + 1))" "$LOG_FILE" | grep -q "compact tool.*Bash"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -ne 0 ]; then
  pass "TC4: Bash not in compact notification (whitelist filtering works)"
else
  fail "TC4: Bash appeared in compact notification despite not being in whitelist"
fi

# TC5: Settings toggle via test endpoint
# First set compact=false so toggle produces true
python3 -c "
import json
f='$TEST_CONFIG_DIR/config.json'
d=json.load(open(f))
d['toolNotifyCompact']=False
json.dump(d,open(f,'w'))
"
RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/test/config/compact" 2>&1 || true)
set +eo pipefail
echo "$RESP" | grep -q '"compact":true'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  # Toggle back
  curl -s "http://127.0.0.1:$TEST_PORT/test/config/compact" > /dev/null 2>&1 || true
  pass "TC5: Compact toggle endpoint works"
else
  fail "TC5: Compact toggle endpoint returned unexpected response: $RESP"
fi

# TC6: Verify Update-between-tools creates separate compact cycles (correct per SPEC)
# CC outputs intermediate text before each Read, so each Read gets its own compact message
set +eo pipefail
SENT_COUNT_TC6=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -c "compact tool sent" || true)
set -eo pipefail
if [ "$SENT_COUNT_TC6" -ge 2 ]; then
  pass "TC6: Update-resets-cycle works (sent=$SENT_COUNT_TC6 compact messages for 3 reads)"
else
  fail "TC6: expected sent>=2 (intermediate text resets cycle), got sent=$SENT_COUNT_TC6"
fi

# Restore config for subsequent phases
python3 -c "
import json
f='$TEST_CONFIG_DIR/config.json'
d=json.load(open(f))
d['toolNotifyCompact']=False
d['toolNotifyList']=['Bash','AskUserQuestion','Read']
json.dump(d,open(f,'w'))
"

# Cleanup temp files
rm -f /tmp/tg-cli-test-compact-{1,2,3}.txt
