#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Codex /bot_new session launch test (HTTP API) ---"

ensure_infrastructure

BOT_PANE=$($TMUX_TEST list-panes -t "$BOT_SESSION" -F '#{pane_id}')

# Cleanup Codex tmux session created by /bot_new
cleanup_bot_new_codex() {
  echo "  [codex/bot_new] cleanup: killing codex tmux session..."
  $TMUX_TEST kill-session -t "=tg-cli" 2>/dev/null || true
}
trap cleanup_bot_new_codex EXIT

LOG_BEFORE_PHASE=$(wc -l < "$LOG_FILE")

# Step 1: Trigger /bot_new via HTTP API
pane_log "[codex/bot_new] BEFORE step 1: trigger /bot_new HTTP API" "$BOT_PANE"
echo "  Triggering /bot_new via HTTP API..."
HTTP_RESP=$(curl -s "http://127.0.0.1:${TEST_PORT}/bot_new")
echo "  HTTP response: $HTTP_RESP"
HTTP_OK=$(echo "$HTTP_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('ok',''))" 2>/dev/null || echo "false")
if [ "$HTTP_OK" != "True" ]; then
  fail "Codex /bot_new HTTP API call failed"
  exit 1
fi
pane_log "[codex/bot_new] AFTER step 1: trigger /bot_new HTTP API" "$BOT_PANE"

# Extract UUID from /bot_new response
LAUNCH_UUID=$(echo "$HTTP_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('uuid',''))" 2>/dev/null || echo "")
echo "  Launch UUID: $LAUNCH_UUID"
if [ -z "$LAUNCH_UUID" ]; then
  fail "Could not extract UUID from /bot_new response"
  exit 1
fi

# Step 2: Wait for askSessionName log
pane_log "[codex/bot_new] BEFORE step 2: wait for askSessionName" "$BOT_PANE"
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
  pass "Codex /bot_new triggered askSessionName flow"
else
  fail "Codex /bot_new did not trigger askSessionName within ${TIMEOUT}s"
  exit 1
fi
pane_log "[codex/bot_new] AFTER step 2: wait for askSessionName" "$BOT_PANE"

LOG_BEFORE_SESSION=$(wc -l < "$LOG_FILE")

# Step 3: Select default session name via /bot_new/callback
echo "  Selecting default session name via /bot_new/callback..."
CB_RESP=$(curl -s "http://127.0.0.1:${TEST_PORT}/bot_new/callback?uuid=${LAUNCH_UUID}&data=session_default")
CB_OK=$(echo "$CB_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('ok',''))" 2>/dev/null || echo "false")
if [ "$CB_OK" != "True" ]; then
  fail "Codex /bot_new/callback session_default failed"
  exit 1
fi
pass "Codex /bot_new session_default callback accepted"

# Step 4: Wait for askWorkDir log
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
  pass "Codex askWorkDir confirmation step reached"
else
  fail "Codex askWorkDir not triggered within ${TIMEOUT}s"
  exit 1
fi

LOG_BEFORE_WORKDIR=$(wc -l < "$LOG_FILE")

# Step 5: Select current directory via /bot_new/callback (dir_select)
echo "  Selecting current directory via /bot_new/callback dir_select..."
DIR_RESP=$(curl -s "http://127.0.0.1:${TEST_PORT}/bot_new/callback?uuid=${LAUNCH_UUID}&data=dir_select")
DIR_OK=$(echo "$DIR_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('ok',''))" 2>/dev/null || echo "false")
if [ "$DIR_OK" != "True" ]; then
  fail "Codex /bot_new/callback dir_select failed"
  exit 1
fi

# Step 6: Wait for executeLaunch log
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
  pass "Codex executeLaunch: tmux session created"
else
  fail "Codex executeLaunch: tmux session not created within ${TIMEOUT}s"
  exit 1
fi

# Step 7: Verify tmux session exists
if $TMUX_TEST has-session -t "tg-cli" 2>/dev/null; then
  pass "Codex tmux session 'tg-cli' exists after /bot_new"
else
  fail "Codex tmux session 'tg-cli' not found after executeLaunch"
fi

# Step 8: Wait for executeLaunch done log
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
  pass "Codex executeLaunch done log with pane ID found"
else
  fail "Codex executeLaunch done log with pane ID not found in bot log"
fi

echo "  [codex/bot_new] test complete."
