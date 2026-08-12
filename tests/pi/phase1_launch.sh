#!/bin/bash
# Phase 1 = SPEC (a): launch a pi pane via the harness; assert the bot log records "backend":"pi" AND the
# session appears in /session/list (registered from the extension's SessionStart POST).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi (a) launch + backend recognition ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE")
start_pi "e2e-pi-1"

SLICE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE")

# (a.1) The bot received the pi hook POST at all.
set +eo pipefail
printf '%s\n' "$SLICE" | grep -q "Raw hook payload"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (a): hook HTTP POST received by bot"
else
  fail "pi (a): no 'Raw hook payload' in bot log after start"
fi

# (a.2) backend=pi recorded in the raw SessionStart payload (extension self-reports backend:"pi").
set +eo pipefail
printf '%s\n' "$SLICE" | grep -q '"backend":"pi"'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (a): bot log records backend:pi"
else
  fail "pi (a): backend:pi not found in bot log"
fi

# (a.3) SessionStart tracked / notification fired.
set +eo pipefail
printf '%s\n' "$SLICE" | grep -q "Session tracked"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (a): SessionStart tracked in bot log"
else
  fail "pi (a): 'Session tracked' not found in bot log"
fi

# (a.4) The session appears in /session/list (the readiness/registration gate).
LIST_JSON=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" 2>/dev/null || echo '{}')
echo "  DEBUG: /session/list = $LIST_JSON"
if [ "$(pi_session_registered "$E2E_PANE")" = "True" ]; then
  pass "pi (a): pi session present in /session/list (target=$E2E_PANE)"
else
  fail "pi (a): pi session NOT present in /session/list (target=$E2E_PANE): $LIST_JSON"
fi

# (a.5) /session/list reports this session's backend as pi (if the field is exposed).
BK=$(printf '%s' "$LIST_JSON" | python3 -c '
import sys, json
pane = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); sys.exit(0)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane.split("@")[0] + "@"):
        print(s.get("backend", "")); sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
echo "  DEBUG: /session/list backend field = '$BK'"
if [ "$BK" = "pi" ]; then
  pass "pi (a): /session/list reports backend=pi"
else
  # Non-fatal: the list endpoint may not surface the backend field; (a.2) already proved backend:pi.
  echo "  NOTE: /session/list did not surface backend=pi (field value='$BK'); backend:pi already asserted via bot log"
fi

pane_log "[pi/launch] end"
echo "  pi (a) launch test complete."
