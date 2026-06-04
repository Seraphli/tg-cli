#!/bin/bash
# Standalone test (round 2, boss directive): bot-startup failure diagnostics.
# Verifies that wait_for_bot_ready, on timeout, captures the bot launch command, a startup probe,
# the bot tmux pane, and the current bot.log into a per-run bot-startup-fail-*.log so a bot that
# fails to come up is diagnosable post-hoc.
set -uo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/e2e_common.sh"

# Isolate from any real test run: dedicated config dir, a dynamically-chosen free port (so a stray
# local service can't make wait_for_bot_ready falsely succeed), and a dedicated bot session.
TEST_CONFIG_DIR="/tmp/tg-cli-startup-diag-test"
LOG_FILE="$TEST_CONFIG_DIR/bot.log"
TEST_PORT=$(python3 -c "import socket; s=socket.socket(); s.bind(('127.0.0.1',0)); port=s.getsockname()[1]; s.close(); print(port)")
BOT_SESSION="tg-cli-startup-diag-bot"
rm -rf "$TEST_CONFIG_DIR"
mkdir -p "$TEST_CONFIG_DIR"

echo "--- bot startup diagnostics test (dead port $TEST_PORT) ---"

# Seed a recognizable bot.log line and a fake launch command.
echo "STARTUP_DIAG_LOG_MARKER prior bot output" > "$LOG_FILE"
export _LAST_BOT_LAUNCH_CMD="cd /diag-test && ./tg-cli --config-dir $TEST_CONFIG_DIR bot --port $TEST_PORT --debug"

# Create a bot tmux pane with a recognizable marker so capture-pane has content.
$TMUX_TEST kill-session -t "=$BOT_SESSION" 2>/dev/null || true
$TMUX_TEST new-session -d -s "$BOT_SESSION"
$TMUX_TEST send-keys -t "$BOT_SESSION" "echo STARTUP_DIAG_PANE_MARKER" Enter
sleep 1

# Act: probe a dead port with a short timeout so wait_for_bot_ready times out and writes diagnostics.
rc=0
wait_for_bot_ready 3 || rc=$?

if [ "$rc" -ne 0 ]; then
  pass "startup-diag: wait_for_bot_ready returns non-zero on timeout (rc=$rc)"
else
  fail "startup-diag: wait_for_bot_ready did NOT fail against a dead port (rc=$rc)"
fi

DIAG=$(ls -t "$TEST_CONFIG_DIR"/bot-startup-fail-*.log 2>/dev/null | head -1 || true)
if [ -n "$DIAG" ] && [ -f "$DIAG" ]; then
  pass "startup-diag: diagnostic file created ($DIAG)"
else
  fail "startup-diag: no bot-startup-fail-*.log was created"
fi

check_section() {
  local pat="$1"; local label="$2"
  if grep -q "$pat" "$DIAG" 2>/dev/null; then
    pass "startup-diag: diagnostic contains $label"
  else
    fail "startup-diag: diagnostic MISSING $label (pattern: '$pat')"
  fi
}
check_section "wait_for_bot_ready TIMEOUT" "timeout header"
check_section "last bot launch command" "launch-command section"
check_section "startup probe" "startup-probe section"
check_section "bot tmux pane" "pane section"
check_section "STARTUP_DIAG_PANE_MARKER" "captured pane marker"
check_section "bot.log" "bot.log section"
check_section "STARTUP_DIAG_LOG_MARKER" "captured bot.log content"

# Cleanup
$TMUX_TEST kill-session -t "=$BOT_SESSION" 2>/dev/null || true
rm -rf "$TEST_CONFIG_DIR"
echo "--- bot startup diagnostics test complete ---"
