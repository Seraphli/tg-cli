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
d['toolNotifyList']=['Read','Bash','Edit','Glob','Grep','Other']
json.dump(d,open(f,'w'))
"

# Create temp test files
echo "compact test 1" > /tmp/tg-cli-test-compact-1.txt
echo "compact test 2" > /tmp/tg-cli-test-compact-2.txt
echo "compact test 3" > /tmp/tg-cli-test-compact-3.txt

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || true)

# TC1 + TC2 + TC3: Inject an EXPLICIT step-by-step prompt (per lesson [2026-02-12]/[2026-06-04]).
# The old vague prompt "Read these 3 files one by one" let CC narrate / use other tools freely, which
# made TC2's "no standard ToolUse notifications" assertion non-deterministic. Spell out the whole flow
# (Read tool ONLY, one file per step, no other tool) so the asserted behavior is pinned, like TC6.
pane_log "[compact] BEFORE inject"
inject_prompt "Do these steps strictly in order, one at a time. Finish each step completely before starting the next. Use only the Read tool and do not run any other tool (no Bash, no Grep, no Glob, no TodoWrite). Do not read more than one file per step. Step 1: read /tmp/tg-cli-test-compact-1.txt. Step 2: read /tmp/tg-cli-test-compact-2.txt. Step 3: read /tmp/tg-cli-test-compact-3.txt. After all three reads are done, tell me the contents of the three files."
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
d['toolNotifyList']=['Read','Other']
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

# TC6: Verify a new MessageDisplay text block resets the compact cycle WITHIN one turn → separate
# compact messages (register.go:619). The old prompt "Read these 3 files one by one" never told CC
# to narrate before each read, so CC legally batched the 3 reads under one text block → 1 reset
# boundary → only 1 "compact tool sent" (round-1 FAIL, /tmp/e2e-fix1-stage1.txt:2843). Fix per
# project lesson [2026-02-12]: the prompt MUST explicitly instruct text-before-each-tool. The prompt
# below spells out the operation flow step by step and forbids batching, so CC produces
# text→read→text→read→text→read = 3 distinct MessageDisplay messages, each resetting the cycle.
# Use FRESH, UNIQUE files (NOT the TC1 files, whose content is already in this CC session's context —
# CC could otherwise answer from context and skip the Read tool, dropping the sent count). Unique
# content forces a real Read of each file.
python3 -c "
import json
f='$TEST_CONFIG_DIR/config.json'
d=json.load(open(f))
d['toolNotifyCompact']=True
d['toolNotifyList']=['Read','Other']
json.dump(d,open(f,'w'))
"
TC6_FILE1="/tmp/tg-cli-test-compact-tc6-1-$$.txt"
TC6_FILE2="/tmp/tg-cli-test-compact-tc6-2-$$.txt"
TC6_FILE3="/tmp/tg-cli-test-compact-tc6-3-$$.txt"
echo "compact tc6 unique 1 $RANDOM" > "$TC6_FILE1"
echo "compact tc6 unique 2 $RANDOM" > "$TC6_FILE2"
echo "compact tc6 unique 3 $RANDOM" > "$TC6_FILE3"
LOG_BEFORE_TC6=$(wc -l < "$LOG_FILE" 2>/dev/null || true)
pane_log "[compact_tc6] BEFORE inject"
inject_prompt "Do these steps strictly in order, one at a time. Finish each step completely before starting the next. Do NOT read more than one file per step, and do NOT mention or read a later file before its own step. Step 1: first output the sentence \"Now reading file 1.\" then read $TC6_FILE1. Step 2: first output the sentence \"Now reading file 2.\" then read $TC6_FILE2. Step 3: first output the sentence \"Now reading file 3.\" then read $TC6_FILE3."
wait_for_idle
pane_log "[compact_tc6] AFTER idle"
set +eo pipefail
SENT_COUNT_TC6=$(tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE" | grep -c "compact tool sent" || true)
set -eo pipefail
rm -f "$TC6_FILE1" "$TC6_FILE2" "$TC6_FILE3"
if [ "$SENT_COUNT_TC6" -ge 2 ]; then
  pass "TC6: new text resets compact cycle within a turn (sent=$SENT_COUNT_TC6 for 3 narrated reads)"
else
  fail "TC6: expected sent>=2 (each narrated text block resets compact cycle, register.go:619), got sent=$SENT_COUNT_TC6"
fi

# TC7 (round 8): SEND-anchored text-before-tool ordering. The bounded PreToolUse wait
# (streamFlushAwaitToolBoundary) must flush the pre-tool text to Telegram BEFORE the tool notification.
# Anchor on bot SEND log lines ('Stream send:' for a text bubble, 'compact tool sent' for the tool),
# NOT MessageDisplay delta-receipt lines. Before round 8 the tool notification could overtake the text
# (PreToolUse hook arrives before MessageDisplay; StreamFlush returned instantly on a stale bubble).
set +eo pipefail
TC6_SLICE=$(tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE")
FIRST_TEXT_LINE=$(printf '%s\n' "$TC6_SLICE" | grep -n "Stream send:" | head -1 | cut -d: -f1)
FIRST_TOOL_LINE=$(printf '%s\n' "$TC6_SLICE" | grep -n "compact tool sent" | head -1 | cut -d: -f1)
LATE_MD_COUNT=$(printf '%s\n' "$TC6_SLICE" | grep -c "late MD after tool notify")
set -eo pipefail
if [ -n "$FIRST_TEXT_LINE" ] && [ -n "$FIRST_TOOL_LINE" ] && [ "$FIRST_TEXT_LINE" -lt "$FIRST_TOOL_LINE" ]; then
  pass "TC7: pre-tool text SEND precedes tool-notification SEND (first Stream send line=$FIRST_TEXT_LINE < first compact tool sent line=$FIRST_TOOL_LINE)"
else
  fail "TC7: text-before-tool ordering FAIL (first Stream send=$FIRST_TEXT_LINE, first compact tool sent=$FIRST_TOOL_LINE)"
fi
# TC8 (round 8): late-MD residual-inversion marker is countable; 0 = no inversions (informational, not a gate).
echo "  TC8 [info]: late-MD residual-inversion markers in TC6 = ${LATE_MD_COUNT:-0} (0 = ideal)"

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
