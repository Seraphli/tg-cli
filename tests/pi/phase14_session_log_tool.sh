#!/bin/bash
# Phase 14 = Round-2 Item B: `tg-cli session log` must render a pi tool line's DETAIL as the tool's argument
# (e.g. the bash command), NOT the raw arguments JSON. Root cause: ParsePiTranscript took the formatToolDetail
# callback but never invoked it, hardcoding string(arguments); the fix calls it (with NormalizePiToolName +
# argMap), so extractToolParam / the /session/log closure render a clean detail. entry.Tool keeps pi's
# lowercase name (O2).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi Item B: session log tool detail (command, not raw JSON) ---"

ensure_infrastructure
start_pi "e2e-pi-14"

MARK="PI_SESSIONLOG_MARKER"
pane_log "[pi/item B] before bash inject"
inject_prompt "Run the bash command: echo $MARK. Do not explain — just run the bash tool."
wait_for_idle "$TIMEOUT" "$E2E_PANE"
sleep 2

# Name the session for `session log --name`.
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
[ -n "$SESSION_ID" ] || fail "pi Item B: could not resolve session id for target $E2E_PANE"
curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=e2e-pi-log14" > /dev/null 2>&1 || true

LOG_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-pi-log14 --port "$TEST_PORT" --lines 30 2>&1) || true
echo "  === DEBUG: session log output ==="
printf '%s\n' "$LOG_OUTPUT" | sed 's/^/    /'
echo "  === END DEBUG ==="

# PRECONDITION: the tool line must have rendered (the bash tool call reached the transcript).
printf '%s\n' "$LOG_OUTPUT" | grep -q "$MARK" || fail "pi Item B PRECONDITION: the bash tool line ($MARK) is not in the session log — cannot test the tool-detail render"

# Item B: the tool detail must be the CLEAN command, NOT the raw arguments JSON ({"command":...}).
set +eo pipefail
printf '%s\n' "$LOG_OUTPUT" | grep -qE '\{"command"|\{&quot;command|\{"path"|command":'
_ps_raw=("${PIPESTATUS[@]}")
printf '%s\n' "$LOG_OUTPUT" | grep -q "echo $MARK"
_ps_cmd=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps_raw[1]}" -ne 0 ] && pass "pi Item B: session log tool detail is not the raw arguments JSON" \
  || record_fail "pi Item B: session log tool detail dumped the raw arguments JSON ({\"command\":…})"
[ "${_ps_cmd[1]}" -eq 0 ] && pass "pi Item B: session log tool detail renders the command (echo $MARK)" \
  || record_fail "pi Item B: session log tool detail missing the command (echo $MARK)"

pane_log "[pi/item B] end"
echo "  pi Item B session-log tool-detail test complete."
