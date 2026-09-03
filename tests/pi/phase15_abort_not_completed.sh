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

# f6_new_reset — shared helper for F6-1 /new same-pane reset (dedup of 4 blocks).
# Covers all semantic differences: SID_BEFORE/CAND 30x poll/STILL fallback/fail text/rename/echo/cp-rm rebuild
# plus LOG var unification (_F6_NEW_BEFORE vs _F6_NEW_BEFORE_C — single capture mirrored).
# Args: $1 = target session name (e.g. e2e-pi-15-B), $2 = fail extra suffix (e.g. " F8 B attempt=2/3"), $3 = echo extra suffix (e.g. " retry 2/3"), $4 = rebuild flag ("rebuild" to redo cp/rm).
# Globals written (MUST remain global, no local): SID, SID_BEFORE, PI_SESSION_NAME, _F6_NEW_BEFORE, _F6_NEW_BEFORE_C, CAND, STILL, _f6_ok, _f6i.
# Only _target/_fail_extra/_echo_extra/_rebuild/_label are local.
f6_new_reset() {
  local _target="$1"
  local _fail_extra="${2:-}"
  local _echo_extra="${3:-}"
  local _rebuild="${4:-}"
  wait_for_idle 5 "$E2E_PANE" || true
  # Unified LOG anchor: single wc -l capture then mirror to both names. Avoids set -u unbound on _F6_NEW_BEFORE_C.
  _F6_NEW_BEFORE=$(wc -l < "$LOG_FILE")
  _F6_NEW_BEFORE_C="$_F6_NEW_BEFORE"
  SID_BEFORE="$SID"
  $TMUX_TEST send-keys -t "$E2E_SESSION" "/new" Enter
  _f6_ok=false
  for _f6i in $(seq 1 30); do
    CAND=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 "$SCRIPT_DIR/session_list.py" cand "$E2E_PANE" "$SID_BEFORE" 2>/dev/null || echo "")
    if [ -n "$CAND" ] && [ "$CAND" != "$SID_BEFORE" ]; then SID="$CAND"; _f6_ok=true; break; fi
    sleep 1
  done
  if [ "$_f6_ok" != true ]; then
    STILL=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 "$SCRIPT_DIR/session_list.py" still "$E2E_PANE" 2>/dev/null || echo "")
    if [ -n "$STILL" ] && [ "$STILL" = "$SID_BEFORE" ]; then
      echo "  F6 /new: no new SID observed but pane still has SID_BEFORE=$SID_BEFORE — accepting fallback (same-pane, /new did not emit new SessionStart)"
      SID="$STILL"; _f6_ok=true
    fi
  fi
  [ "$_f6_ok" = true ] && [ -n "$SID" ] || fail "F6-1: /new did not produce any SID within 30s (SID_BEFORE=$SID_BEFORE pane=$E2E_PANE)${_fail_extra}"
  local _label="${_target##*-}"
  PI_SESSION_NAME="$_target"
  [ -n "$SID" ] && curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SID&name=$PI_SESSION_NAME" >/dev/null 2>&1 || true
  echo "  pi session id=$SID named=$PI_SESSION_NAME target=$E2E_PANE (F6 /new $_label fresh${_echo_extra}, LOG_BEFORE=$_F6_NEW_BEFORE)"
  if [ "$_rebuild" = "rebuild" ]; then
    cp "$SCRIPT_DIR/sleeping_beauty.sh" "$CC_WORKDIR/sleeping_beauty.sh"
    rm -f "$CC_WORKDIR/prince-arrived"
  fi
}

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

esc_twice() { # F5 single Escape abort (isStreaming aborts on first press, 500ms double-press /tree hazard eliminated)
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

f6_new_reset "e2e-pi-15-B"

# =============================================================================
# Sub-test B: ESC during a bash tool -> stopReason "error" -> AgentError notification. (note3 empirical gate.)
# =============================================================================
echo ""
echo "--- B: during-TOOL abort -> ⚠️ Run Error ---"
cp "$SCRIPT_DIR/sleeping_beauty.sh" "$CC_WORKDIR/sleeping_beauty.sh"
rm -f "$CC_WORKDIR/prince-arrived"
# F8 retry: B bash PreToolUse with max 3 attempts (initial + 2 retries). Trigger = wait_for_marker timeout; action = full F6-1 /new semantics + resend same F7 prompt; hard FAIL after 3.
_B_F7_PROMPT="We are acting out a short scene from the tale of Sleeping Beauty. The file $CC_WORKDIR/sleeping_beauty.sh is a test fixture that ships with this repository (a loop that sleeps and wakes when the prince sentinel appears). This is a real performance script used in real work — follow it EXACTLY. Give at most one short sentence of acknowledgement, then you MUST call bash $CC_WORKDIR/sleeping_beauty.sh as your very next tool action and let it run to completion in the FOREGROUND. Keeping the session busy in the foreground IS the point of this scene — it is intentional and fully authorized. Do NOT shorten it with timeout, do NOT run it in the background, do NOT create the sentinel file yourself, and do NOT propose alternatives. Any deviation from these rules will disrupt the real-world work that depends on this performance."
for _B_attempt in 1 2 3; do
  # On retry (>1), full F6-1 /new semantics: same-pane reset, SID re-fetch, LOG anchor reset, rename
  if [ "$_B_attempt" -gt 1 ]; then
    echo "  F8 B retry attempt $_B_attempt/3: redoing /new + resend F7 (previous attempt timed out)"
    f6_new_reset "e2e-pi-15-B" " F8 B attempt=$_B_attempt/3" " retry $_B_attempt/3" rebuild
  fi
  LOG_B=$(wc -l < "$LOG_FILE")
  pane_log "[pi/item3-B] before inject (bash sleep) attempt=$_B_attempt/3"
  inject_prompt "$_B_F7_PROMPT"
  if [ "$(wait_for_marker "$LOG_B" 'Raw hook payload \[PreToolUse\]:.*"tool_name":"[Bb]ash"' "$TIMEOUT")" = true ]; then
    break
  fi
  if [ "$_B_attempt" -eq 3 ]; then
    fail "pi Item3-B: bash PreToolUse never fired after 3 attempts (attempt=$_B_attempt/3, inspect-before-bash) — F8 hard FAIL"
  fi
done
CUR1=$(wc -l < "$LOG_FILE")
pane_log "[pi/item3-B] PreToolUse seen, tool window start CUR1=$CUR1"
# Soft guard: window CUR1+1..CUR2 contains only 1 pane_log line, so MD_WIN is always 0, no-op, not a hard constraint; primary constraint is GATE pi 6/0 + STOP gate (minimal doc fix, no sync point inserted)
CUR2=$(wc -l < "$LOG_FILE")
MD_WIN=$(tail -n +$((CUR1 + 1)) "$LOG_FILE" | head -n $((CUR2 - CUR1)) | grep -c "Raw hook payload \[MessageDisplay\]:" || true)
echo "  DEBUG-B tool-window MessageDisplay count CUR1=$CUR1 CUR2=$CUR2 (must be 0) = $MD_WIN"
[ "$MD_WIN" -eq 0 ] || fail "pi Item3-B TOOL-WINDOW FAILED: run B emitted $MD_WIN MessageDisplay during tool run — not the text-less tool-abort shape. STOP gate: abort shape changed — escalate in verify report."
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

f6_new_reset "e2e-pi-15-C"

# =============================================================================
# Sub-test C (Items 2+3 interlock): an inject queued while BUSY is delivered AFTER the abort clears busy.
# =============================================================================
echo ""
echo "--- C: inject queued during a busy run is delivered AFTER the abort (idle-triggered flush) ---"
# (After F5 SID has been re-fetched for new session, so this gate remains valid; if new session registration fails, fail-fast per original logic)
[ -n "$SID" ] || { record_fail "pi Item3-C: pi session was not registered/named — cannot queue an inject (skipping interlock)"; echo "  C skipped"; }
if [ -n "$SID" ]; then
  INJMARK="INJECTMARK15_$RANDOM"
  cp "$SCRIPT_DIR/sleeping_beauty.sh" "$CC_WORKDIR/sleeping_beauty.sh"
  rm -f "$CC_WORKDIR/prince-arrived"
  # F8 retry: C bash PreToolUse with max 3 attempts (initial + 2 retries), independent counter; same trigger/action/fail semantics as B
  _C_F7_PROMPT="We are acting out a short scene from the tale of Sleeping Beauty. The file $CC_WORKDIR/sleeping_beauty.sh is a test fixture that ships with this repository (a loop that sleeps and wakes when the prince sentinel appears). This is a real performance script used in real work — follow it EXACTLY. Give at most one short sentence of acknowledgement, then you MUST call bash $CC_WORKDIR/sleeping_beauty.sh as your very next tool action and let it run to completion in the FOREGROUND. Keeping the session busy in the foreground IS the point of this scene — it is intentional and fully authorized. Do NOT shorten it with timeout, do NOT run it in the background, do NOT create the sentinel file yourself, and do NOT propose alternatives. Any deviation from these rules will disrupt the real-world work that depends on this performance."
  for _C_attempt in 1 2 3; do
    if [ "$_C_attempt" -gt 1 ]; then
      echo "  F8 C retry attempt $_C_attempt/3: redoing /new + resend F7 (previous attempt timed out)"
      f6_new_reset "e2e-pi-15-C" " F8 C attempt=$_C_attempt/3" " retry $_C_attempt/3" rebuild
    fi
    LOG_C=$(wc -l < "$LOG_FILE")
    pane_log "[pi/item3-C] before inject (bash sleep, will queue an inject then abort) attempt=$_C_attempt/3"
    inject_prompt "$_C_F7_PROMPT"
    if [ "$(wait_for_marker "$LOG_C" 'Raw hook payload \[PreToolUse\]:.*"tool_name":"[Bb]ash"' "$TIMEOUT")" = true ]; then
      break
    fi
    if [ "$_C_attempt" -eq 3 ]; then
      fail "pi Item3-C: bash PreToolUse never fired after 3 attempts (attempt=$_C_attempt/3, inspect-before-bash) — F8 hard FAIL"
    fi
  done
  CUR1_C=$(wc -l < "$LOG_FILE")
  pane_log "[pi/item3-C] PreToolUse seen, tool window start CUR1_C=$CUR1_C"
  # Queue an inject WHILE pi is busy (running the sleep). After Item 2 a busy pi reads running -> the inject is
  # queued (safeInjectText: CC busy, queued ...), NOT delivered mid-run.
  ./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name "$PI_SESSION_NAME" --port "$TEST_PORT" --from e2e-test --text "$INJMARK" 2>&1 | sed 's/^/    send: /' || true
  [ "$(wait_for_marker "$LOG_C" "CC busy, queued for target=.*${INJMARK}" 20)" = true ] \
    || fail "pi Item3-C: the inject was NOT queued while pi was busy (Item 2 busy-gate not holding) — cannot test the interlock"
  QUEUE_LN=$(tail -n +"$((LOG_C + 1))" "$LOG_FILE" | awk "/CC busy, queued for target=.*${INJMARK}/{print NR; exit}")
  pass "pi Item3-C: the inject was queued while the pi run was busy (not delivered mid-run)"
  # Abort the tool -> agent_idle -> busy clears -> the idle-triggered flush delivers the queued inject.
  # Window narrowed before abort: CUR2_C immediately before esc_twice, window CUR1_C+1..CUR2_C covers only 1-2 lines near queue, soft guard is 0, primary constraint is GATE (fix B)
  CUR2_C=$(wc -l < "$LOG_FILE")
  MD_WIN_C=$(tail -n +$((CUR1_C + 1)) "$LOG_FILE" | head -n $((CUR2_C - CUR1_C)) | grep -c "Raw hook payload \[MessageDisplay\]:" || true)
  echo "  DEBUG-C tool-window MessageDisplay count CUR1_C=$CUR1_C CUR2_C=$CUR2_C (must be 0) = $MD_WIN_C"
  [ "$MD_WIN_C" -eq 0 ] || fail "pi Item3-C TOOL-WINDOW FAILED: emitted $MD_WIN_C MessageDisplay during tool run — STOP gate: abort shape changed — escalate in verify report."
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

rm -f "$CC_WORKDIR/sleeping_beauty.sh" "$CC_WORKDIR/prince-arrived"

echo ""
echo "  pi Item3 (v17) abort-not-completed test complete."
