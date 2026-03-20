#!/bin/bash
# Phase 22: Full integration test — session new + mailbox + session exit + tmux auto-kill
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Full integration test (session lifecycle + mailbox + tmux cleanup) ---"

ensure_infrastructure

INTEG_SESSION="e2e-integ"
INTEG_AGENT="e2e-integ-agent"
RECV_PID=""
RECV_FILE="/tmp/e2e-mailbox-recv-$$.txt"

cleanup_integration() {
  [ -n "$RECV_PID" ] && kill "$RECV_PID" 2>/dev/null || true
  $TMUX_TEST kill-session -t "$INTEG_SESSION" 2>/dev/null || true
  rm -f "$RECV_FILE"
}
trap cleanup_integration EXIT

LOG_BEFORE_PHASE=$(wc -l < "$LOG_FILE")

# Step 1: Create CC session via CLI
echo "  [integ] Creating CC session: $INTEG_AGENT..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" session new \
  --session "$INTEG_SESSION" --workdir /tmp --name "$INTEG_AGENT" --port "$TEST_PORT" > /dev/null 2>&1
pane_log "[integ] after session new"

# Wait for session to appear in session list
echo "  [integ] Waiting for session to register..."
ELAPSED=0
SESSION_FOUND=false
while [ $ELAPSED -lt 60 ]; do
  LIST=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session list --port "$TEST_PORT" 2>&1) || true
  if echo "$LIST" | grep -q "$INTEG_AGENT"; then
    SESSION_FOUND=true
    break
  fi
  sleep 3
  ELAPSED=$((ELAPSED + 3))
done
if [ "$SESSION_FOUND" = true ]; then
  pass "integration: session new created $INTEG_AGENT"
else
  fail "integration: session $INTEG_AGENT not found in session list after 60s"
  exit 1
fi

# Step 2: Start mailbox receive in background
echo "  [integ] Starting mailbox receive in background..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" mailbox receive \
  --name "$CLAUDE_SESSION" --port "$TEST_PORT" > "$RECV_FILE" 2>&1 &
RECV_PID=$!

# Step 3: Send message to CC asking it to send mailbox
echo "  [integ] Sending message to $INTEG_AGENT..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send \
  --name "$INTEG_AGENT" --port "$TEST_PORT" \
  --text "Run this exact command: tg-cli mailbox send --from $INTEG_AGENT --to $CLAUDE_SESSION --subject 'E2E Integration' --text 'e2e_integration_marker_22' --port $TEST_PORT" > /dev/null 2>&1
pane_log "[integ] after session send"

# Step 4: Wait for CC to send the mailbox message
echo "  [integ] Waiting for CC to send mailbox..."
ELAPSED=0
MAIL_SENT=false
while [ $ELAPSED -lt 120 ]; do
  if tail -n +"$((LOG_BEFORE_PHASE + 1))" "$LOG_FILE" | grep "Mailbox send.*$INTEG_AGENT" > /dev/null 2>&1; then
    MAIL_SENT=true
    break
  fi
  sleep 3
  ELAPSED=$((ELAPSED + 3))
done
if [ "$MAIL_SENT" = true ]; then
  pass "integration: CC sent mailbox message (no Permission Request)"
else
  fail "integration: CC did not send mailbox within 120s"
  pane_log "[integ] mailbox send timeout"
  exit 1
fi

# Step 5: Wait for mailbox receive to complete
echo "  [integ] Waiting for mailbox receive to return..."
ELAPSED=0
RECV_DONE=false
while [ $ELAPSED -lt 30 ]; do
  if ! kill -0 "$RECV_PID" 2>/dev/null; then
    RECV_DONE=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done
if [ "$RECV_DONE" = true ] && grep -q "e2e_integration_marker_22" "$RECV_FILE" 2>/dev/null; then
  pass "integration: mailbox receive got message with marker"
else
  fail "integration: mailbox receive did not get expected message"
  echo "  recv file content: $(cat "$RECV_FILE" 2>/dev/null || echo '(empty)')"
fi
RECV_PID=""  # Already exited

# Step 6: Check inbox read status
INBOX=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" mailbox inbox --name "$CLAUDE_SESSION" --port "$TEST_PORT" 2>&1) || true
if echo "$INBOX" | grep "$INTEG_AGENT" | grep -q "^\*"; then
  fail "integration: mailbox inbox still shows unread (*) after receive"
else
  pass "integration: mailbox inbox shows read (no *)"
fi

# Step 6b: Session watch test
WATCH_FILE="/tmp/e2e-watch-$$.txt"
echo "  [integ] Starting session watch in background..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" session watch \
  --name "$INTEG_AGENT" --port "$TEST_PORT" > "$WATCH_FILE" 2>&1 &
WATCH_PID=$!

# Send a simple task to trigger Stop
sleep 2
echo "  [integ] Sending simple task to trigger Stop..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send \
  --name "$INTEG_AGENT" --port "$TEST_PORT" \
  --text "Say exactly: watch_test_ok" > /dev/null 2>&1

# Wait for watch to return
echo "  [integ] Waiting for watch to return..."
ELAPSED=0
WATCH_DONE=false
while [ $ELAPSED -lt 60 ]; do
  if ! kill -0 "$WATCH_PID" 2>/dev/null; then
    WATCH_DONE=true
    break
  fi
  sleep 3
  ELAPSED=$((ELAPSED + 3))
done
if [ "$WATCH_DONE" = true ] && grep -q '"event":"Stop"' "$WATCH_FILE" 2>/dev/null; then
  pass "integration: session watch returned Stop event"
else
  fail "integration: session watch did not return Stop event"
  echo "  watch file content: $(cat "$WATCH_FILE" 2>/dev/null || echo '(empty)')"
  kill "$WATCH_PID" 2>/dev/null || true
fi
rm -f "$WATCH_FILE"

# Step 7: Session exit
LOG_BEFORE_EXIT=$(wc -l < "$LOG_FILE")
echo "  [integ] Exiting session $INTEG_AGENT..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" session exit \
  --name "$INTEG_AGENT" --port "$TEST_PORT" > /dev/null 2>&1
pane_log "[integ] after session exit"

# Step 8: Wait for SessionEnd kill-pane
echo "  [integ] Waiting for SessionEnd kill-pane..."
ELAPSED=0
KILL_FOUND=false
while [ $ELAPSED -lt 60 ]; do
  if tail -n +"$((LOG_BEFORE_EXIT + 1))" "$LOG_FILE" | grep "SessionEnd kill-pane" > /dev/null 2>&1; then
    KILL_FOUND=true
    break
  fi
  sleep 3
  ELAPSED=$((ELAPSED + 3))
done
if [ "$KILL_FOUND" = true ]; then
  pass "integration: SessionEnd kill-pane executed"
else
  fail "integration: SessionEnd kill-pane not found in bot log within 60s"
fi

# Step 9: Verify tmux session is gone
sleep 2
if $TMUX_TEST has-session -t "$INTEG_SESSION" 2>/dev/null; then
  fail "integration: tmux session $INTEG_SESSION still exists after exit"
else
  pass "integration: tmux session $INTEG_SESSION cleaned up"
fi

# Step 10: Verify kill-pane log has the target pane
KILL_LOG=$(tail -n +"$((LOG_BEFORE_EXIT + 1))" "$LOG_FILE" | grep "SessionEnd kill-pane" | tail -1)
if echo "$KILL_LOG" | grep -q "target=%"; then
  pass "integration: SessionEnd kill-pane has pane target"
else
  fail "integration: SessionEnd kill-pane missing pane target: $KILL_LOG"
fi

echo "  [integ] Full integration test complete."
