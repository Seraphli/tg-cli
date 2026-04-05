#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- CapturePane test ---"

ensure_infrastructure

# Test CapturePane via HTTP API endpoint
ENCODED_PANE=$(printf '%s' "$E2E_PANE" | python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read()))")
CAPTURE_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/capture?target=$ENCODED_PANE")

if [ -n "$CAPTURE_RESP" ] && [ "$CAPTURE_RESP" != "null" ] && [ ${#CAPTURE_RESP} -gt 10 ]; then
  pass "CapturePane: /capture API returns content"
else
  fail "CapturePane: /capture API returned empty or error - response: $CAPTURE_RESP"
fi
