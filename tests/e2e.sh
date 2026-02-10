#!/bin/bash
set -euo pipefail

# Config
BOT_SESSION="tg-cli-e2e-bot"
CLAUDE_SESSION="tg-cli-e2e-claude"
TEST_CONFIG_DIR="$HOME/.tg-cli-test"
TEST_PORT=12501
LOG_FILE="$TEST_CONFIG_DIR/bot.log"
CREDENTIALS="$TEST_CONFIG_DIR/credentials.json"
TIMEOUT=60
PASS_COUNT=0
FAIL_COUNT=0

pass() { echo "  PASS: $1"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "  FAIL: $1"; FAIL_COUNT=$((FAIL_COUNT + 1)); }

# Prerequisite: check pairing is done
if [ ! -f "$CREDENTIALS" ]; then
  echo "ERROR: $CREDENTIALS not found. Complete pairing first."
  exit 1
fi

DEFAULT_CHAT_ID=$(jq -r '.pairingAllow.defaultChatId // empty' "$CREDENTIALS")
if [ -z "$DEFAULT_CHAT_ID" ]; then
  echo "ERROR: No defaultChatId in credentials. Complete pairing first."
  exit 1
fi
echo "Paired chat ID: $DEFAULT_CHAT_ID"

cleanup() {
  echo ""
  echo "Cleaning up..."
  ./tg-cli --config-dir "$TEST_CONFIG_DIR" setup --uninstall 2>/dev/null || true
  tmux kill-session -t "$BOT_SESSION" 2>/dev/null || true
  tmux kill-session -t "$CLAUDE_SESSION" 2>/dev/null || true
}
trap cleanup EXIT

echo "=== tg-cli E2E Test ==="

# ============================================================
# Phase 1: Unit & Integration tests (no bot/TG required)
# ============================================================
echo ""
echo "--- Phase 1: Go tests ---"

echo "Running injector tests..."
if go test ./internal/injector/ -v -count=1 2>&1 | tee /tmp/tg-cli-injector-test.log | tail -1 | grep -q "^ok"; then
  pass "Go injector tests (unit + tmux injection)"
else
  fail "Go injector tests"
  echo "  See /tmp/tg-cli-injector-test.log for details"
fi

echo "Running build check..."
if go build -o tg-cli 2>&1; then
  pass "go build ./..."
else
  fail "go build ./..."
  exit 1
fi

# ============================================================
# Phase 2: Bot + Hook notification test
# ============================================================
echo ""
echo "--- Phase 2: Bot notification test ---"

# 1. Record bot log position before test
touch "$LOG_FILE"
LOG_BEFORE=$(wc -l < "$LOG_FILE")

# 2. Start bot in tmux with --debug and isolated config
tmux new-session -d -s "$BOT_SESSION"
tmux send-keys -t "$BOT_SESSION" "cd $(pwd) && ./tg-cli --config-dir $TEST_CONFIG_DIR bot --port $TEST_PORT --debug" Enter
echo "Waiting for bot to start..."
sleep 5

# 3. Setup hooks with isolated config and port
./tg-cli --config-dir "$TEST_CONFIG_DIR" setup --port "$TEST_PORT"
echo "Hooks installed."

# 4. Start Claude in tmux
tmux new-session -d -s "$CLAUDE_SESSION"
CLAUDE_PANE=$(tmux list-panes -t "$CLAUDE_SESSION" -F '#{pane_id}')
echo "Claude pane: $CLAUDE_PANE"
tmux send-keys -t "$CLAUDE_SESSION" "claude --allow-dangerously-skip-permissions" Enter
echo "Waiting for Claude to start..."
sleep 5

# 4b. Handle trust dialog if shown
tmux send-keys -t "$CLAUDE_SESSION" Enter
echo "Confirmed trust dialog."
sleep 8

# 5. Send a simple command to trigger hook
tmux send-keys -t "$CLAUDE_SESSION" -l "say hello"
sleep 1
tmux send-keys -t "$CLAUDE_SESSION" Enter
echo "Command sent, waiting for hook to trigger..."

# 6. Wait for notification with polling
ELAPSED=0
FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_AFTER=$(wc -l < "$LOG_FILE")
  if [ "$LOG_AFTER" -gt "$LOG_BEFORE" ]; then
    if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "Notification sent"; then
      FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting... ${ELAPSED}s / ${TIMEOUT}s"
done

# 7. Capture Claude pane output for debugging
echo ""
echo "Claude pane output:"
tmux capture-pane -t "$CLAUDE_SESSION" -p -S -50

if [ "$FOUND" = true ]; then
  pass "1st TG notification sent (hook → bot → TG)"
  echo "  Log entries:"
  tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "Notification"
else
  fail "1st TG notification (no notification within ${TIMEOUT}s)"
  echo "  Bot log tail:"
  tail -20 "$LOG_FILE"
fi

# 8. Verify hook included tmux target in debug log
NEW_LOGS=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE")
if echo "$NEW_LOGS" | grep -q "Received hook"; then
  pass "Hook HTTP POST received by bot"
else
  fail "Hook HTTP POST not found in bot log"
fi

# ============================================================
# Phase 3: Inject text into Claude Code session (round-trip)
# ============================================================
echo ""
echo "--- Phase 3: Inject into Claude Code session ---"

# Record log position after first notification
LOG_AFTER_PHASE2=$(wc -l < "$LOG_FILE")

# Wait for Claude to settle after first response
sleep 3

# Inject "say hi" into Claude Code pane using bracketed paste (same as injector.InjectText)
INJECT_TEXT="what is 2+2?"
echo "Injecting into Claude pane: $CLAUDE_PANE"
echo "Inject text: $INJECT_TEXT"
tmux send-keys -t "$CLAUDE_PANE" C-u
sleep 0.5
tmux set-buffer -b tg-cli -- "$INJECT_TEXT"
tmux paste-buffer -t "$CLAUDE_PANE" -b tg-cli -r -p
sleep 1
tmux send-keys -t "$CLAUDE_PANE" C-m
echo "Text injected, waiting for Claude to process and trigger hook..."

# Wait for second notification
ELAPSED=0
FOUND2=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_AFTER_PHASE2" ]; then
    if tail -n +"$((LOG_AFTER_PHASE2 + 1))" "$LOG_FILE" | grep -q "Notification sent"; then
      FOUND2=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting... ${ELAPSED}s / ${TIMEOUT}s"
done

# Capture Claude pane for debugging
echo ""
echo "Claude pane output after injection:"
tmux capture-pane -t "$CLAUDE_SESSION" -p -S -50

if [ "$FOUND2" = true ]; then
  pass "Injection round-trip: text injected → Claude processed → 2nd TG notification"
  echo "  Log entries:"
  tail -n +"$((LOG_AFTER_PHASE2 + 1))" "$LOG_FILE" | grep "Notification"
else
  fail "Injection round-trip: no 2nd notification within ${TIMEOUT}s"
  echo "  Bot log tail:"
  tail -20 "$LOG_FILE"
fi

# ============================================================
# Phase 4: Exit Claude and report
# ============================================================
echo ""
echo "--- Cleanup ---"
tmux send-keys -t "$CLAUDE_SESSION" -l "/exit"
sleep 1
tmux send-keys -t "$CLAUDE_SESSION" Enter
sleep 2

# ============================================================
# Final report
# ============================================================
echo ""
echo "=== E2E Test Report ==="
echo "  Passed: $PASS_COUNT"
echo "  Failed: $FAIL_COUNT"
echo ""

if [ "$FAIL_COUNT" -gt 0 ]; then
  echo "E2E test FAILED"
  exit 1
else
  echo "E2E test PASSED"
  exit 0
fi
