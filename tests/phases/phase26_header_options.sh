#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Header options test (session send --no-header, cron inject header, --skip-tmux) ---"

ensure_infrastructure

# =============================================
# Test 1: session send --no-header
# =============================================

# 1a: Send WITH header (default) — should have TG notification
LOG_BEFORE_H=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-cli --port "$TEST_PORT" --text "header_test_with" > /dev/null 2>&1 || true
sleep 2
if tail -n +"$((LOG_BEFORE_H + 1))" "$LOG_FILE" | grep -q "Session send notification:"; then
  pass "session send: TG notification sent (with header)"
else
  fail "session send: TG notification missing (with header)"
fi

# 1b: Send WITHOUT header (--no-header) — TG notification should still be sent
LOG_BEFORE_NH=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-cli --port "$TEST_PORT" --text "header_test_noheader" --no-header > /dev/null 2>&1 || true
sleep 2
if tail -n +"$((LOG_BEFORE_NH + 1))" "$LOG_FILE" | grep -q "Session send via API:.*header_test_noheader"; then
  pass "session send --no-header: API log recorded"
else
  fail "session send --no-header: API log not found"
fi
if tail -n +"$((LOG_BEFORE_NH + 1))" "$LOG_FILE" | grep -q "Session send notification:"; then
  pass "session send --no-header: TG notification still sent"
else
  fail "session send --no-header: TG notification missing"
fi

wait_for_cc_idle

# =============================================
# Test 2: --skip-tmux
# =============================================

# Run install with --skip-tmux and check stdout
INSTALL_OUTPUT=$(echo "" | ./tg-cli --config-dir "$TEST_CONFIG_DIR" install --port "$TEST_PORT" --settings "$TEST_CLAUDE_CONFIG_DIR/settings.json" --skip-tmux 2>&1) || true
if echo "$INSTALL_OUTPUT" | grep -q "tmux hook registered"; then
  fail "--skip-tmux: tmux hooks were registered despite --skip-tmux"
else
  pass "--skip-tmux: tmux hooks correctly skipped"
fi

# =============================================
# Test 3: cron inject header
# =============================================

# Get the CC session name for inject target
SESSION_NAME="e2e-cli"

# 3a: Add inject job WITH header (default)
LOG_BEFORE_CRON=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
CRON_TOKEN="cron-header-test-$RANDOM"
CRON_ADD_RESP=$(curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/add" \
  -H "Content-Type: application/json" \
  -d "{\"mode\":\"inject\",\"schedule\":\"1s\",\"prompt\":\"$CRON_TOKEN\",\"agent_name\":\"$SESSION_NAME\"}")
CRON_JOB_ID=$(echo "$CRON_ADD_RESP" | jq -r '.id // ""')

if [ -z "$CRON_JOB_ID" ]; then
  fail "cron inject header: failed to add job"
else
  # Wait for cron loop (30s tick + margin)
  echo "  Waiting 35s for cron inject to fire..."
  sleep 35

  # Check bot log for execution
  if tail -n +"$((LOG_BEFORE_CRON + 1))" "$LOG_FILE" | grep -q "Cron inject job: injected to"; then
    pass "cron inject header: job executed"

    # Capture pane to verify header was injected
    PANE_CONTENT=$(curl -s "http://127.0.0.1:$TEST_PORT/capture?target=$(printf '%s' "$CLAUDE_PANE" | jq -sRr @uri)" | jq -r '.content // ""')
    if echo "$PANE_CONTENT" | grep -q "Cron:"; then
      pass "cron inject header: ⏰ Cron header found in pane"
    else
      fail "cron inject header: ⏰ Cron header NOT found in pane"
    fi
  else
    fail "cron inject header: job did not execute within 35s"
  fi

  # Cleanup
  curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/remove" \
    -H "Content-Type: application/json" \
    -d "{\"id\":\"$CRON_JOB_ID\"}" > /dev/null 2>&1
fi

wait_for_cc_idle
pane_log "[header_options] AFTER all tests"
