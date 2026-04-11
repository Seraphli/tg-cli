#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Resume session test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE")

# session_end exited CC. Restart CC in the existing tmux pane.
pane_log "[resume] BEFORE CC restart"
# Wait for shell to be ready after CC exit (sentinel approach)
SENTINEL="SHELL_READY_$(date +%s)"
ELAPSED=0
while [ $ELAPSED -lt 30 ]; do
  $TMUX_TEST send-keys -t "$E2E_SESSION" "echo $SENTINEL"
  sleep 1
  $TMUX_TEST send-keys -t "$E2E_SESSION" Enter
  sleep 2
  PANE_CONTENT=$($TMUX_TEST capture-pane -t "${E2E_PANE%@*}" -p 2>/dev/null || true)
  if echo "$PANE_CONTENT" | grep -q "$SENTINEL"; then
    echo "  Shell is ready (sentinel detected)."
    break
  fi
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for shell readiness... ${ELAPSED}s / 30s"
done
if [ $ELAPSED -ge 30 ]; then
  echo "  FAIL: Shell not ready within 30s after CC exit"
  fail "Shell readiness after CC exit"
  exit 1
fi
$TMUX_TEST send-keys -t "$E2E_SESSION" "BROWSER=none CLAUDE_CONFIG_DIR=$TEST_CLAUDE_CONFIG_DIR claude --model sonnet --allow-dangerously-skip-permissions"
sleep 1
$TMUX_TEST send-keys -t "$E2E_SESSION" Enter

# Wait for CC to show banner or trust dialog
ELAPSED_CC=0
CC_STARTED=false
while [ $ELAPSED_CC -lt 30 ]; do
  sleep 2
  PANE_CONTENT=$($TMUX_TEST capture-pane -t "${E2E_PANE%@*}" -p 2>/dev/null || true)
  if echo "$PANE_CONTENT" | grep -qi "Bypass Permissions"; then
    $TMUX_TEST send-keys -t "$E2E_SESSION" Down
    sleep 1
    $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
    echo "  Bypass Permissions dialog detected, accepted."
    CC_STARTED=true
    break
  fi
  if echo "$PANE_CONTENT" | grep -qi "trust"; then
    $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
    echo "  Trust dialog detected, confirmed."
    CC_STARTED=true
    break
  fi
  if echo "$PANE_CONTENT" | grep -q "Claude Code"; then
    echo "  CC banner detected."
    CC_STARTED=true
    break
  fi
  ELAPSED_CC=$((ELAPSED_CC + 2))
  echo "  Waiting for CC to start... ${ELAPSED_CC}s / 30s"
done
if [ "$CC_STARTED" = false ]; then
  pane_log "[resume] CC failed to start"
  echo "  FAIL: CC did not start within 30s"
  fail "CC startup after restart"
  exit 1
fi
pane_log "[resume] AFTER CC startup detected"

# Wait for SessionStart notification in bot log
ELAPSED=0
while ! tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "SessionStart" > /dev/null 2>&1; do
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  if [ "$ELAPSED" -ge "$TIMEOUT" ]; then
    pane_log "[resume] SessionStart timeout - pane state"
    echo "  FAIL: SessionStart not detected within ${TIMEOUT}s"
    fail "SessionStart after CC restart"
    exit 1
  fi
  echo "  Waiting for SessionStart... ${ELAPSED}s / ${TIMEOUT}s"
done
pass "SessionStart after CC restart"
# Bug 2 regression guard: SessionStart notification header must include 🖥 CLICommand line.
# Before the fix, SessionStart built NotificationData without CLICommand; after the refactor
# it calls sendEventNotification which populates it via GetPaneCLICommand.
sleep 1
SS_DEBUG=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -A 30 "TG message sent \[SessionStart\]" | head -40 || true)
if echo "$SS_DEBUG" | grep -q "🖥"; then
  pass "Bug 2 regression guard: SessionStart header contains 🖥 CLICommand line"
else
  fail "Bug 2: SessionStart header missing 🖥 CLICommand line. Log excerpt: $SS_DEBUG"
fi
if echo "$SS_DEBUG" | grep -qE '📊 Context:'; then
  pass "Bug 2 regression guard: SessionStart header contains 📊 Context line"
else
  echo "  INFO: SessionStart header missing 📊 Context line (may be expected if no context data)"
fi

# Wait for CC to reach idle state (session registered)
pane_log "[resume] AFTER CC restart"
wait_for_idle

# Test /resume/list API — should return at least 1 session from previous phases
LOG_BEFORE_LIST=$(wc -l < "$LOG_FILE")
ENCODED_PANE=$(printf '%s' "$E2E_PANE" | python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read()))")
LIST_RESP=$(curl -s "http://127.0.0.1:${TEST_PORT}/resume/list?target=${ENCODED_PANE}")
echo "  /resume/list response: $LIST_RESP"

SESSION_COUNT=$(echo "$LIST_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('sessions',[])))" 2>/dev/null || echo "0")
if [ "$SESSION_COUNT" -gt 0 ]; then
  pass "/resume/list returned $SESSION_COUNT session(s)"
else
  fail "/resume/list returned 0 sessions"
  exit 1
fi

# Verify resume list log entry
if tail -n +"$((LOG_BEFORE_LIST + 1))" "$LOG_FILE" | grep "Resume list:" > /dev/null 2>&1; then
  pass "Resume list log found"
else
  fail "Resume list log not found"
fi

# Read test session ID saved by bot_hook
TEST_SID=$(cat /tmp/tg-cli-e2e-session-id.txt 2>/dev/null || true)
if [ -n "$TEST_SID" ]; then
  RESUME_SID="$TEST_SID"
  echo "  Using saved test session ID: $RESUME_SID"
else
  # Fallback to first session
  RESUME_SID=$(echo "$LIST_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['sessions'][0]['id'])")
  echo "  WARNING: No saved session ID, falling back to sessions[0]: $RESUME_SID"
fi
echo "  Resuming session ID: $RESUME_SID"

# Test /resume/select API
LOG_BEFORE_SELECT=$(wc -l < "$LOG_FILE")
SELECT_RESP=$(curl -s "http://127.0.0.1:${TEST_PORT}/resume/select?target=${ENCODED_PANE}&session_id=${RESUME_SID}")
echo "  /resume/select response: $SELECT_RESP"

if echo "$SELECT_RESP" | grep '"ok"' > /dev/null 2>&1; then
  pass "/resume/select returned ok"
else
  fail "/resume/select did not return ok"
  exit 1
fi

# Verify resume inject log entry
if tail -n +"$((LOG_BEFORE_SELECT + 1))" "$LOG_FILE" | grep "Resume injected via API" > /dev/null 2>&1; then
  pass "Resume injected log found"
else
  fail "Resume injected log not found"
fi

# Clean up: exit the resumed CC session
pane_log "[resume] BEFORE final /exit"
sleep 5
inject_prompt "/exit"
sleep 5
pane_log "[resume] AFTER final /exit"
