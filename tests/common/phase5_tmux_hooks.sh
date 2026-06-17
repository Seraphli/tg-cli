#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Tmux hook registration and triggering test ---"

ensure_infrastructure
pane_log "[tmux_hooks] BEFORE test"

TMUX_CONF_TEST="/tmp/tg-cli-e2e-tmux.conf"
HOOK_TEST_SESSION="tg-cli-hook-test-$$"

# Cleanup any leftover test conf
rm -f "$TMUX_CONF_TEST"

# =============================================
# Test 1: tg-cli install registers tmux hooks on test server
# =============================================

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

INSTALL_OUTPUT=$(echo "" | ./tg-cli --config-dir "$TEST_CONFIG_DIR" install \
  --port "$TEST_PORT" \
  --settings "$TEST_CLAUDE_CONFIG_DIR/settings.json" \
  --tmux-server $TMUX_SERVER_NAME \
  --tmux-conf "$TMUX_CONF_TEST" 2>&1) || true

echo "  DEBUG: INSTALL_OUTPUT (${#INSTALL_OUTPUT} chars): $INSTALL_OUTPUT"
set +eo pipefail
echo "$INSTALL_OUTPUT" | grep -q "tmux hook registered"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "tmux hooks: install registered hooks on test server"
else
  fail "tmux hooks: install did not register hooks - output: $INSTALL_OUTPUT"
fi

# Verify tmux.conf was written
if [ -f "$TMUX_CONF_TEST" ] && grep -q "tg-cli" "$TMUX_CONF_TEST"; then
  pass "tmux hooks: tmux.conf written with hook config"
else
  fail "tmux hooks: tmux.conf not written or missing tg-cli hooks"
fi

# Verify hooks are registered on test server
HOOKS_OUTPUT=$($TMUX_TEST show-hooks -g 2>&1) || true
echo "  DEBUG: HOOKS_OUTPUT (${#HOOKS_OUTPUT} chars): $HOOKS_OUTPUT"
set +eo pipefail
echo "$HOOKS_OUTPUT" | grep -q "session-created"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "tmux hooks: session-created hook registered on test server"
else
  fail "tmux hooks: session-created hook not found on test server"
fi
set +eo pipefail
echo "$HOOKS_OUTPUT" | grep -q "session-closed"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "tmux hooks: session-closed hook registered on test server"
else
  fail "tmux hooks: session-closed hook not found on test server"
fi

# =============================================
# Test 2: session-created event triggers hook
# =============================================

LOG_BEFORE_CREATE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Create a new tmux session on the test server
$TMUX_TEST new-session -d -s "$HOOK_TEST_SESSION"

# Wait for hook to fire and bot to process
ELAPSED=0
CREATE_FOUND=false
while [ $ELAPSED -lt 15 ]; do
  set +eo pipefail
  tail -n +"$((LOG_BEFORE_CREATE + 1))" "$LOG_FILE" | grep -q "tmux session-created notification:.*$HOOK_TEST_SESSION"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    CREATE_FOUND=true
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

if [ "$CREATE_FOUND" = true ]; then
  pass "tmux hooks: session-created event triggered and logged"
else
  fail "tmux hooks: session-created event not detected within 15s"
fi

# =============================================
# Test 3: session-closed event triggers hook
# =============================================

LOG_BEFORE_CLOSE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Kill the test session
$TMUX_TEST kill-session -t "=$HOOK_TEST_SESSION" 2>/dev/null || true

# Wait for hook to fire
ELAPSED=0
CLOSE_FOUND=false
while [ $ELAPSED -lt 15 ]; do
  set +eo pipefail
  tail -n +"$((LOG_BEFORE_CLOSE + 1))" "$LOG_FILE" | grep -q "tmux session-closed notification:.*$HOOK_TEST_SESSION"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    CLOSE_FOUND=true
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

if [ "$CLOSE_FOUND" = true ]; then
  pass "tmux hooks: session-closed event triggered and logged"
else
  fail "tmux hooks: session-closed event not detected within 15s"
fi

# =============================================
# Cleanup
# =============================================

rm -f "$TMUX_CONF_TEST"
pane_log "[tmux_hooks] AFTER test"
# Re-install with --skip-tmux to restore clean state for subsequent phases
echo "" | ./tg-cli --config-dir "$TEST_CONFIG_DIR" install \
  --port "$TEST_PORT" \
  --settings "$TEST_CLAUDE_CONFIG_DIR/settings.json" \
  --skip-tmux > /dev/null 2>&1 || true
