#!/bin/bash
# Phase 5 = SPEC (e): `tg-cli session log` renders the pi transcript (via ParsePiTranscript).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi (e) session log renders pi transcript ---"

ensure_infrastructure
start_pi "e2e-pi-5"

# Produce a uniquely markable assistant turn in the transcript.
pane_log "[pi/session_log] before marker inject"
inject_prompt "Reply with exactly: pi_transcript_marker_e5. Do not use any tools."
wait_for_idle "$TIMEOUT" "$E2E_PANE"

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
if [ -n "$SESSION_ID" ]; then
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=e2e-pi-log" > /dev/null 2>&1 || true
else
  fail "pi (e): could not resolve session id for target $E2E_PANE"
fi

LOG_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-pi-log --port "$TEST_PORT" --lines 20 2>&1) || true
echo "  DEBUG: session log (${#LOG_OUTPUT} chars): $LOG_OUTPUT"

# (e.1) header contains the tmux target marker.
set +eo pipefail
echo "$LOG_OUTPUT" | grep -q "📟"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (e): session log header has tmux target (📟)"
else
  fail "pi (e): session log missing tmux target header: ${LOG_OUTPUT%%$'\n'*}"
fi

# (e.2) separator lines between messages.
set +eo pipefail
echo "$LOG_OUTPUT" | grep -q "────────────────────────"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (e): session log has separator lines"
else
  fail "pi (e): session log missing separator lines"
fi

# (e.3) timestamps present.
set +eo pipefail
echo "$LOG_OUTPUT" | grep -qE "[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (e): session log has timestamps"
else
  fail "pi (e): session log missing timestamps"
fi

# (e.4) the parsed pi transcript contains the marker (proves ParsePiTranscript rendered assistant text).
set +eo pipefail
echo "$LOG_OUTPUT" | grep -q "pi_transcript_marker_e5"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (e): session log renders the pi assistant text (ParsePiTranscript)"
else
  fail "pi (e): marker 'pi_transcript_marker_e5' not found in rendered transcript"
fi

# (e.5) --format json yields valid structure (target + messages).
JSON_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-pi-log --port "$TEST_PORT" --lines 5 --format json 2>&1) || true
if echo "$JSON_OUTPUT" | python3 -c "import sys,json; d=json.load(sys.stdin); assert 'target' in d and 'messages' in d" 2>/dev/null; then
  pass "pi (e): session log --format json valid (target + messages)"
else
  fail "pi (e): session log --format json invalid: ${JSON_OUTPUT%%$'\n'*}"
fi

pane_log "[pi/session_log] end"
echo "  pi (e) session log test complete."
