#!/bin/bash
# Phase 7 = SPEC (g): the start_pi readiness gate keys on session registration via /session/list (the
# extension's SessionStart POST arrived), NOT the idle API — for which an unregistered pane and an idle
# one are wire-identical. Assert: immediately after a raw launch the session is NOT yet in /session/list
# while /session/idle already reports non-busy (so gating on idle would false-proceed), and that the
# session DOES eventually register.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi (g) readiness gate keys on /session/list ---"

ensure_infrastructure
require_pi_key
ensure_pi_config

# Raw launch WITHOUT start_pi's registration wait / warmup, so we can observe the pre-registration window.
_launch_pi_pane "e2e-pi-7" "$TEST_CONFIG_DIR/pi-sessions/e2e-pi-7"
_PI_PHASE_SESSION="e2e-pi-7"
trap '_pi_phase_cleanup' EXIT

# Sample IMMEDIATELY (t~=0): the extension's SessionStart POST takes ~2-3s, so the session is not yet
# registered — but /session/idle already reads non-busy (no turn running).
IDLE0=$(check_session_idle "$E2E_PANE")
REG0=$(pi_session_registered "$E2E_PANE")
echo "  DEBUG t0: idle=$IDLE0 registered=$REG0"

# (g.1) not yet registered right after launch — the gate MUST wait for /session/list.
if [ "$REG0" = "False" ]; then
  pass "pi (g): session NOT yet in /session/list immediately after launch (gate must wait for registration)"
else
  fail "pi (g): session already registered at t=0 — cannot demonstrate the gate waits (reg0=$REG0)"
fi

# (g.2) /session/idle is wire-identical to a ready-idle session (idle/unknown, not busy) while unregistered,
# so a gate on the idle API would false-proceed here.
if [ "$IDLE0" != "busy" ]; then
  pass "pi (g): /session/idle reports '$IDLE0' (not busy) while unregistered — idle API cannot gate readiness"
else
  fail "pi (g): /session/idle reported busy at t=0 (unexpected — no turn is running yet)"
fi

# (g.3) the correct gate: poll /session/list until the pi session registers.
elapsed=0
reg=false
while [ $elapsed -lt 90 ]; do
  if [ "$(pi_session_registered "$E2E_PANE")" = "True" ]; then reg=true; break; fi
  sleep 2
  elapsed=$((elapsed + 2))
done
if [ "$reg" = true ]; then
  pass "pi (g): session registers in /session/list (correct readiness gate, t=${elapsed}s)"
else
  fail "pi (g): session never registered in /session/list within 90s"
fi

pane_log "[pi/readiness] end"
echo "  pi (g) readiness gate test complete."
