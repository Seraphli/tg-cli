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
