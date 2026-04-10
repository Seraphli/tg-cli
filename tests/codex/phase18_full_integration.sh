#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Codex full integration test (lifecycle: inject -> response -> exit) ---"

# session new launches config.claudeCommand (CC), not Codex.
# Skip until codexCommand config field is added.
echo "  SKIP: session new launches CC, not Codex (needs codexCommand config)"
exit 0

ensure_infrastructure

CODEX_SESSION="e2e-codex-integ"
CODEX_AGENT="e2e-codex-agent"

cleanup_codex_integ() {
  $TMUX_TEST kill-session -t "=$CODEX_SESSION" 2>/dev/null || true
}
trap cleanup_codex_integ EXIT

LOG_BEFORE_PHASE=$(wc -l < "$LOG_FILE")

# Step 1: Create Codex session via CLI
echo "  [codex/integ] Creating Codex session: $CODEX_AGENT..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" session new \
  --session "$CODEX_SESSION" --workdir "$(pwd)" --name "$CODEX_AGENT" --port "$TEST_PORT" > /dev/null 2>&1
pane_log "[codex/integ] after session new"

# Wait for session to appear in session list
ELAPSED=0
SESSION_FOUND=false
while [ $ELAPSED -lt 60 ]; do
  LIST=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session list --port "$TEST_PORT" 2>&1) || true
  if echo "$LIST" | grep -q "$CODEX_AGENT"; then
    SESSION_FOUND=true
    break
  fi
  sleep 3
  ELAPSED=$((ELAPSED + 3))
done
if [ "$SESSION_FOUND" = true ]; then
  pass "Codex integration: session new created $CODEX_AGENT"
else
  fail "Codex integration: session $CODEX_AGENT not found in session list after 60s"
  exit 1
fi

# Step 2: Send a simple task to Codex and wait for Stop
LOG_BEFORE_SEND=$(wc -l < "$LOG_FILE")
echo "  [codex/integ] Sending simple task to $CODEX_AGENT..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send \
  --name "$CODEX_AGENT" --port "$TEST_PORT" --from e2e-test \
  --text "Say exactly: codex_integration_ok" > /dev/null 2>&1
pane_log "[codex/integ] after session send"

# Step 3: Wait for Stop notification from Codex
ELAPSED=0
STOP_FOUND=false
while [ $ELAPSED -lt 120 ]; do
  if tail -n +"$((LOG_BEFORE_SEND + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
    STOP_FOUND=true
    break
  fi
  sleep 3
  ELAPSED=$((ELAPSED + 3))
  echo "  Waiting for Codex Stop... ${ELAPSED}s / 120s"
done
if [ "$STOP_FOUND" = true ]; then
  pass "Codex integration: Stop notification received after task"
else
  fail "Codex integration: Stop notification not received within 120s"
  pane_log "[codex/integ] stop timeout"
  exit 1
fi

# Step 4: Session watch test
WATCH_FILE="/tmp/e2e-codex-watch-$$.txt"
echo "  [codex/integ] Starting session watch in background..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" session watch \
  --name "$CODEX_AGENT" --port "$TEST_PORT" > "$WATCH_FILE" 2>&1 &
WATCH_PID=$!

sleep 2
echo "  [codex/integ] Sending simple task to trigger Stop for watch..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send \
  --name "$CODEX_AGENT" --port "$TEST_PORT" --from e2e-test \
  --text "Say exactly: watch_test_ok" > /dev/null 2>&1

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
  pass "Codex integration: session watch returned Stop event"
else
  fail "Codex integration: session watch did not return Stop event"
  echo "  watch file: $(cat "$WATCH_FILE" 2>/dev/null || echo '(empty)')"
  kill "$WATCH_PID" 2>/dev/null || true
fi
rm -f "$WATCH_FILE"

# Step 5: Session exit
LOG_BEFORE_EXIT=$(wc -l < "$LOG_FILE")
echo "  [codex/integ] Exiting session $CODEX_AGENT..."
./tg-cli --config-dir "$TEST_CONFIG_DIR" session exit \
  --name "$CODEX_AGENT" --port "$TEST_PORT" > /dev/null 2>&1
pane_log "[codex/integ] after session exit"

# Step 6: Wait for SessionEnd kill-pane
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
  pass "Codex integration: SessionEnd kill-pane executed"
else
  fail "Codex integration: SessionEnd kill-pane not found in bot log within 60s"
fi

# Step 7: Verify tmux session is cleaned up
sleep 2
if $TMUX_TEST has-session -t "$CODEX_SESSION" 2>/dev/null; then
  fail "Codex integration: tmux session $CODEX_SESSION still exists after exit"
else
  pass "Codex integration: tmux session $CODEX_SESSION cleaned up"
fi

echo "  [codex/integ] Full integration test complete."
