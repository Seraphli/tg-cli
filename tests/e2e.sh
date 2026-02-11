#!/bin/bash
set -euo pipefail

# Config
BOT_SESSION="tg-cli-e2e-bot"
CLAUDE_SESSION="tg-cli-e2e-claude"
TEST_CONFIG_DIR="$HOME/.tg-cli-test"
TEST_SETTINGS="$TEST_CONFIG_DIR/claude-settings.json"
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
  rm -f "$TEST_SETTINGS"
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
./tg-cli --config-dir "$TEST_CONFIG_DIR" setup --port "$TEST_PORT" --settings "$TEST_SETTINGS"
echo "Hooks installed."

# 4. Start Claude in tmux
tmux new-session -d -s "$CLAUDE_SESSION"
CLAUDE_PANE=$(tmux list-panes -t "$CLAUDE_SESSION" -F '#{pane_id}')
echo "Claude pane: $CLAUDE_PANE"
tmux send-keys -t "$CLAUDE_SESSION" "claude --model haiku --allow-dangerously-skip-permissions --setting-sources local --settings $TEST_SETTINGS" Enter
echo "Waiting for Claude to start..."
sleep 5

# 4b. Handle trust dialog if shown
tmux send-keys -t "$CLAUDE_SESSION" Enter
echo "Confirmed trust dialog."
sleep 8

# 4c. Check SessionStart notification
LOG_AFTER_START=$(wc -l < "$LOG_FILE")
if [ "$LOG_AFTER_START" -gt "$LOG_BEFORE" ]; then
  if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "SessionStart"; then
    pass "SessionStart hook triggered and logged"
  else
    fail "SessionStart hook not found in bot log"
  fi
else
  fail "No new log entries after Claude start"
fi

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
# Phase 3: Long message pagination test (real Claude output)
# ============================================================
echo ""
echo "--- Phase 3: Long message pagination test ---"

# Record log position before injecting long prompt
LOG_BEFORE_PAGE=$(wc -l < "$LOG_FILE")

# Wait for Claude to settle after Phase 2
echo "Waiting for Claude to settle..."
sleep 3

# Inject a long-output prompt to trigger pagination
LONG_PROMPT="list the numbers from 1 to 100, each on its own line, in the format 'Number NNN: test line for pagination verification'"
echo "Injecting long-output prompt into Claude pane: $CLAUDE_PANE"
echo "Prompt: ${LONG_PROMPT:0:80}..."
tmux send-keys -t "$CLAUDE_PANE" C-u
sleep 0.5
tmux set-buffer -b tg-cli -- "$LONG_PROMPT"
tmux paste-buffer -t "$CLAUDE_PANE" -b tg-cli -r -p
sleep 1
tmux send-keys -t "$CLAUDE_PANE" C-m
echo "Long prompt injected, waiting for Claude to respond and trigger pagination..."

# Wait for bot log to contain multi-page notification indicator
ELAPSED=0
PAGINATION_FOUND=false
MSG_ID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PAGE" ]; then
    NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
    if echo "$NEW_PAGE_LOGS" | grep -qE "pages, msg_id="; then
      PAGINATION_FOUND=true
      MSG_ID=$(echo "$NEW_PAGE_LOGS" | grep -oP 'msg_id=\K[0-9]+' | head -1)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for pagination... ${ELAPSED}s / ${TIMEOUT}s"
done

# Capture Claude pane for debugging
echo ""
echo "Claude pane output after long prompt:"
tmux capture-pane -t "$CLAUDE_SESSION" -p -S -100

if [ "$PAGINATION_FOUND" = true ]; then
  pass "Long message triggered pagination (real Claude output)"
  echo "  Log excerpt:"
  tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE" | grep -E "pages|Notification" | head -5
else
  # Check if Claude sent any notification at all
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PAGE" ]; then
    NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
    if echo "$NEW_PAGE_LOGS" | grep -q "Notification sent"; then
      fail "Long message did not trigger pagination (Claude output too short, no multi-page indicator)"
      echo "  This means Claude's response was under the 4096-char limit."
      echo "  Skipping page turn test."
    else
      fail "No notification sent after long prompt (Claude may not have responded)"
      echo "  Bot log tail:"
      tail -20 "$LOG_FILE"
    fi
  else
    fail "No bot activity after long prompt injection"
    echo "  Bot log tail:"
    tail -20 "$LOG_FILE"
  fi
fi

# Page turn test (only if pagination was triggered)
if [ "$PAGINATION_FOUND" = true ] && [ -n "$MSG_ID" ]; then
  echo ""
  echo "Testing page turn callback..."
  CB_RESP=$(curl -s -w "\n%{http_code}" \
    "http://127.0.0.1:$TEST_PORT/callback?msg_id=$MSG_ID&page=2")
  CB_CODE=$(echo "$CB_RESP" | tail -1)
  if [ "$CB_CODE" = "200" ]; then
    pass "Page turn simulation via /callback returned 200"
  else
    fail "Page turn simulation via /callback returned $CB_CODE"
    echo "  Response: $(echo "$CB_RESP" | head -1)"
  fi

  # Verify bot logged the page turn
  sleep 1
  if tail -10 "$LOG_FILE" | grep -q "Callback page turn"; then
    pass "Bot logged callback page turn"
  else
    fail "Bot did not log callback page turn"
  fi
elif [ "$PAGINATION_FOUND" = false ]; then
  echo "  Skipping page turn test (pagination was not triggered)"
fi

# ============================================================
# Phase 4: PermissionRequest real test
# ============================================================
echo ""
echo "--- Phase 4: PermissionRequest test ---"

# Record log position
LOG_BEFORE_PERM=$(wc -l < "$LOG_FILE")

# Send command that triggers Bash permission (file write to ensure permission dialog)
tmux send-keys -t "$CLAUDE_SESSION" -l "run this bash command: echo perm_test_ok > /tmp/tg-cli-perm-test.txt"
sleep 1
tmux send-keys -t "$CLAUDE_SESSION" Enter

echo "Claude pane:"
tmux capture-pane -t "$CLAUDE_SESSION" -p -S -50

# Wait for permission request in bot log
ELAPSED=0
PERM_FOUND=false
PERM_MSG_ID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PERM" ]; then
    if tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep -q "Permission request sent"; then
      PERM_FOUND=true
      PERM_MSG_ID=$(tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep -oP 'msg_id=\K[0-9]+' | head -1)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

echo "Claude pane:"
tmux capture-pane -t "$CLAUDE_SESSION" -p -S -50

if [ "$PERM_FOUND" = true ] && [ -n "$PERM_MSG_ID" ]; then
  pass "PermissionRequest TG notification sent (msg_id=$PERM_MSG_ID)"

  # Approve via API endpoint
  DECIDE_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/permission/decide?msg_id=$PERM_MSG_ID&decision=allow")
  if echo "$DECIDE_RESP" | grep -q "allow"; then
    pass "Permission approved via /permission/decide API"
  else
    fail "Permission decide API returned unexpected: $DECIDE_RESP"
  fi

  echo "Claude pane:"
  tmux capture-pane -t "$CLAUDE_SESSION" -p -S -50

  # Wait for Stop notification (Claude completes after permission approved)
  sleep 10
  LOG_AFTER_PERM=$(wc -l < "$LOG_FILE")
  if tail -n +"$((LOG_BEFORE_PERM + 1))" "$LOG_FILE" | grep -q "Permission resolved"; then
    pass "Permission resolved and logged"
  else
    fail "Permission resolution not found in log"
  fi
else
  fail "PermissionRequest not triggered within ${TIMEOUT}s"
fi

# ============================================================
# Phase 5: AskUserQuestion real test
# ============================================================
echo ""
echo "--- Phase 5: AskUserQuestion test ---"

LOG_BEFORE_AQ=$(wc -l < "$LOG_FILE")

# Send prompt that should trigger AskUserQuestion
tmux send-keys -t "$CLAUDE_SESSION" -l "I need you to ask me a question with AskUserQuestion tool. Ask me: which approach should we use? Options: Approach A, Approach B"
sleep 1
tmux send-keys -t "$CLAUDE_SESSION" Enter

echo "Claude pane:"
tmux capture-pane -t "$CLAUDE_SESSION" -p -S -50

# Wait for AskUserQuestion notification
ELAPSED=0
AQ_FOUND=false
AQ_MSG_ID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_AQ" ]; then
    NEW_LOGS=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE")
    if echo "$NEW_LOGS" | grep -q "AskUserQuestion sent"; then
      AQ_FOUND=true
      AQ_MSG_ID=$(echo "$NEW_LOGS" | grep -oP 'AskUserQuestion sent.*msg_id=\K[0-9]+' | head -1)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

echo "Claude pane:"
tmux capture-pane -t "$CLAUDE_SESSION" -p -S -50

if [ "$AQ_FOUND" = true ] && [ -n "$AQ_MSG_ID" ]; then
  pass "AskUserQuestion TG notification sent (msg_id=$AQ_MSG_ID)"

  # Select option 1 (Approach B) via API
  SELECT_RESP=$(curl -s -w "\n%{http_code}" "http://127.0.0.1:$TEST_PORT/tool/respond?msg_id=$AQ_MSG_ID&tool=AskUserQuestion&option=1")
  SELECT_CODE=$(echo "$SELECT_RESP" | tail -1)
  if [ "$SELECT_CODE" = "200" ]; then
    pass "AskUserQuestion option selected via /tool/respond API"
  else
    fail "AskUserQuestion select API returned $SELECT_CODE"
  fi

  echo "Claude pane:"
  tmux capture-pane -t "$CLAUDE_SESSION" -p -S -50

  # Verify bot logged the selection
  sleep 2
  if tail -20 "$LOG_FILE" | grep -q "AskUserQuestion option selected"; then
    pass "AskUserQuestion option selection logged"
  else
    fail "AskUserQuestion option selection not found in log"
  fi
else
  fail "AskUserQuestion not triggered within ${TIMEOUT}s"
fi

# ============================================================
# Phase 6: Exit Claude and report
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
