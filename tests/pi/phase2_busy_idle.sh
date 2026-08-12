#!/bin/bash
# Phase 2 = SPEC (b): run a turn and assert the busy indicator goes busy->idle, store-driven via the
# extension (agent_start->SetRunning, agent_settled->Stop->SetIdle).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi (b) busy -> idle (store-driven) ---"

ensure_infrastructure
start_pi "e2e-pi-2"

TYPING_BEFORE=$(wc -l < "$TYPING_LOG_FILE" 2>/dev/null || echo 0)
pane_log "[pi/busy] before inject"
inject_prompt "Write a detailed eight-sentence paragraph about the history of terminal multiplexers. Do not use any tools."

# Catch the store-driven BUSY state during the run (agent_start -> SetRunning). /session/idle reads the
# product IsSessionRunning store-first path (gated on DetectBackend==pi).
BUSY_SEEN=false
for i in $(seq 1 40); do
  st=$(check_session_idle "$E2E_PANE")
  if [ "$st" = "busy" ]; then BUSY_SEEN=true; echo "  busy observed at poll $i"; break; fi
  sleep 1
done
if [ "$BUSY_SEEN" = true ]; then
  pass "pi (b): session went BUSY during the run (store-driven agent_start)"
else
  fail "pi (b): never observed busy after inject (store not driven by agent_start?)"
fi

# Settle: agent_settled -> Stop -> SetIdle.
wait_for_idle "$TIMEOUT" "$E2E_PANE"
FINAL=$(check_session_idle "$E2E_PANE")
if [ "$FINAL" = "idle" ]; then
  pass "pi (b): session returned to IDLE after settle (Stop -> SetIdle)"
else
  fail "pi (b): session not idle after wait_for_idle (state=$FINAL)"
fi

# Store transition markers in the typing log: agent_start=running and Stop=idle.
TSLICE=$(tail -n +"$((TYPING_BEFORE + 1))" "$TYPING_LOG_FILE" 2>/dev/null || true)
set +eo pipefail
printf '%s\n' "$TSLICE" | grep -q "state: event=agent_start .*state=running"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (b): typing log shows agent_start -> state=running"
else
  fail "pi (b): no agent_start/state=running in typing log"
fi
set +eo pipefail
printf '%s\n' "$TSLICE" | grep -q "state: event=Stop .*state=idle"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (b): typing log shows Stop -> state=idle"
else
  fail "pi (b): no Stop/state=idle in typing log"
fi

pane_log "[pi/busy] end"
echo "  pi (b) busy/idle test complete."
