#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Header options test (session send --no-header, cron inject header, --skip-tmux) ---"

ensure_infrastructure

# =============================================
# Test 1: session send header behavior
# =============================================

# Extract short pane_id and fake TMUX env for simulating "from within e2e-cli pane"
SHORT_PANE="${E2E_PANE%@*}"
SOCK_PATH="${E2E_PANE#*@}"
FAKE_TMUX="${SOCK_PATH},1234,0"

# 1a: auto-resolve from via TMUX_PANE/TMUX env → inject header with resolved sender
LOG_BEFORE_1A=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
env TMUX_PANE="$SHORT_PANE" TMUX="$FAKE_TMUX" \
  ./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-cli --port "$TEST_PORT" --text "header_test_auto" > /dev/null 2>&1 || true
sleep 2
NEW_1A=$(tail -n +"$((LOG_BEFORE_1A + 1))" "$LOG_FILE")
set +eo pipefail
echo "$NEW_1A" | grep -q 'Session send via API:.*from=e2e-cli.*header_test_auto.*injectText=.*💬 Message from agent \[e2e-cli\]'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'Session send via API...auto' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session send: auto-resolved from + header injected"
else
  fail "session send: auto-resolve or header injection missing"
fi
set +eo pipefail
echo "$NEW_1A" | grep -q 'Session send notification:.*from=e2e-cli.*header_test_auto'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'Session send notification...auto' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session send: TG notification log includes auto-resolved from"
else
  fail "session send: TG notification missing or missing from (auto-resolved)"
fi

wait_for_idle

# 1b: no TMUX_PANE and no --from → command must error out, no inject logged
LOG_BEFORE_1B=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
set +e
STDERR_1B=$(env -u TMUX_PANE -u TMUX ./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-cli --port "$TEST_PORT" --text "header_test_noresolve" 2>&1 >/dev/null)
EXIT_1B=$?
set -e
set +eo pipefail
echo "$STDERR_1B" | grep -q "cannot resolve sender"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'cannot resolve sender' PIPESTATUS=${_ps[*]}"
if [ "$EXIT_1B" -ne 0 ] && [ "${_ps[1]}" -eq 0 ]; then
  pass "session send: rejects anonymous send without from"
else
  fail "session send: unexpectedly accepted anonymous send (exit=$EXIT_1B stderr=$STDERR_1B)"
fi
NEW_1B=$(tail -n +"$((LOG_BEFORE_1B + 1))" "$LOG_FILE")
set +eo pipefail
echo "$NEW_1B" | grep -q 'Session send via API:.*header_test_noresolve'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'Session send via API...noresolve' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  fail "session send: anonymous text reached bot log (inject should be blocked)"
else
  pass "session send: anonymous text not logged (inject blocked)"
fi

# 1c: explicit --from → header uses explicit sender
LOG_BEFORE_1C=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-cli --port "$TEST_PORT" --from manual-sender --text "header_test_explicit" > /dev/null 2>&1 || true
sleep 2
NEW_1C=$(tail -n +"$((LOG_BEFORE_1C + 1))" "$LOG_FILE")
set +eo pipefail
echo "$NEW_1C" | grep -q 'Session send via API:.*from=manual-sender.*header_test_explicit.*injectText=.*💬 Message from agent \[manual-sender\]'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'Session send via API...explicit' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session send --from: explicit sender used in inject header"
else
  fail "session send --from: explicit sender not reflected in inject header"
fi
set +eo pipefail
echo "$NEW_1C" | grep -q 'Session send notification:.*from=manual-sender.*header_test_explicit'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'Session send notification...explicit' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session send --from: TG notification log includes from"
else
  fail "session send --from: TG notification log missing from"
fi

wait_for_idle

# 1d: explicit --from + --no-header → inject text has no header prefix
LOG_BEFORE_1D=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-cli --port "$TEST_PORT" --from manual-sender --text "header_test_noheader" --no-header > /dev/null 2>&1 || true
sleep 2
NEW_1D=$(tail -n +"$((LOG_BEFORE_1D + 1))" "$LOG_FILE")
set +eo pipefail
echo "$NEW_1D" | grep -q 'Session send via API:.*noHeader=true.*header_test_noheader.*injectText="header_test_noheader"'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'Session send via API...noheader' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session send --no-header: inject has no header prefix"
else
  fail "session send --no-header: inject has header prefix or log missing"
fi
set +eo pipefail
echo "$NEW_1D" | grep -q 'Session send notification:.*from=manual-sender.*header_test_noheader'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'Session send notification...noheader' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session send --no-header: TG notification still sent with from"
else
  fail "session send --no-header: TG notification missing"
fi

wait_for_idle

# =============================================
# Test 2: --skip-tmux
# =============================================

# Run install with --skip-tmux and check stdout
INSTALL_OUTPUT=$(echo "" | ./tg-cli --config-dir "$TEST_CONFIG_DIR" install --port "$TEST_PORT" --settings "$TEST_CLAUDE_CONFIG_DIR/settings.json" --skip-tmux 2>&1) || true
echo "  DEBUG: INSTALL_OUTPUT (${#INSTALL_OUTPUT} chars): $INSTALL_OUTPUT"
set +eo pipefail
echo "$INSTALL_OUTPUT" | grep -q "tmux hook registered"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'tmux hook registered' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
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
echo "  DEBUG: CRON_ADD_RESP (${#CRON_ADD_RESP} chars): $CRON_ADD_RESP"
CRON_JOB_ID=$(echo "$CRON_ADD_RESP" | jq -r '.id // ""')

if [ -z "$CRON_JOB_ID" ]; then
  fail "cron inject header: failed to add job"
else
  # Poll for cron execution (30s tick + margin)
  CRON_ELAPSED=0
  CRON_FIRED=false
  while [ $CRON_ELAPSED -lt 60 ]; do
    set +eo pipefail
    tail -n +"$((LOG_BEFORE_CRON + 1))" "$LOG_FILE" | grep -q "Cron inject job: injected to"
    _ps=("${PIPESTATUS[@]}")
    set -eo pipefail
    echo "  DEBUG: grep 'Cron inject job: injected to' PIPESTATUS=${_ps[*]}"
    if [ "${_ps[1]}" -eq 0 ]; then
      CRON_FIRED=true
      break
    fi
    sleep 2
    CRON_ELAPSED=$((CRON_ELAPSED + 2))
    echo "  Waiting for cron inject... ${CRON_ELAPSED}s / 60s"
  done

  if [ "$CRON_FIRED" = true ]; then
    pass "cron inject header: job executed"

    # Capture pane to verify header was injected
    PANE_CONTENT=$(curl -s "http://127.0.0.1:$TEST_PORT/capture?target=$(printf '%s' "$E2E_PANE" | jq -sRr @uri)" | jq -r '.content // ""')
    echo "  DEBUG: PANE_CONTENT (${#PANE_CONTENT} chars): $PANE_CONTENT"
    set +eo pipefail
    echo "$PANE_CONTENT" | grep -q "Cron:"
    _ps=("${PIPESTATUS[@]}")
    set -eo pipefail
    echo "  DEBUG: grep 'Cron:' PIPESTATUS=${_ps[*]}"
    if [ "${_ps[1]}" -eq 0 ]; then
      pass "cron inject header: ⏰ Cron header found in pane"
    else
      fail "cron inject header: ⏰ Cron header NOT found in pane"
    fi
  else
    fail "cron inject header: job did not execute within 60s"
  fi

  # Cleanup
  curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/remove" \
    -H "Content-Type: application/json" \
    -d "{\"id\":\"$CRON_JOB_ID\"}" > /dev/null 2>&1
fi

wait_for_idle
pane_log "[header_options] AFTER all tests"
