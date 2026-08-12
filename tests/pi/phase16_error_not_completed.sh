#!/bin/bash
# Phase 16 = Round-4 Item 3b (v18): a GENUINE provider-fault error run (pi stopReason "error" after retries
# exhausted, NOT an abort) must NOT be relabelled Task Completed. The extension posts agent_idle{stop_reason:
# "error", error_message:<provider error>} (not Stop); the Go handler SetIdles AND sends a standalone AgentError
# notification carrying the error text; the last bubble stays a plain Message (never "✅ Task Completed").
#
# Induce a real provider fault deterministically: pi loads its provider config at startup, so we launch a
# DEDICATED pi pane whose models.json base-url points at an unreachable local port (127.0.0.1:1 -> immediate
# ECONNREFUSED). Every model request fails at the transport, pi's retry loop exhausts, and the assistant message
# ends stopReason "error" (provider layer: signal.aborted ? "aborted" : "error" -> "error", errorMessage carries
# the transport error). This is the error branch WITHOUT an ESC abort — distinct from phase15's abort-shaped error.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi Item3b (v18): a provider-fault error run is NOT reported Task Completed ---"

ensure_infrastructure
require_pi_key
ensure_pi_config

# Break ONLY the base-url (keep provider/model/key shape valid so pi accepts the config and actually attempts the
# request, then fails at the transport). Restore on exit so later phases are unaffected (start_pi also rewrites it).
MODELS_JSON="$PI_CODING_AGENT_DIR/models.json"
MODELS_BACKUP="$PI_CODING_AGENT_DIR/models.json.phase16bak"
command cp -f "$MODELS_JSON" "$MODELS_BACKUP"
MODELS_JSON="$MODELS_JSON" python3 - <<'PY'
import json, os
p = os.environ["MODELS_JSON"]
c = json.load(open(p))
for prov in c.get("providers", {}).values():
    prov["baseUrl"] = "http://127.0.0.1:1/v1"  # nothing listens on port 1 -> immediate ECONNREFUSED
tmp = p + ".tmp"
json.dump(c, open(tmp, "w"), indent=2)
os.replace(tmp, p)
PY

E2E_SESSION="e2e-pi-16"
_p16_cleanup() {
  local rc=$?
  [ -f "$MODELS_BACKUP" ] && command cp -f "$MODELS_BACKUP" "$PI_CODING_AGENT_DIR/models.json" && rm -f "$MODELS_BACKUP" || true
  $TMUX_TEST kill-session -t "=$E2E_SESSION" 2>/dev/null || true
  exit $rc
}
trap _p16_cleanup EXIT

# Launch a fresh pi pane on the broken config (no warmup — the first real turn IS the error turn).
mkdir -p "$TEST_CONFIG_DIR/pi-sessions/$E2E_SESSION"
_launch_pi_pane "$E2E_SESSION" "$TEST_CONFIG_DIR/pi-sessions/$E2E_SESSION"
echo "Waiting for pi (broken base-url) to register..."
REG=false
for i in $(seq 1 45); do
  if [ "$(pi_session_registered "$E2E_PANE")" = "True" ]; then REG=true; echo "  registered at t=$((i*2))s"; break; fi
  sleep 2
done
[ "$REG" = true ] || fail "pi Item3b: pane never registered (SessionStart not received) — cannot test the error run"
pane_log "[pi/item3b] registered on broken base-url"

LOG_E=$(wc -l < "$LOG_FILE")
inject_prompt "Reply with exactly one word: hello"

# Every request hits ECONNREFUSED; pi retries then settles stopReason error. Give the retry loop room.
AI_SEEN=false
for i in $(seq 1 120); do
  if tail -n +"$((LOG_E + 1))" "$LOG_FILE" | grep -q "Raw hook payload \[agent_idle\]:"; then AI_SEEN=true; break; fi
  sleep 1
done
[ "$AI_SEEN" = true ] || fail "pi Item3b: the errored run never settled with agent_idle within 120s (retries may still be running, or agent_settled did not fire) — report to note3"
sleep 2
pane_log "[pi/item3b] after error settle"
SLICE_E=$(tail -n +"$((LOG_E + 1))" "$LOG_FILE")

# E.1: agent_idle carries stopReason "error" verbatim + a NON-EMPTY error_message (the transport failure text).
AI_E=$(printf '%s\n' "$SLICE_E" | awk '/Raw hook payload \[agent_idle\]:/{print; exit}')
echo "  DEBUG-E agent_idle (first 300): ${AI_E:0:300}"
set +eo pipefail
printf '%s\n' "$AI_E" | grep -q '"stop_reason":"error"'
_ps_sr=("${PIPESTATUS[@]}")
printf '%s\n' "$AI_E" | grep -qE '"error_message":"[^"]+"'
_ps_em=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps_sr[1]}" -eq 0 ] && pass "pi Item3b: settle posted agent_idle with stop_reason=error (provider fault)" \
  || record_fail "pi Item3b: agent_idle did not carry stop_reason=error for the provider fault"
[ "${_ps_em[1]}" -eq 0 ] && pass "pi Item3b: agent_idle carried a non-empty error_message (the transport error text)" \
  || record_fail "pi Item3b: agent_idle error_message was empty for the provider fault (nothing to forward)"

# E.2: a standalone AgentError notification was sent.
set +eo pipefail
printf '%s\n' "$SLICE_E" | grep -q "Notification sent to chat.*: AgentError "
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[1]}" -eq 0 ] && pass "pi Item3b: standalone ⚠️ Run Error (AgentError) notification sent, carrying the error text" \
  || record_fail "pi Item3b: NO AgentError notification for the provider-fault error run (D3 violated)"

# E.3: NO Stop / no Task-Completed relabel for the errored turn.
set +eo pipefail
printf '%s\n' "$SLICE_E" | grep -qE "Raw hook payload \[Stop\]:|outcome=direct_send"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[1]}" -ne 0 ] && pass "pi Item3b: no Stop / no outcome=direct_send for the errored turn (bubble not relabelled Task Completed)" \
  || record_fail "pi Item3b: a Stop / direct_send was emitted for the errored turn — reported as completed"

# E.4: NO "Task Completed" notification header rendered for this turn.
set +eo pipefail
printf '%s\n' "$SLICE_E" | grep -q "Task Completed"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[1]}" -ne 0 ] && pass "pi Item3b: no \"Task Completed\" header for the errored turn" \
  || record_fail "pi Item3b: a \"Task Completed\" header appeared for the errored turn — the failed run was reported as complete"

# E.5: busy cleared.
[ "$(pi_api_idle)" = "True" ] && pass "pi Item3b: busy cleared after the errored run (idle API True)" \
  || record_fail "pi Item3b: busy did not clear after the errored run (idle API still running)"

echo ""
echo "  pi Item3b (v18) error-not-completed test complete."
