#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- CLI send-file test ---"

ensure_infrastructure

# Create test file
TEST_FILE="/tmp/tg-cli-e2e-send-file-test.txt"
echo "CLI send-file test content - $(date)" > "$TEST_FILE"

LOG_BEFORE=$(wc -l < "$LOG_FILE")
pane_log "[send_file] BEFORE CLI send-file"

# Run CLI send-file command directly
SEND_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" send-file --file "$TEST_FILE" --caption "E2E test file" --port "$TEST_PORT" 2>&1) || true

pane_log "[send_file] AFTER CLI send-file"

# Check CLI output for success
echo "  DEBUG: SEND_OUTPUT (${#SEND_OUTPUT} chars): $SEND_OUTPUT"
set +eo pipefail
echo "$SEND_OUTPUT" | grep -q "File sent"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'File sent' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "CLI send-file: command returned success"
else
  fail "CLI send-file: command output unexpected - $SEND_OUTPUT"
fi

# Check bot log for [File] File sent
ELAPSED=0
FILE_FOUND=false
while [ $ELAPSED -lt 30 ]; do
  if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "\[File\] File sent" > /dev/null 2>&1; then
    FILE_FOUND=true
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

if [ "$FILE_FOUND" = true ]; then
  pass "CLI send-file: [File] File sent logged"
else
  fail "CLI send-file: [File] File sent not found in bot log within 30s"
fi

# Cleanup
rm -f "$TEST_FILE"
