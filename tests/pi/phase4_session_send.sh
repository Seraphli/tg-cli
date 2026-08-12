#!/bin/bash
# Phase 4 = SPEC (d): `tg-cli session send` a prompt into the pi pane and assert it runs.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi (d) session send into pi pane ---"

ensure_infrastructure
start_pi "e2e-pi-4"

# Name the session so `session send --name` can target it.
SESSION_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
pane = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); sys.exit(0)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane.split("@")[0] + "@"):
        print(s.get("id", "")); sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
if [ -n "$SESSION_ID" ]; then
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=e2e-pi-send" > /dev/null 2>&1 || true
  echo "  Named session $SESSION_ID as e2e-pi-send"
else
  fail "pi (d): could not resolve session id for target $E2E_PANE"
fi

LOG_BEFORE=$(wc -l < "$LOG_FILE")
pane_log "[pi/session_send] before send"
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-pi-send --port "$TEST_PORT" --from e2e-test \
  --text "Reply with exactly: pi_session_send_marker_d4. Do not run any tools or commands." > /dev/null 2>&1 || true
sleep 2
pane_log "[pi/session_send] after send"

# (d.1) the injected text is recorded in the bot log (send was received + injected).
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "pi_session_send_marker_d4"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (d): session send injected + logged"
else
  fail "pi (d): session send injection not found in bot log"
fi

# (d.2) the prompt actually RAN: the pi session went busy then settled with a Stop for this turn.
wait_for_idle "$TIMEOUT" "$E2E_PANE"
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "Raw hook payload \[Stop\]:"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (d): the injected prompt ran to completion (Stop received for the turn)"
else
  fail "pi (d): no Stop after session send — prompt did not run"
fi

pane_log "[pi/session_send] end"
echo "  pi (d) session send test complete."
