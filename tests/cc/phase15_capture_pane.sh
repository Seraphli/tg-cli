#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- CapturePane test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-15"

pane_log "[capture_pane] BEFORE test"

# Test CapturePane via HTTP API endpoint
ENCODED_PANE=$(printf '%s' "$E2E_PANE" | python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read()))")
CAPTURE_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/capture?target=$ENCODED_PANE")

if [ -n "$CAPTURE_RESP" ] && [ "$CAPTURE_RESP" != "null" ] && [ ${#CAPTURE_RESP} -gt 10 ]; then
  pass "CapturePane: /capture API returns content"
else
  fail "CapturePane: /capture API returned empty or error - response: $CAPTURE_RESP"
fi
pane_log "[capture_pane] AFTER test"

# --- TC15-2: Single-page capture collapse button ---
echo ""
echo "  [TC15-2] Single-page capture collapse button"

SINGLE_JSON=$(python3 -c "import json; print(json.dumps({'target': '$E2E_PANE', 'content': 'hello-capture-single'}))")
pane_log "[TC15-2] BEFORE capture_message single"
CAP_RESP=$(curl -sf -X POST "http://127.0.0.1:$TEST_PORT/test/capture_message" \
  -H "Content-Type: application/json" \
  -d "$SINGLE_JSON")
echo "  DEBUG: TC15-2 capture_message RESP: $CAP_RESP"
pane_log "[TC15-2] AFTER capture_message single"

MID=$(echo "$CAP_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('msg_id',''))")
if [ -z "$MID" ]; then
  fail "TC15-2-0: capture_message returned no msg_id (resp=$CAP_RESP)"
fi

# Assert chunks==1
set +eo pipefail
echo "$CAP_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); sys.exit(0 if d.get('chunks')==1 else 1)"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-2-1: chunks==1 (single page)"
else
  fail "TC15-2-1: chunks!=1 (got $(echo "$CAP_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('chunks','?'))"))"
fi

# Assert buttons contains {text="📗 Collapse", unique="ce", data="c"}
set +eo pipefail
echo "$CAP_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
btns=d.get('buttons',[])
ok=any(b.get('text')=='📗 Collapse' and b.get('unique')=='ce' and b.get('data')=='c' for b in btns)
sys.exit(0 if ok else 1)
"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-2-2: buttons contains {text='📗 Collapse', unique='ce', data='c'}"
else
  fail "TC15-2-2: buttons missing collapse entry (buttons=$(echo "$CAP_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('buttons',[]))"))"
fi

# TC15-4: capture buttons include delete
set +eo pipefail
echo "$CAP_RESP" | python3 -c "import sys,json; sys.exit(0 if any(b.get('unique')=='del' for b in json.load(sys.stdin).get('buttons',[])) else 1)"
_rc=$?; set -eo pipefail
[ "$_rc" -eq 0 ] && pass "TC15-4: capture buttons include del" || fail "TC15-4: capture missing del (resp=$CAP_RESP)"

# Assert page_entry fields
pane_log "[TC15-2] BEFORE page_entry check"
PE_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/page_entry?msg_id=$MID")
echo "  DEBUG: TC15-2 page_entry RESP: $PE_RESP"
pane_log "[TC15-2] AFTER page_entry check"

set +eo pipefail
echo "$PE_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
ok = (d.get('exists') and
      '📺 Pane Capture' in d.get('header','') and
      d.get('collapsed') == False and
      d.get('current_page') == 1 and
      d.get('raw_mode') == True)
sys.exit(0 if ok else 1)
"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-2-3: page_entry header/collapsed/current_page/raw_mode correct"
else
  fail "TC15-2-3: page_entry fields wrong (resp=$PE_RESP)"
fi

# Collapse via ce data=c
pane_log "[TC15-2] BEFORE collapse"
COLLAPSE_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$MID&unique=ce&data=c")
echo "  DEBUG: TC15-2-4 collapse RESP: $COLLAPSE_RESP"
pane_log "[TC15-2] AFTER collapse"

set +eo pipefail
echo "$COLLAPSE_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
ok = (d.get('collapsed') == True and
      d.get('text','').strip() == '📺 Pane Capture')
sys.exit(0 if ok else 1)
"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-2-4: collapse → collapsed=true, text=='📺 Pane Capture'"
else
  fail "TC15-2-4: collapse wrong (collapsed=$(echo "$COLLAPSE_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('collapsed','?'))") text=$(echo "$COLLAPSE_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(repr(d.get('text','?')))"))"
fi

# TC15-4b: del persists after collapse
set +eo pipefail
echo "$COLLAPSE_RESP" | python3 -c "import sys,json; sys.exit(0 if 'del' in json.load(sys.stdin).get('buttons',[]) else 1)"
_rc=$?; set -eo pipefail
[ "$_rc" -eq 0 ] && pass "TC15-4b: del persists after collapse" || fail "TC15-4b: del lost after collapse (resp=$COLLAPSE_RESP)"

# Expand via ce data=e
pane_log "[TC15-2] BEFORE expand"
EXPAND_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$MID&unique=ce&data=e")
echo "  DEBUG: TC15-2-5 expand RESP (${#EXPAND_RESP} chars)"
pane_log "[TC15-2] AFTER expand"

set +eo pipefail
echo "$EXPAND_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
ok = (d.get('collapsed') == False and
      'hello-capture-single' in d.get('text',''))
sys.exit(0 if ok else 1)
"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-2-5: expand → collapsed=false, text contains hello-capture-single"
else
  fail "TC15-2-5: expand wrong (collapsed=$(echo "$EXPAND_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('collapsed','?'))") has_content=$(echo "$EXPAND_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print('hello-capture-single' in d.get('text',''))"))"
fi

# TC15-4c: del persists after expand
set +eo pipefail
echo "$EXPAND_RESP" | python3 -c "import sys,json; sys.exit(0 if 'del' in json.load(sys.stdin).get('buttons',[]) else 1)"
_rc=$?; set -eo pipefail
[ "$_rc" -eq 0 ] && pass "TC15-4c: del persists after expand" || fail "TC15-4c: del lost after expand (resp=$EXPAND_RESP)"

# TC15-5/6: delete and re-delete (LAST since they delete $MID)
CHAT=$(echo "$CAP_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('chat_id',''))")
DEL_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
DEL_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$MID&unique=del&chat_id=$CHAT")
set +eo pipefail
echo "$DEL_RESP" | python3 -c "import sys,json; sys.exit(0 if json.load(sys.stdin).get('status')=='deleted' else 1)"
_rc=$?; set -eo pipefail
[ "$_rc" -eq 0 ] && pass "TC15-5-1: del returns status=deleted" || fail "TC15-5-1: del failed (resp=$DEL_RESP)"
sleep 1
set +eo pipefail
tail -n +"$((DEL_BEFORE + 1))" "$LOG_FILE" | grep -q "del callback: deleted msg_id=$MID"
_ps=("${PIPESTATUS[@]}"); set -eo pipefail
[ "${_ps[1]}" -eq 0 ] && pass "TC15-5-2: bot log 'del callback: deleted msg_id=$MID'" || fail "TC15-5-2: del log missing"
DEL2_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$MID&unique=del&chat_id=$CHAT")
set +eo pipefail
echo "$DEL2_RESP" | python3 -c "import sys,json; sys.exit(0 if json.load(sys.stdin).get('status')=='error' else 1)"
_rc=$?; set -eo pipefail
[ "$_rc" -eq 0 ] && pass "TC15-6: re-delete returns error (msg removed)" || fail "TC15-6: re-delete did not error (resp=$DEL2_RESP)"

# --- TC15-3: Multi-page capture CurrentPage restore (config threshold paginationMaxRunes=500) ---
echo ""
echo "  [TC15-3] Multi-page capture collapse button with CurrentPage restore"

# Build content: __P1_MARKER__ + line-numbered filler + __P2_MARKER__
# paginationMaxRunes=500 in test config, so ~900 runes total triggers chunks>=2
# Use python3 to build well-formed JSON body to avoid shell quoting issues
MULTI_JSON=$(python3 -c "
import json
lines = ['__P1_MARKER__']
for i in range(1, 31):
    lines.append(f'line-{i}-abcdefghij')
lines.append('__P2_MARKER__')
content = '\n'.join(lines)
print(json.dumps({'target': '$E2E_PANE', 'content': content}))
")

pane_log "[TC15-3] BEFORE capture_message multi"
CAP3_RESP=$(curl -sf -X POST "http://127.0.0.1:$TEST_PORT/test/capture_message" \
  -H "Content-Type: application/json" \
  -d "$MULTI_JSON")
echo "  DEBUG: TC15-3 capture_message RESP: $CAP3_RESP"
pane_log "[TC15-3] AFTER capture_message multi"

MID3=$(echo "$CAP3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('msg_id',''))")
if [ -z "$MID3" ]; then
  fail "TC15-3-0: capture_message returned no msg_id (resp=$CAP3_RESP)"
fi

# Assert chunks>=2
set +eo pipefail
echo "$CAP3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); sys.exit(0 if d.get('chunks',0)>=2 else 1)"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  TOTAL_CHUNKS=$(echo "$CAP3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('chunks',0))")
  pass "TC15-3-1: chunks>=2 (got $TOTAL_CHUNKS)"
else
  fail "TC15-3-1: chunks<2 — paginationMaxRunes config may not be set to 500 (resp=$CAP3_RESP)"
fi

# Assert initial current_page==chunks (starts at last page)
set +eo pipefail
echo "$CAP3_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
sys.exit(0 if d.get('current_page')==d.get('chunks') else 1)
"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-3-2: initial current_page==chunks (starts at last page)"
else
  fail "TC15-3-2: current_page=$(echo "$CAP3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('current_page','?'))") != chunks=$(echo "$CAP3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('chunks','?'))")"
fi

# Assert buttons include unique="p" and unique="ce"
set +eo pipefail
echo "$CAP3_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
btns=d.get('buttons',[])
has_p=any(b.get('unique')=='p' for b in btns)
has_ce=any(b.get('unique')=='ce' for b in btns)
sys.exit(0 if has_p and has_ce else 1)
"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-3-3: buttons include pagination (unique=p) and collapse (unique=ce)"
else
  fail "TC15-3-3: buttons missing p or ce (buttons=$(echo "$CAP3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('buttons',[]))"))"
fi

# Navigate to page 1: text must contain __P1_MARKER__
pane_log "[TC15-3] BEFORE navigate p=1"
P1_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$MID3&unique=p&data=1")
echo "  DEBUG: TC15-3-4 p=1 RESP (${#P1_RESP} chars)"
pane_log "[TC15-3] AFTER navigate p=1"

set +eo pipefail
echo "$P1_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
sys.exit(0 if '__P1_MARKER__' in d.get('text','') else 1)
"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-3-4: p=1 text contains __P1_MARKER__"
else
  fail "TC15-3-4: p=1 text missing __P1_MARKER__ (text_len=${#P1_RESP})"
fi

# Navigate to page 2: text must contain __P2_MARKER__ and NOT __P1_MARKER__
pane_log "[TC15-3] BEFORE navigate p=2"
P2_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$MID3&unique=p&data=2")
echo "  DEBUG: TC15-3-5 p=2 RESP (${#P2_RESP} chars)"
pane_log "[TC15-3] AFTER navigate p=2"

set +eo pipefail
echo "$P2_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
text=d.get('text','')
sys.exit(0 if '__P2_MARKER__' in text and '__P1_MARKER__' not in text else 1)
"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-3-5: p=2 text contains __P2_MARKER__ and not __P1_MARKER__"
else
  fail "TC15-3-5: p=2 marker check failed (has_P2=$(echo "$P2_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print('__P2_MARKER__' in d.get('text',''))") has_P1=$(echo "$P2_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print('__P1_MARKER__' in d.get('text',''))"))"
fi

# TC15-4d: del persists after multi-page p-nav (capture del survives pagination)
set +eo pipefail
echo "$P2_RESP" | python3 -c "import sys,json; sys.exit(0 if 'del' in json.load(sys.stdin).get('buttons',[]) else 1)"
_rc=$?; set -eo pipefail
[ "$_rc" -eq 0 ] && pass "TC15-4d: del persists after multi-page p-nav" || fail "TC15-4d: del lost after p-nav (resp=$P2_RESP)"

# Assert page_entry current_page==2
pane_log "[TC15-3] BEFORE page_entry after p=2"
PE3_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/page_entry?msg_id=$MID3")
echo "  DEBUG: TC15-3-6 page_entry RESP: $PE3_RESP"
pane_log "[TC15-3] AFTER page_entry after p=2"

set +eo pipefail
echo "$PE3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); sys.exit(0 if d.get('current_page')==2 else 1)"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-3-6: page_entry current_page==2 after navigation"
else
  fail "TC15-3-6: page_entry current_page=$(echo "$PE3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('current_page','?'))") (expected 2)"
fi

# Collapse
pane_log "[TC15-3] BEFORE collapse"
C3_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$MID3&unique=ce&data=c")
echo "  DEBUG: TC15-3-7 collapse RESP: $C3_RESP"
pane_log "[TC15-3] AFTER collapse"

set +eo pipefail
echo "$C3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); sys.exit(0 if d.get('collapsed')==True else 1)"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-3-7: collapsed=true after collapse"
else
  fail "TC15-3-7: collapse failed (collapsed=$(echo "$C3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('collapsed','?'))"))"
fi

# Expand: must restore to page 2 (contain __P2_MARKER__ and NOT __P1_MARKER__)
pane_log "[TC15-3] BEFORE expand"
E3_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$MID3&unique=ce&data=e")
echo "  DEBUG: TC15-3-8 expand RESP (${#E3_RESP} chars)"
pane_log "[TC15-3] AFTER expand"

set +eo pipefail
echo "$E3_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
text=d.get('text','')
ok = (d.get('collapsed')==False and
      '__P2_MARKER__' in text and
      '__P1_MARKER__' not in text)
sys.exit(0 if ok else 1)
"
_rc=$?
set -eo pipefail
if [ "$_rc" -eq 0 ]; then
  pass "TC15-3-8: expand restores to page 2 (collapsed=false, P2 present, P1 absent)"
else
  fail "TC15-3-8: expand did not restore to page 2 (collapsed=$(echo "$E3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('collapsed','?'))") has_P2=$(echo "$E3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print('__P2_MARKER__' in d.get('text',''))") has_P1=$(echo "$E3_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print('__P1_MARKER__' in d.get('text',''))"))"
fi
