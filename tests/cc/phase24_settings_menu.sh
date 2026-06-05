#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Settings menu test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Test 1: Verify test callback endpoint is registered
RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=1&unique=settings&data=voice&chat_id=12345" 2>&1 || true)
echo "  DEBUG: RESP (${#RESP} chars): $RESP"
set +eo pipefail
echo "$RESP" | grep -q '"status"'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep '\"status\"' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  set +eo pipefail
  echo "$RESP" | grep -q '"ok"'
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  echo "  DEBUG: grep '\"ok\"' PIPESTATUS=${_ps[*]}"
  if [ "${_ps[1]}" -eq 0 ]; then
    pass "Test callback endpoint registered and responding"
  else
    fail "Test callback endpoint returned unexpected response: $RESP"
  fi
else
  fail "Test callback endpoint not responding: $RESP"
fi

# Test 2: Verify settings-related log entry
sleep 1
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "Test callback"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'Test callback' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Settings callback logged"
else
  fail "Settings callback not found in log"
fi

# TC24-3: settings menu has delete button (send a real settings message)
SET_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/settings_message?chat_id=$DEFAULT_CHAT_ID")
SMID=$(echo "$SET_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('msg_id',''))")
[ -z "$SMID" ] && fail "TC24-3-0: settings_message no msg_id (resp=$SET_RESP)"
set +eo pipefail
echo "$SET_RESP" | python3 -c "import sys,json; sys.exit(0 if any(b.get('unique')=='del' for b in json.load(sys.stdin).get('buttons',[])) else 1)"
_rc=$?; set -eo pipefail
[ "$_rc" -eq 0 ] && pass "TC24-3-1: settings menu includes del button" || fail "TC24-3-1: settings menu missing del (resp=$SET_RESP)"

# TC24-2: each sub-menu actually renders on the real settings message (real, not default-case)
for sub in voice cwd toolnotify perm routes mailbox status cron; do
  R=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$SMID&unique=settings&data=$sub&chat_id=$DEFAULT_CHAT_ID" 2>&1 || echo "CURLFAIL")
  set +eo pipefail
  echo "$R" | python3 -c "import sys,json; sys.exit(0 if json.load(sys.stdin).get('status')=='ok' else 1)" 2>/dev/null
  _rc=$?; set -eo pipefail
  [ "$_rc" -eq 0 ] && pass "TC24-2: settings/$sub renders ok" || fail "TC24-2: settings/$sub failed (resp=$R)"
done

# TC24-3-2: del removes the settings message
DEL_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$SMID&unique=del&chat_id=$DEFAULT_CHAT_ID")
set +eo pipefail
echo "$DEL_RESP" | python3 -c "import sys,json; sys.exit(0 if json.load(sys.stdin).get('status')=='deleted' else 1)"
_rc=$?; set -eo pipefail
[ "$_rc" -eq 0 ] && pass "TC24-3-2: settings menu deleted via del" || fail "TC24-3-2: settings del failed (resp=$DEL_RESP)"
