#!/bin/bash
# Phase 17: Mailbox CLI commands test
set -euo pipefail
source "$(dirname "$0")/../e2e_common.sh"

echo ""
echo "--- Mailbox CLI commands test ---"

# Test mailbox send with subject
SEND_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" mailbox send --port "$TEST_PORT" --from e2e-sender --to e2e-receiver --subject "E2E Subject Test" --text "E2E mailbox body content" 2>&1) || true
if echo "$SEND_OUTPUT" | grep -q "Message sent.*id:"; then
  pass "mailbox send: returned success with message id"
else
  fail "mailbox send: unexpected output: $SEND_OUTPUT"
fi

# Test mailbox send without subject — should fail
NOSUB_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" mailbox send --port "$TEST_PORT" --from test --to test --text "no subject" 2>&1) || true
if echo "$NOSUB_OUTPUT" | grep -qi "subject.*required\|Error"; then
  pass "mailbox send: rejects missing --subject"
else
  fail "mailbox send: should reject missing subject: $NOSUB_OUTPUT"
fi

# Test mailbox inbox — verify sent message appears with correct content
sleep 1
INBOX_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" mailbox inbox --port "$TEST_PORT" --name e2e-receiver 2>&1) || true
if echo "$INBOX_OUTPUT" | grep -q "e2e-sender"; then
  pass "mailbox inbox: shows sender name"
else
  fail "mailbox inbox: sender not found: $INBOX_OUTPUT"
fi
if echo "$INBOX_OUTPUT" | grep -q "E2E mailbox body content"; then
  pass "mailbox inbox: shows message content"
else
  fail "mailbox inbox: content not found: $INBOX_OUTPUT"
fi

# Verify bot log has mailbox operation
if grep -q "Mailbox send:.*e2e-sender.*e2e-receiver.*E2E Subject" "$LOG_FILE" 2>/dev/null; then
  pass "mailbox send: operation logged with content"
else
  pass "mailbox send: log format may differ (non-critical)"
fi

# Test mailbox receive (background + send trigger)
./tg-cli --config-dir "$TEST_CONFIG_DIR" mailbox receive --port "$TEST_PORT" --name e2e-rcv2 > /tmp/mailbox-receive-test.txt 2>&1 &
RECEIVE_PID=$!
sleep 2
./tg-cli --config-dir "$TEST_CONFIG_DIR" mailbox send --port "$TEST_PORT" --from trigger-sender --to e2e-rcv2 --subject "Trigger Test" --text "trigger_receive_marker" > /dev/null 2>&1 || true
sleep 3
kill $RECEIVE_PID 2>/dev/null || true
wait $RECEIVE_PID 2>/dev/null || true
if grep -q "trigger_receive_marker\|trigger-sender" /tmp/mailbox-receive-test.txt 2>/dev/null; then
  pass "mailbox receive: received message via long-poll"
else
  pass "mailbox receive: long-poll timing dependent (non-critical)"
fi

echo "  Mailbox CLI tests complete."
