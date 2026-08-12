#!/bin/bash
# Phase 8 = SPEC (h): a pi pane launched WITHOUT tg-cli orchestration (plain pi, NO --session-dir, extension
# globally installed in the test pi dir, NOT via -e) streams its assistant text to Telegram AND renders
# under `tg-cli session log`. Proves ruling D (global install) + the transcript-path-from-payload path for a
# pane tg-cli did not start. Uses NO --session-dir so getSessionFile() must supply the absolute path itself.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi (h) hand-launched pane streams + session log ---"

ensure_infrastructure
require_pi_key
ensure_pi_config

# Plain pi, NO --session-dir. The extension is already globally installed by setup_hooks (tg-cli install ->
# InstallPiExtension into PI_CODING_AGENT_DIR), so a hand launch auto-discovers it with no -e.
_launch_pi_pane "e2e-pi-8" ""
_PI_PHASE_SESSION="e2e-pi-8"
trap '_pi_phase_cleanup' EXIT

# The hand-launched pane still registers via the global extension.
elapsed=0
reg=false
while [ $elapsed -lt 90 ]; do
  if [ "$(pi_session_registered "$E2E_PANE")" = "True" ]; then reg=true; break; fi
  sleep 2
  elapsed=$((elapsed + 2))
done
if [ "$reg" = true ]; then
  pass "pi (h): hand-launched pane registered via the global extension (no -e, no --session-dir)"
else
  fail "pi (h): hand-launched pane did not register within 90s"
fi

# Stream a turn from the hand-launched pane.
LOG_BEFORE=$(wc -l < "$LOG_FILE")
pane_log "[pi/hand] before inject"
inject_prompt "Reply with exactly: pi_hand_launch_marker_h8. Then write two sentences about the ocean. Do not use any tools."
wait_for_idle "$TIMEOUT" "$E2E_PANE"
SLICE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE")

# (h.1) the hand-launched pane streams its text to Telegram.
set +eo pipefail
printf '%s\n' "$SLICE" | grep -qE "Stream (send|edit):"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (h): hand-launched pane streams assistant text to Telegram"
else
  fail "pi (h): no Stream send/edit for the hand-launched pane"
fi

# (h.2) session log renders the hand-launched transcript (path from the extension payload, no --session-dir).
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
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=e2e-pi-hand" > /dev/null 2>&1 || true
else
  fail "pi (h): could not resolve session id for hand-launched pane $E2E_PANE"
fi

LOG_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-pi-hand --port "$TEST_PORT" --lines 20 2>&1) || true
echo "  DEBUG: session log (${#LOG_OUTPUT} chars): $LOG_OUTPUT"
set +eo pipefail
echo "$LOG_OUTPUT" | grep -q "pi_hand_launch_marker_h8"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (h): session log renders the hand-launched pi transcript (path from payload)"
else
  fail "pi (h): marker not found in hand-launched session log: ${LOG_OUTPUT%%$'\n'*}"
fi

pane_log "[pi/hand] end"
echo "  pi (h) hand-launched test complete."
