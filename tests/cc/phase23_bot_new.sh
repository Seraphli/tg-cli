#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- /bot_new session launch test (HTTP API) ---"

ensure_infrastructure

# Get bot pane ID for pane captures
BOT_PANE=$($TMUX_TEST list-panes -t "$BOT_SESSION" -F '#{pane_id}')

# Cleanup: destroy the tmux session created by /bot_new (on test server)
cleanup_bot_new() {
  echo "  [bot_new] cleanup: killing tg-cli tmux session..."
  $TMUX_TEST kill-session -t "=tg-cli" 2>/dev/null || true
}
trap cleanup_bot_new EXIT

LOG_BEFORE_PHASE=$(wc -l < "$LOG_FILE")

# Step 1: Trigger /bot_new via HTTP API (no session/workdir => interactive flow)
pane_log "[bot_new] BEFORE step 1: trigger /bot_new HTTP API" "$BOT_PANE"
echo "  Triggering /bot_new via HTTP API..."
HTTP_RESP=$(curl -s "http://127.0.0.1:${TEST_PORT}/bot_new")
echo "  HTTP response: $HTTP_RESP"
HTTP_OK=$(echo "$HTTP_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('ok',''))" 2>/dev/null || echo "false")
if [ "$HTTP_OK" != "True" ]; then
  fail "/bot_new HTTP API call failed"
  exit 1
fi
pane_log "[bot_new] AFTER step 1: trigger /bot_new HTTP API" "$BOT_PANE"

# Extract UUID from /bot_new response
LAUNCH_UUID=$(echo "$HTTP_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('uuid',''))" 2>/dev/null || echo "")
echo "  Launch UUID: $LAUNCH_UUID"
if [ -z "$LAUNCH_UUID" ]; then
  fail "Could not extract UUID from /bot_new response"
  exit 1
fi

# Step 2: Wait for askSessionName log
pane_log "[bot_new] BEFORE step 2: wait for askSessionName" "$BOT_PANE"
echo "  Waiting for askSessionName log..."
ELAPSED=0
SESSION_ASK_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_PHASE + 1))" "$LOG_FILE" | grep "askSessionName: sent msg_id=" > /dev/null 2>&1; then
    SESSION_ASK_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for askSessionName... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$SESSION_ASK_FOUND" = true ]; then
  pass "/bot_new triggered askSessionName flow"
else
  fail "/bot_new did not trigger askSessionName within ${TIMEOUT}s"
  exit 1
fi
pane_log "[bot_new] AFTER step 2: wait for askSessionName" "$BOT_PANE"

# Verify askSessionName has buttons
if tail -n +"$((LOG_BEFORE_PHASE + 1))" "$LOG_FILE" | grep "askSessionName: sent msg_id=.*buttons=" > /dev/null 2>&1; then
  pass "askSessionName notification includes buttons"
else
  fail "askSessionName notification missing buttons"
fi

LOG_BEFORE_SESSION=$(wc -l < "$LOG_FILE")

# Step 3: Select default session name via /bot_new/callback
pane_log "[bot_new] BEFORE step 3: select session_default callback" "$BOT_PANE"
echo "  Selecting default session name via /bot_new/callback..."
CB_RESP=$(curl -s "http://127.0.0.1:${TEST_PORT}/bot_new/callback?uuid=${LAUNCH_UUID}&data=session_default")
echo "  Callback response: $CB_RESP"
CB_OK=$(echo "$CB_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('ok',''))" 2>/dev/null || echo "false")
if [ "$CB_OK" != "True" ]; then
  fail "/bot_new/callback session_default failed"
  exit 1
fi
pane_log "[bot_new] AFTER step 3: select session_default callback" "$BOT_PANE"

# Step 4: Wait for askWorkDir log
pane_log "[bot_new] BEFORE step 4: wait for askWorkDir" "$BOT_PANE"
echo "  Waiting for askWorkDir log..."
ELAPSED=0
WORKDIR_ASK_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_SESSION + 1))" "$LOG_FILE" | grep "askWorkDir: sent msg_id=" > /dev/null 2>&1; then
    WORKDIR_ASK_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for askWorkDir... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$WORKDIR_ASK_FOUND" = true ]; then
  pass "askWorkDir confirmation step reached"
else
  fail "askWorkDir not triggered within ${TIMEOUT}s"
  exit 1
fi
pane_log "[bot_new] AFTER step 4: wait for askWorkDir" "$BOT_PANE"

# Verify askWorkDir has buttons
if tail -n +"$((LOG_BEFORE_SESSION + 1))" "$LOG_FILE" | grep "askWorkDir: sent msg_id=.*buttons=" > /dev/null 2>&1; then
  pass "askWorkDir notification includes buttons"
else
  fail "askWorkDir notification missing buttons"
fi

LOG_BEFORE_WORKDIR=$(wc -l < "$LOG_FILE")

# Step 5: Select current directory via /bot_new/callback (dir_select)
pane_log "[bot_new] BEFORE step 5: select dir_select callback" "$BOT_PANE"
echo "  Selecting current directory via /bot_new/callback dir_select..."
DIR_RESP=$(curl -s "http://127.0.0.1:${TEST_PORT}/bot_new/callback?uuid=${LAUNCH_UUID}&data=dir_select")
echo "  Dir select response: $DIR_RESP"
DIR_OK=$(echo "$DIR_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('ok',''))" 2>/dev/null || echo "false")
if [ "$DIR_OK" != "True" ]; then
  fail "/bot_new/callback dir_select failed"
  exit 1
fi
pane_log "[bot_new] AFTER step 5: select dir_select callback" "$BOT_PANE"

# Step 6: Wait for executeLaunch log
pane_log "[bot_new] BEFORE step 6: wait for executeLaunch" "$BOT_PANE"
echo "  Waiting for executeLaunch log..."
ELAPSED=0
LAUNCH_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_WORKDIR + 1))" "$LOG_FILE" | grep -E "executeLaunch: created (session|window)" > /dev/null 2>&1; then
    LAUNCH_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for executeLaunch... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$LAUNCH_FOUND" = true ]; then
  pass "executeLaunch: tmux session created"
else
  fail "executeLaunch: tmux session not created within ${TIMEOUT}s"
  exit 1
fi
pane_log "[bot_new] AFTER step 6: wait for executeLaunch" "$BOT_PANE"

# Step 7: Verify tmux session exists (on test server)
pane_log "[bot_new] BEFORE step 7: verify tmux session" "$BOT_PANE"
if $TMUX_TEST has-session -t "tg-cli" 2>/dev/null; then
  pass "tmux session 'tg-cli' exists after /bot_new"
else
  fail "tmux session 'tg-cli' not found after executeLaunch"
fi
pane_log "[bot_new] AFTER step 7: verify tmux session" "$BOT_PANE"
NEW_PANE=$($TMUX_TEST list-panes -t "tg-cli" -F '#{pane_id}' 2>/dev/null || echo "")
if [ -n "$NEW_PANE" ]; then
  pane_log "[bot_new] new CC pane after step 7" "$NEW_PANE"
fi

# Step 8: Wait for executeLaunch done log
pane_log "[bot_new] BEFORE step 8: wait for executeLaunch done" "$BOT_PANE"
echo "  Waiting for executeLaunch done log..."
ELAPSED=0
DONE_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_WORKDIR + 1))" "$LOG_FILE" | grep "executeLaunch: done session=tg-cli pane=" > /dev/null 2>&1; then
    DONE_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for executeLaunch done... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$DONE_FOUND" = true ]; then
  pass "executeLaunch done log with pane ID found"
else
  fail "executeLaunch done log with pane ID not found in bot log"
fi
pane_log "[bot_new] AFTER step 8: wait for executeLaunch done" "$BOT_PANE"
NEW_PANE=$($TMUX_TEST list-panes -t "tg-cli" -F '#{pane_id}' 2>/dev/null || echo "")
if [ -n "$NEW_PANE" ]; then
  pane_log "[bot_new] new CC pane after step 8" "$NEW_PANE"
fi

echo "  [bot_new] test complete."
