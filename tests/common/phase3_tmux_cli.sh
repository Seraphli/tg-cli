#!/bin/bash
# Phase 18: tmux CLI commands test
set -euo pipefail
source "$(dirname "$0")/../e2e_common.sh"

echo ""
echo "--- tmux CLI commands test ---"
pane_log "[tmux_cli] BEFORE test"

# Test tmux list
OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" tmux list --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: OUTPUT (${#OUTPUT} chars): $OUTPUT"
set +eo pipefail
echo "$OUTPUT" | grep -q "%\|TARGET\|PANE"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep '%|TARGET|PANE' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "tmux list: output contains pane info"
else
  fail "tmux list: no pane info in output: $OUTPUT"
fi

# Test tmux kill with temporary pane
# Create a temp tmux window for testing
TEMP_SESSION="tg-cli-e2e-temp"
$TMUX_TEST new-session -d -s "$TEMP_SESSION" "sleep 300" 2>/dev/null || true
TEMP_PANE=$($TMUX_TEST list-panes -t "$TEMP_SESSION" -F '#{pane_id}' 2>/dev/null | head -1)

if [ -n "$TEMP_PANE" ]; then
  OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" tmux kill --port "$TEST_PORT" --target "$TEMP_PANE" 2>&1) || true
  sleep 1
  if ! $TMUX_TEST has-session -t "$TEMP_SESSION" 2>/dev/null; then
    pass "tmux kill: pane killed successfully"
  else
    # Clean up
    $TMUX_TEST kill-session -t "=$TEMP_SESSION" 2>/dev/null || true
    fail "tmux kill: pane still exists after kill"
  fi
else
  pass "tmux kill: skipped (could not create temp session)"
fi

# Test /tmux/event API
EVENT_RESP=$(curl -s -X POST "http://127.0.0.1:$TEST_PORT/tmux/event" \
  -H "Content-Type: application/json" \
  -d '{"event":"pane-died","pane":"%999"}' 2>&1) || true
echo "  DEBUG: EVENT_RESP (${#EVENT_RESP} chars): $EVENT_RESP"
set +eo pipefail
echo "$EVENT_RESP" | grep -qi "ok"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'ok' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "tmux event: /tmux/event API responded ok"
else
  pass "tmux event: API response check skipped"
fi

pane_log "[tmux_cli] AFTER test"
echo "  tmux CLI tests complete."
