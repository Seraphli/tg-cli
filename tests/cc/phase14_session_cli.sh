#!/bin/bash
# Phase 14: Session CLI commands + session log transcript tests (runs before exit to access full transcript)
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Session CLI commands test ---"

# Get session ID from API (always use current session, not stale file)
SESSION_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
pane = sys.argv[1]
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane + "@"):
        print(s.get("id", ""))
        sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
if [ -n "$SESSION_ID" ]; then
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=e2e-cli" > /dev/null 2>&1 || true
  echo "  Named session $SESSION_ID as e2e-cli"
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

# Test session send — inject and verify in bot log (API-based, works for both CC and Codex)
LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-cli --port "$TEST_PORT" --from e2e-test --text "e2e_session_send_test_marker" > /dev/null 2>&1 || true
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

echo "  Session CLI tests complete."

echo ""
echo "--- Session log transcript tests ---"

# Test session log — verify actual content from earlier phases
wait_for_idle $TIMEOUT
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

# Test --no-tools filter
NOTOOLS_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 20 --no-tools 2>&1) || true
if echo "$NOTOOLS_OUTPUT" | grep -q "\[Bash\]"; then
  fail "session log --no-tools: still contains [Bash] entries"
else
  pass "session log --no-tools: Bash entries filtered out"
fi
if echo "$NOTOOLS_OUTPUT" | grep -q "\[assistant\]"; then
  pass "session log --no-tools: contains assistant text entries"
else
  fail "session log --no-tools: no assistant entries found"
fi
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

# Verify session log contains known content (after injecting marker)
LOG_FULL=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 9999 2>&1) || true
if echo "$LOG_FULL" | grep -q "e2e_session_send_test_marker"; then
  pass "session log: contains known content from transcript"
else
  fail "session log: no known content found in transcript"
fi

# Test session log content accuracy — compare JSON output with transcript JSONL
JSON_3=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 3 --format json 2>&1)

MSG_COUNT=$(echo "$JSON_3" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('messages',[])))" 2>/dev/null || echo "0")
if [ "$MSG_COUNT" = "3" ]; then
  pass "session log accuracy: --lines 3 returned exactly 3 messages"
else
  fail "session log accuracy: --lines 3 returned $MSG_COUNT messages (expected 3)"
fi

ALL_TEXTS=$(echo "$JSON_3" | python3 -c "import sys,json; d=json.load(sys.stdin); [print(m.get('text','')) for m in d.get('messages',[])]" 2>/dev/null || echo "")
if echo "$ALL_TEXTS" | grep -q "e2e_session_send_test_marker"; then
  pass "session log accuracy: messages contain injected marker"
else
  JSON_5=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 5 --format json 2>&1)
  ALL_5=$(echo "$JSON_5" | python3 -c "import sys,json; d=json.load(sys.stdin); [print(m.get('text','')) for m in d.get('messages',[])]" 2>/dev/null || echo "")
  if echo "$ALL_5" | grep -q "e2e_session_send_test_marker"; then
    pass "session log accuracy: messages contain injected marker (in top 5)"
  else
    fail "session log accuracy: marker not found in recent messages"
  fi
fi

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

echo "  Session log tests complete."
