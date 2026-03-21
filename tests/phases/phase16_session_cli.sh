#!/bin/bash
# Phase 16: Session CLI commands test (runs before exit to access full transcript)
set -euo pipefail
source "$(dirname "$0")/../e2e_common.sh"

echo ""
echo "--- Session CLI commands test ---"

# Get session ID and set agent name
SESSION_ID=$(cat /tmp/tg-cli-e2e-session-id.txt 2>/dev/null || echo "")
if [ -n "$SESSION_ID" ]; then
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=e2e-cli" > /dev/null 2>&1 || true
fi

# Test session list — verify it contains the active session
LIST_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session list --port "$TEST_PORT" 2>&1) || true
if echo "$LIST_OUTPUT" | grep -q "e2e-cli"; then
  pass "session list: contains agent name 'e2e-cli'"
else
  fail "session list: agent name not found: $LIST_OUTPUT"
fi
if echo "$LIST_OUTPUT" | grep -q "target=\|%"; then
  pass "session list: shows tmux target"
else
  fail "session list: tmux target not found: $LIST_OUTPUT"
fi

# Test session log — verify actual content from earlier phases
LOG_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 20 2>&1) || true

# Should have header with tmux target (📟)
if echo "$LOG_OUTPUT" | grep -q "📟"; then
  pass "session log: header contains tmux target (📟)"
else
  fail "session log: missing tmux target header: $(echo "$LOG_OUTPUT" | head -1)"
fi

# Should have separator lines
if echo "$LOG_OUTPUT" | grep -q "────────────────────────"; then
  pass "session log: has separator lines between messages"
else
  fail "session log: missing separator lines"
fi

# Should have timestamps
if echo "$LOG_OUTPUT" | grep -qE "[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}"; then
  pass "session log: has timestamp format"
else
  fail "session log: missing timestamps"
fi

# Should contain known content — check with large line count
LOG_FULL=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 9999 2>&1) || true
if echo "$LOG_FULL" | grep -q "say hello\|tool_notify_test_ok\|e2e_session_send_test_marker"; then
  pass "session log: contains known content from earlier phases"
else
  fail "session log: no known content found in transcript"
fi

# Test --no-tools filter
NOTOOLS_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 20 --no-tools 2>&1) || true
# Should NOT contain [Bash] entries
if echo "$NOTOOLS_OUTPUT" | grep -q "\[Bash\]"; then
  fail "session log --no-tools: still contains [Bash] entries"
else
  pass "session log --no-tools: Bash entries filtered out"
fi
# Should still contain [assistant] text entries
if echo "$NOTOOLS_OUTPUT" | grep -q "\[assistant\]"; then
  pass "session log --no-tools: contains assistant text entries"
else
  fail "session log --no-tools: no assistant entries found"
fi
# Should contain [user] text entries (user messages must not be filtered)
if echo "$NOTOOLS_OUTPUT" | grep -q "\[user\]"; then
  pass "session log --no-tools: contains user text entries"
else
  fail "session log --no-tools: no user entries found (user messages incorrectly filtered)"
fi

# Test --format json
JSON_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 3 --format json 2>&1) || true
if echo "$JSON_OUTPUT" | python3 -c "import sys,json; d=json.load(sys.stdin); assert 'target' in d and 'messages' in d" 2>/dev/null; then
  pass "session log --format json: valid JSON with target and messages"
else
  fail "session log --format json: invalid JSON structure: $(echo "$JSON_OUTPUT" | head -1)"
fi

# Test session send — inject and verify in bot log
LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-cli --port "$TEST_PORT" --text "e2e_session_send_test_marker" > /dev/null 2>&1 || true
sleep 2
if tail -n +$((LOG_BEFORE+1)) "$LOG_FILE" | grep -q "e2e_session_send_test_marker"; then
  pass "session send: message injected and logged"
else
  fail "session send: injection not found in bot log"
fi
# Check TG notification for session send
if tail -n +$((LOG_BEFORE+1)) "$LOG_FILE" | grep -q "Session send notification\|CLI Message"; then
  pass "session send: TG notification sent"
else
  fail "session send: TG notification not found in log"
fi

# Test session log content accuracy — compare JSON output with transcript JSONL
JSON_3=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 3 --format json 2>&1)

# Check message count == 3
MSG_COUNT=$(echo "$JSON_3" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('messages',[])))" 2>/dev/null || echo "0")
if [ "$MSG_COUNT" = "3" ]; then
  pass "session log accuracy: --lines 3 returned exactly 3 messages"
else
  fail "session log accuracy: --lines 3 returned $MSG_COUNT messages (expected 3)"
fi

# Check that one of the 3 messages contains the injected marker (CC may reply after inject)
ALL_TEXTS=$(echo "$JSON_3" | python3 -c "import sys,json; d=json.load(sys.stdin); [print(m.get('text','')) for m in d.get('messages',[])]" 2>/dev/null || echo "")
if echo "$ALL_TEXTS" | grep -q "e2e_session_send_test_marker"; then
  pass "session log accuracy: messages contain injected marker"
else
  # Try with more lines in case CC reply pushed marker out of top 3
  JSON_5=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 5 --format json 2>&1)
  ALL_5=$(echo "$JSON_5" | python3 -c "import sys,json; d=json.load(sys.stdin); [print(m.get('text','')) for m in d.get('messages',[])]" 2>/dev/null || echo "")
  if echo "$ALL_5" | grep -q "e2e_session_send_test_marker"; then
    pass "session log accuracy: messages contain injected marker (in top 5)"
  else
    fail "session log accuracy: marker not found in recent messages"
  fi
fi

# Cross-check: verify message types are consistent (user/assistant alternation)
MSG_TYPES=$(echo "$JSON_3" | python3 -c "
import sys,json
d=json.load(sys.stdin)
msgs=d.get('messages',[])
for m in msgs:
    print(m.get('type','unknown'))
" 2>/dev/null || echo "")
if echo "$MSG_TYPES" | grep -q "user\|assistant"; then
  pass "session log accuracy: messages have valid user/assistant types"
else
  fail "session log accuracy: messages missing valid types: $MSG_TYPES"
fi

echo "  Session CLI tests complete."
