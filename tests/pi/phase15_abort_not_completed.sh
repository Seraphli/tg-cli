#!/bin/bash
# Phase 15 = Round-4 Item 3 (v17): an ESC-aborted pi run must NOT be reported as a completed turn. The extension
# posts agent_idle (NOT Stop) at agent_settled when pi's own stopReason is "aborted" or "error", forwarding that
# stopReason VERBATIM in stop_reason; the Go handler dispatches the notification off it (R2/R3 — notifications
# unify upward, EVERY interrupt notifies):
#   during-TEXT abort (ESC mid-stream)  -> pi stopReason "aborted" -> agent_idle{stop_reason:"aborted"} -> a
#                                          standalone "⏹ Interrupted" (AgentInterrupted) notification.
#   during-TOOL abort (ESC in bash)     -> pi stopReason "error"   -> agent_idle{stop_reason:"error",
#                                          error_message:"This operation was aborted"} -> a "⚠️ Run Error"
#                                          (AgentError) notification.
# Both measured empirically on the fixed binary (rounds/4 note3-r4-*-abort-*.md). BOTH shapes MUST: post agent_idle
# not Stop, emit their notification, leave NO Task-Completed relabel / no outcome=direct_send, and clear busy.
# Sub-test C is the Items 2+3 interlock: an inject queued while a pi run is BUSY is delivered AFTER the abort
# clears busy (the idle-triggered bot.go:81-88 flush that Item 2 enables), never mid-run.
#
# The during-TOOL shape (sub-test B) is note3's empirical gate: if it does NOT produce agent_idle + AgentError,
# that is a finding (STOP + report), not something to paper over.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi Item3 (v17): ESC-aborted run posts agent_idle + notifies, never Task Completed ---"

ensure_infrastructure
start_pi "e2e-pi-15"

# Name the registered pi session so sub-test C can `session send --name` an inject to it.
PI_SESSION_NAME="e2e-pi15"
SID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); sys.exit(0)
pane = sys.argv[1]
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane.split("@")[0] + "@"):
        print(s.get("id", "")); sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
[ -n "$SID" ] && curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SID&name=$PI_SESSION_NAME" >/dev/null 2>&1 || true
echo "  pi session id=$SID named=$PI_SESSION_NAME target=$E2E_PANE"

esc_twice() { # pi aborts the running turn on Escape; send twice (mirrors phase11)
  $TMUX_TEST send-keys -t "$E2E_SESSION" Escape
  sleep 1
  $TMUX_TEST send-keys -t "$E2E_SESSION" Escape
}

wait_for_marker() { # $1=start-line $2=marker-regex $3=max-seconds ; echoes true/false
  local start="$1" rx="$2" max="$3" i
  for i in $(seq 1 "$max"); do
    if tail -n +"$((start + 1))" "$LOG_FILE" | grep -qE "$rx"; then echo true; return; fi
    sleep 1
  done
  echo false
}

# =============================================================================
# Sub-test A: ESC during streamed text -> stopReason "aborted" -> AgentInterrupted notification.
# =============================================================================
echo ""
echo "--- A: during-TEXT abort -> ⏹ Interrupted ---"
LOG_A=$(wc -l < "$LOG_FILE")
pane_log "[pi/item3-A] before inject (text stream)"
inject_prompt "Write a long, detailed, multi-sentence essay (at least fifteen sentences) about the history of computing, from the abacus to modern GPUs. Do NOT use any tools. Just write continuous prose."

# Gate on the first MessageDisplay (streaming underway) then ESC mid-stream.
[ "$(wait_for_marker "$LOG_A" 'Raw hook payload \[MessageDisplay\]:' "$TIMEOUT")" = true ] \
  || fail "pi Item3-A: no MessageDisplay for the text run (model never streamed) — cannot test a during-TEXT abort"
pane_log "[pi/item3-A] first MD seen, aborting mid-stream"
esc_twice

[ "$(wait_for_marker "$LOG_A" 'Raw hook payload \[agent_idle\]:' 90)" = true ] \
  || fail "pi Item3-A: run never settled with agent_idle after the abort (empirical gate: text-abort did not post agent_idle)"
sleep 2
pane_log "[pi/item3-A] after abort+settle"
SLICE_A=$(tail -n +"$((LOG_A + 1))" "$LOG_FILE")

# A.1: the settle payload is agent_idle carrying pi's stopReason "aborted" verbatim.
AI_A=$(printf '%s\n' "$SLICE_A" | awk '/Raw hook payload \[agent_idle\]:/{print; exit}')
echo "  DEBUG-A agent_idle (first 240): ${AI_A:0:240}"
set +eo pipefail
printf '%s\n' "$AI_A" | grep -q '"stop_reason":"aborted"'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[1]}" -eq 0 ] && pass "pi Item3-A: settle posted agent_idle with stop_reason=aborted (verbatim)" \
  || record_fail "pi Item3-A: agent_idle did not carry stop_reason=aborted (during-TEXT abort classification changed)"

# A.2: a standalone AgentInterrupted notification was sent (R2/R3 — the interrupt notifies).
set +eo pipefail
printf '%s\n' "$SLICE_A" | grep -q "Notification sent to chat.*: AgentInterrupted "
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[1]}" -eq 0 ] && pass "pi Item3-A: standalone ⏹ Interrupted (AgentInterrupted) notification sent" \
  || record_fail "pi Item3-A: NO AgentInterrupted notification for the text abort — the interrupt was silent (R2/R3 violated)"

# A.3: NO Stop for the aborted turn (no Task-Completed relabel, no direct_send). SLICE_A is scoped to run A only.
set +eo pipefail
printf '%s\n' "$SLICE_A" | grep -qE "Raw hook payload \[Stop\]:|outcome=direct_send"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[1]}" -ne 0 ] && pass "pi Item3-A: no Stop / no outcome=direct_send for the aborted turn (bubble not relabelled Task Completed)" \
  || record_fail "pi Item3-A: a Stop / direct_send was emitted for the aborted turn — the interrupted turn was reported as completed"

# A.4: busy cleared (typing.log agent_idle->idle) AND the idle API reports idle.
set +eo pipefail
grep -q "event=agent_idle .*state=idle" "$TYPING_LOG_FILE"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[0]}" -eq 0 ] && [ "$(pi_api_idle)" = "True" ] \
  && pass "pi Item3-A: busy cleared after the abort (agent_idle->idle, idle API True)" \
  || record_fail "pi Item3-A: busy did not clear after the abort (agent_idle typing state or idle API still running)"

# =============================================================================
# Sub-test B: ESC during a bash tool -> stopReason "error" -> AgentError notification. (note3 empirical gate.)
# =============================================================================
echo ""
echo "--- B: during-TOOL abort -> ⚠️ Run Error ---"
LOG_B=$(wc -l < "$LOG_FILE")
pane_log "[pi/item3-B] before inject (bash sleep)"
inject_prompt "Your VERY FIRST action must be the bash tool running exactly this command: sleep 30. Do NOT write any text before calling the tool — call bash immediately as your first output, with no preamble."

[ "$(wait_for_marker "$LOG_B" 'Raw hook payload \[PreToolUse\]:' "$TIMEOUT")" = true ] \
  || fail "pi Item3-B: run B PreToolUse never fired (model did not call the tool) — cannot test a during-TOOL abort"
# Text-less precondition (mirrors phase11): a preamble would change the shape. Fail loud, do not fall through.
MD_COUNT=$(tail -n +"$((LOG_B + 1))" "$LOG_FILE" | grep -c "Raw hook payload \[MessageDisplay\]:" || true)
echo "  DEBUG-B run B MessageDisplay count (must be 0) = $MD_COUNT"
[ "$MD_COUNT" -eq 0 ] || fail "pi Item3-B PRECONDITION FAILED: run B emitted $MD_COUNT MessageDisplay (a preamble) — not the text-less tool-abort shape. Strengthen the prompt."
pane_log "[pi/item3-B] PreToolUse seen, aborting the tool"
esc_twice

[ "$(wait_for_marker "$LOG_B" 'Raw hook payload \[agent_idle\]:' 90)" = true ] \
  || fail "pi Item3-B: run B never settled with agent_idle after the tool abort — EMPIRICAL GATE FAILED (during-TOOL abort did not post agent_idle). This is a finding: STOP and report to note3."
sleep 2
pane_log "[pi/item3-B] after abort+settle"
SLICE_B=$(tail -n +"$((LOG_B + 1))" "$LOG_FILE")

# B.1: agent_idle carries stopReason "error" verbatim (pi's during-tool classification).
AI_B=$(printf '%s\n' "$SLICE_B" | awk '/Raw hook payload \[agent_idle\]:/{print; exit}')
echo "  DEBUG-B agent_idle (first 260): ${AI_B:0:260}"
set +eo pipefail
printf '%s\n' "$AI_B" | grep -q '"stop_reason":"error"'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[1]}" -eq 0 ] && pass "pi Item3-B: settle posted agent_idle with stop_reason=error (verbatim)" \
  || record_fail "pi Item3-B: agent_idle did not carry stop_reason=error — EMPIRICAL GATE: the during-TOOL abort shape changed (report to note3)"

# B.2: a standalone AgentError notification was sent (carries the errorMessage).
set +eo pipefail
printf '%s\n' "$SLICE_B" | grep -q "Notification sent to chat.*: AgentError "
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[1]}" -eq 0 ] && pass "pi Item3-B: standalone ⚠️ Run Error (AgentError) notification sent" \
  || record_fail "pi Item3-B: NO AgentError notification for the tool abort (R2/R3 violated)"

# B.3: NO Stop for the aborted turn.
set +eo pipefail
printf '%s\n' "$SLICE_B" | grep -qE "Raw hook payload \[Stop\]:|outcome=direct_send"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[1]}" -ne 0 ] && pass "pi Item3-B: no Stop / no outcome=direct_send for the tool-aborted turn (not reported Task Completed)" \
  || record_fail "pi Item3-B: a Stop / direct_send was emitted for the tool-aborted turn — reported as completed"

# B.4: busy cleared.
[ "$(pi_api_idle)" = "True" ] && pass "pi Item3-B: busy cleared after the tool abort (idle API True)" \
  || record_fail "pi Item3-B: busy did not clear after the tool abort (idle API still running)"

# =============================================================================
# Sub-test C (Items 2+3 interlock): an inject queued while BUSY is delivered AFTER the abort clears busy.
# =============================================================================
echo ""
echo "--- C: inject queued during a busy run is delivered AFTER the abort (idle-triggered flush) ---"
[ -n "$SID" ] || { record_fail "pi Item3-C: pi session was not registered/named — cannot queue an inject (skipping interlock)"; echo "  C skipped"; }
if [ -n "$SID" ]; then
  INJMARK="INJECTMARK15_$RANDOM"
  LOG_C=$(wc -l < "$LOG_FILE")
  pane_log "[pi/item3-C] before inject (bash sleep, will queue an inject then abort)"
  inject_prompt "Your VERY FIRST action must be the bash tool running exactly this command: sleep 30. Do NOT write any text before the tool call."
  [ "$(wait_for_marker "$LOG_C" 'Raw hook payload \[PreToolUse\]:' "$TIMEOUT")" = true ] \
    || fail "pi Item3-C: PreToolUse never fired (model did not call the tool) — cannot set up the busy window"

  # Queue an inject WHILE pi is busy (running the sleep). After Item 2 a busy pi reads running -> the inject is
  # queued (safeInjectText: CC busy, queued ...), NOT delivered mid-run.
  ./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name "$PI_SESSION_NAME" --port "$TEST_PORT" --from e2e-test --text "$INJMARK" 2>&1 | sed 's/^/    send: /' || true
  [ "$(wait_for_marker "$LOG_C" "CC busy, queued for target=.*${INJMARK}" 20)" = true ] \
    || fail "pi Item3-C: the inject was NOT queued while pi was busy (Item 2 busy-gate not holding) — cannot test the interlock"
  QUEUE_LN=$(tail -n +"$((LOG_C + 1))" "$LOG_FILE" | awk "/CC busy, queued for target=.*${INJMARK}/{print NR; exit}")
  pass "pi Item3-C: the inject was queued while the pi run was busy (not delivered mid-run)"

  # Abort the tool -> agent_idle -> busy clears -> the idle-triggered flush delivers the queued inject.
  pane_log "[pi/item3-C] queued; aborting the tool"
  esc_twice
  [ "$(wait_for_marker "$LOG_C" 'Raw hook payload \[agent_idle\]:' 90)" = true ] \
    || fail "pi Item3-C: run never settled with agent_idle after the abort — cannot test the interlock"
  # After the abort clears busy, the queued inject is flushed (idle-triggered). Wait for the delivery.
  [ "$(wait_for_marker "$LOG_C" "flushInjectQueue: .*(merging|inject completed):? " 60)" = true ] \
    || fail "pi Item3-C: the queued inject was never delivered after the abort (idle flush did not fire) — interlock broken"
  sleep 1
  SLICE_C=$(tail -n +"$((LOG_C + 1))" "$LOG_FILE")
  IDLE_LN=$(printf '%s\n' "$SLICE_C" | awk '/state: event=agent_idle .*state=idle/{print NR; exit}')
  # fall back to the agent_idle payload line if the typing-state line is in TYPING_LOG_FILE, not bot.log
  [ -n "$IDLE_LN" ] || IDLE_LN=$(printf '%s\n' "$SLICE_C" | awk '/Raw hook payload \[agent_idle\]:/{print NR; exit}')
  DELIVER_LN=$(printf '%s\n' "$SLICE_C" | awk '/flushInjectQueue: .*(merging|inject completed)/{print NR; exit}')
  echo "  DEBUG-C IDLE_LN=$IDLE_LN DELIVER_LN=$DELIVER_LN QUEUE_LN=$QUEUE_LN"
  if [ -n "$IDLE_LN" ] && [ -n "$DELIVER_LN" ] && [ "$DELIVER_LN" -gt "$IDLE_LN" ]; then
    pass "pi Item3-C: the queued inject was delivered AFTER the abort cleared busy (idle-triggered flush; Items 2+3 interlock)"
  else
    record_fail "pi Item3-C: the inject delivery did not follow the abort's idle (IDLE_LN=$IDLE_LN DELIVER_LN=$DELIVER_LN) — interlock ordering wrong"
  fi
fi

echo ""
echo "  pi Item3 (v17) abort-not-completed test complete."
