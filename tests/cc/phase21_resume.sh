#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Resume session test ---"

ensure_infrastructure

# Step A: Start CC, inject a prompt to create history
LOG_BEFORE=$(wc -l < "$LOG_FILE")
start_claude "e2e-cc-21"

pane_log "[resume] BEFORE warmup inject"
inject_prompt "say hello"
wait_for_idle
pane_log "[resume] AFTER warmup inject"

# Save session ID for resume (use /session/list API, not log grep)
ENCODED_PANE_FOR_SID=$(printf '%s' "$E2E_PANE" | python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read()))")
TEST_SID=$(curl -s "http://127.0.0.1:${TEST_PORT}/session/list" | python3 -c "
import sys, json
pane = sys.argv[1]
d = json.load(sys.stdin)
for s in d.get('sessions', []):
    t = s.get('target', '')
    if t == pane or t.startswith(pane + '@') or pane.startswith(t):
        print(s.get('id', ''))
        sys.exit(0)
print('')
" "$E2E_PANE" 2>/dev/null || echo "")
if [ -z "$TEST_SID" ]; then
  fail "No session ID found via /session/list for pane $E2E_PANE"
  exit 1
fi
echo "  Test session ID: $TEST_SID"

# Step B: Exit CC
LOG_BEFORE_EXIT=$(wc -l < "$LOG_FILE")
pane_log "[resume] BEFORE /exit"
$TMUX_TEST send-keys -t "$E2E_SESSION" "/exit"
sleep 1
$TMUX_TEST send-keys -t "$E2E_SESSION" Enter

ELAPSED=0
EXIT_FOUND=false
while [ $ELAPSED -lt 30 ]; do
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  if tail -n +"$((LOG_BEFORE_EXIT + 1))" "$LOG_FILE" | grep -q "SessionEnd" 2>/dev/null; then
    EXIT_FOUND=true
    break
  fi
done
if [ "$EXIT_FOUND" = false ]; then
  fail "SessionEnd not found after /exit"
  exit 1
fi
pass "SessionEnd after /exit"

# Step C: Wait for shell, then restart CC in same tmux session
SENTINEL="SHELL_READY_$(date +%s)"
ELAPSED=0
while [ $ELAPSED -lt 30 ]; do
  $TMUX_TEST send-keys -t "$E2E_SESSION" "echo $SENTINEL"
  sleep 1
  $TMUX_TEST send-keys -t "$E2E_SESSION" Enter
  sleep 2
  PANE_CONTENT=$($TMUX_TEST capture-pane -t "${E2E_PANE%@*}" -p 2>/dev/null || true)
  if echo "$PANE_CONTENT" | grep -q "$SENTINEL" 2>/dev/null; then
    echo "  Shell is ready."
    break
  fi
  ELAPSED=$((ELAPSED + 3))
done

LOG_BEFORE_RESTART=$(wc -l < "$LOG_FILE")
$TMUX_TEST send-keys -t "$E2E_SESSION" \
  "$(cc_launch_cmd --dangerously-skip-permissions)"
sleep 1
$TMUX_TEST send-keys -t "$E2E_SESSION" Enter

# Handle trust/bypass dialog
sleep 5
PANE_CONTENT=$($TMUX_TEST capture-pane -t "${E2E_PANE%@*}" -p -S - 2>/dev/null || true)
set +eo pipefail
echo "$PANE_CONTENT" | grep -q "Bypass Permissions"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  $TMUX_TEST send-keys -t "$E2E_SESSION" Down
  sleep 1
  $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
else
  set +eo pipefail
  echo "$PANE_CONTENT" | grep -q "trust"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    $TMUX_TEST send-keys -t "$E2E_SESSION" C-m
  fi
fi

# Wait for SessionStart after restart
ELAPSED=0
while ! tail -n +"$((LOG_BEFORE_RESTART + 1))" "$LOG_FILE" | grep "SessionStart" > /dev/null 2>&1; do
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  if [ "$ELAPSED" -ge "$TIMEOUT" ]; then
    fail "SessionStart after CC restart"
    exit 1
  fi
done
pass "SessionStart after CC restart"

# Bug2 regression guard: SessionStart must include CLI command line
sleep 1
SS_DEBUG=$(tail -n +"$((LOG_BEFORE_RESTART + 1))" "$LOG_FILE" | grep -A 30 "TG message sent \[SessionStart\]" | head -40 || true)
set +eo pipefail
echo "$SS_DEBUG" | grep -q "🖥"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Bug 2 regression: SessionStart has 🖥 CLICommand"
else
  fail "Bug 2: SessionStart missing 🖥 CLICommand"
fi
set +eo pipefail
echo "$SS_DEBUG" | grep -qE '📊 Context:'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Bug 2 regression: SessionStart has 📊 Context"
else
  echo "  INFO: SessionStart missing 📊 Context (may be expected if no context data)"
fi

wait_for_idle

# Step D: Test /resume/list API
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

if tail -n +"$((LOG_BEFORE_LIST + 1))" "$LOG_FILE" | grep "Resume list:" > /dev/null 2>&1; then
  pass "Resume list log found"
else
  fail "Resume list log not found"
fi

# Step E: Test /resume/select API
RESUME_SID="$TEST_SID"
echo "  Resuming session ID: $RESUME_SID"
LOG_BEFORE_SELECT=$(wc -l < "$LOG_FILE")
SELECT_RESP=$(curl -s "http://127.0.0.1:${TEST_PORT}/resume/select?target=${ENCODED_PANE}&session_id=${RESUME_SID}")
echo "  /resume/select response: $SELECT_RESP"
if echo "$SELECT_RESP" | grep '"ok"' > /dev/null 2>&1; then
  pass "/resume/select returned ok"
else
  fail "/resume/select did not return ok"
  exit 1
fi

if tail -n +"$((LOG_BEFORE_SELECT + 1))" "$LOG_FILE" | grep "Resume injected via API" > /dev/null 2>&1; then
  pass "Resume injected log found"
else
  fail "Resume injected log not found"
fi

wait_for_idle
