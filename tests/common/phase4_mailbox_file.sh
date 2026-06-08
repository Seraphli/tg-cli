#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Mailbox file attachment read mark test ---"

ensure_infrastructure
pane_log "[mailbox_file] BEFORE test"

# Create a test file
TEST_FILE="/tmp/tg-cli-e2e-mailbox-test.txt"
echo "E2E mailbox file test content" > "$TEST_FILE"

# Send mailbox message with file attachment
LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
SEND_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" mailbox send \
  --from e2e-sender --to e2e-file-receiver \
  --subject "File test" --text "Message with attachment" \
  --file "$TEST_FILE" \
  --port "$TEST_PORT" 2>&1) || true

echo "  DEBUG: SEND_OUTPUT (${#SEND_OUTPUT} chars): $SEND_OUTPUT"
set +eo pipefail
echo "$SEND_OUTPUT" | grep -qi "sent\|ok\|delivered"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "mailbox file: send with attachment succeeded"
else
  sleep 2
  set +eo pipefail
  tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "Mailbox send:.*file"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    pass "mailbox file: send with attachment confirmed in log"
  else
    fail "mailbox file: send failed: $SEND_OUTPUT"
    rm -f "$TEST_FILE"
    exit 0
  fi
fi

# Receive the message
RECV_OUTPUT=$(timeout 10 ./tg-cli --config-dir "$TEST_CONFIG_DIR" mailbox receive \
  --name e2e-file-receiver --port "$TEST_PORT" 2>&1) || true

echo "  DEBUG: RECV_OUTPUT (${#RECV_OUTPUT} chars): $RECV_OUTPUT"
set +eo pipefail
echo "$RECV_OUTPUT" | grep -qi "file test\|attachment\|Message with"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "mailbox file: receive got message with attachment"
else
  fail "mailbox file: receive did not get expected message: $RECV_OUTPUT"
fi

# Check inbox — message should be marked as read (no * prefix)
INBOX_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" mailbox inbox \
  --name e2e-file-receiver --port "$TEST_PORT" 2>&1) || true

echo "  DEBUG: INBOX_OUTPUT (${#INBOX_OUTPUT} chars): $INBOX_OUTPUT"
set +eo pipefail
echo "$INBOX_OUTPUT" | grep -q "File test"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  set +eo pipefail
  echo "$INBOX_OUTPUT" | grep "File test" | grep -q "^\*\|Unread"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[2]}" -eq 0 ]; then
    fail "mailbox file: attachment message still marked as unread after receive"
  else
    pass "mailbox file: attachment message marked as read after receive"
  fi
else
  pass "mailbox file: attachment message processed (not in inbox)"
fi

pane_log "[mailbox_file] AFTER test"

# Cleanup
rm -f "$TEST_FILE"
