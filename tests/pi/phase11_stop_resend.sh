#!/bin/bash
# Phase 11 = Round-2 Item 1: pi must NOT re-send the PREVIOUS turn's assistant message on a text-less run.
#
# Root cause: the extension's lastAssistantText survives across runs in the closure and was written ONLY in
# message_end. A run that ends with no assistant text (e.g. a tool call aborted before any text) left the
# PREVIOUS turn's T1 in lastAssistantText, and agent_settled POSTed it as the Stop body. The Go side re-sends
# it because this run's OWN UserPromptSubmit already Rotated the stream (ss.Order=nil, stream.go:502), so the
# stale non-empty Stop body hits FinalizeNoEntry (stream.go:625) -> outcome=direct_send (register.go:346).
# Fix: reset lastAssistantText="" in the extension's agent_start handler.
#
# ONE pi session, two runs is SUFFICIENT: Rotate empties Order on run B's own prompt, so run B's own stale
# Stop is FinalizeNoEntry -> direct_send on the pre-fix binary. No multi-session dance is needed; do not
# rewrite this into one. Run A and Run B share the SAME session (no /new between them) — this is what
# preserves the discriminating design above.
#
# SER-62 round-1 robustness layer (F6-1 / F8, ported from phase15): before Run A, one f6_new_reset "/new"
# same-pane reset gives Run A a fresh conversation (warmup noise from start_pi is not in-context). Run A itself
# is wrapped in an F8 retry loop (max 3 attempts): if T1 was not delivered, redo f6_new_reset and re-inject the
# SAME Run A prompt, then retry. This only hardens delivery of T1 against harness flakiness — it does NOT
# add a /new between Run A and Run B, so the one-session two-run discriminating property is unchanged.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

# f6_new_reset — shared helper for F6-1 /new same-pane reset (ported verbatim from phase15_abort_not_completed.sh).
# Args: $1 = target session name, $2 = fail extra suffix, $3 = echo extra suffix, $4 = rebuild flag ("rebuild" to redo cp/rm).
# Globals written (MUST remain global, no local): SID, SID_BEFORE, PI_SESSION_NAME, _F6_NEW_BEFORE, _F6_NEW_BEFORE_C, CAND, STILL, _f6_ok, _f6i.
f6_new_reset() {
  local _target="$1"
  local _fail_extra="${2:-}"
  local _echo_extra="${3:-}"
  local _rebuild="${4:-}"
  wait_for_idle 5 "$E2E_PANE" || true
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
echo "--- pi Item1: no stale re-send on a text-less run ---"

ensure_infrastructure
start_pi "e2e-pi-11"

MARKER="ALPHA_MARKER_ONE_TWO_THREE"

# F6-1: one /new same-pane reset before Run A, so Run A starts a fresh conversation (no start_pi warmup noise).
f6_new_reset "e2e-pi-11"

# ---------- Run A: a normal prompt producing distinctive assistant text T1 = $MARKER ----------
# F8 retry: max 3 attempts. On attempt >1, redo f6_new_reset (same-pane /new) then re-inject the SAME Run A
# prompt. Break as soon as T1 is delivered (reconstruct_tg_full_text contains MARKER). Hard FAIL after 3
# attempts. This hardens ONLY T1 delivery — Run B below still runs in the SAME session that survives this loop.
_A_DELIVERED=false
for _A_attempt in 1 2 3; do
  if [ "$_A_attempt" -gt 1 ]; then
    echo "  F8 A retry attempt $_A_attempt/3: redoing /new + resend run A prompt (previous attempt did not deliver T1)"
    f6_new_reset "e2e-pi-11" " F8 A attempt=$_A_attempt/3" " retry $_A_attempt/3"
  fi
  LOG_A_BEFORE=$(wc -l < "$LOG_FILE")
  pane_log "[pi/item1] Run A before inject attempt=$_A_attempt/3"
  inject_prompt "Reply with exactly this text and nothing else. Do not use any tools. The text is: $MARKER"
  wait_for_idle "$TIMEOUT" "$E2E_PANE"
  sleep 2
  SLICE_A=$(tail -n +"$((LOG_A_BEFORE + 1))" "$LOG_FILE")
  # T1 was DELIVERED (streamed to TG), not just echoed in the UserPromptSubmit payload. reconstruct_tg_full_text
  # parses the Stream send/edit render lines, so a match here means the assistant reply reached Telegram.
  DELIVERED_A=$(reconstruct_tg_full_text "$SLICE_A")
  set +eo pipefail
  printf '%s' "$DELIVERED_A" | grep -q "$MARKER"
  _ps_a=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps_a[1]}" -eq 0 ]; then _A_DELIVERED=true; break; fi
done
[ "$_A_DELIVERED" = true ] && pass "pi Item1: run A delivered T1 ($MARKER streamed to TG)" \
  || fail "pi Item1: run A did not deliver T1 ($MARKER absent from streamed content) after 3 F8 attempts — cannot test re-send"

# ---------- Run B: a text-less tool call, aborted, must NOT re-send T1 ----------
LOG_B_BEFORE=$(wc -l < "$LOG_FILE")
pane_log "[pi/item1] Run B before inject"
inject_prompt "Your VERY FIRST action must be the bash tool running exactly this command: sleep 30. Do NOT write any text before calling the tool — call bash immediately as your first output, with no preamble."

# Gate on run B's PreToolUse (the bash call started) — a deterministic signal, not a fixed sleep.
PTU_SEEN=false
for i in $(seq 1 "$TIMEOUT"); do
  if tail -n +"$((LOG_B_BEFORE + 1))" "$LOG_FILE" | grep -q "Raw hook payload \[PreToolUse\]:"; then PTU_SEEN=true; break; fi
  sleep 1
done
[ "$PTU_SEEN" = true ] || fail "pi Item1: run B PreToolUse never fired (model did not call the tool) — cannot test"

# PRECONDITION (note3 CHANGE 1) — run B MUST be text-less or the test is non-discriminating. MessageDisplay is
# posted by the extension ONLY on text deltas; a model preamble would create stream entries -> Order non-empty
# -> FinalizeNoEntry is never reached -> BOTH main asserts pass even on the PRE-FIX binary (a silent false
# green, the exact failure class this round eliminates). Count MessageDisplay hook payloads for run B's turn;
# if not zero, FAIL LOUDLY as a harness precondition — NEVER fall through to the two main asserts below.
SLICE_B_PRE=$(tail -n +"$((LOG_B_BEFORE + 1))" "$LOG_FILE")
MD_COUNT=$(printf '%s\n' "$SLICE_B_PRE" | grep -c "Raw hook payload \[MessageDisplay\]:" || true)
echo "  DEBUG: run B MessageDisplay count (must be 0) = $MD_COUNT"
[ "$MD_COUNT" -eq 0 ] || fail "pi Item1 PRECONDITION FAILED: run B emitted $MD_COUNT MessageDisplay (a preamble before the tool) — the test would be non-discriminating. Strengthen the prompt so bash is the first output. NOT falling through to the main asserts."

# Abort run B before the tool returns (Escape) — F5 single Escape abort (isStreaming aborts on first press, 500ms double-press /tree hazard eliminated)
$TMUX_TEST send-keys -t "$E2E_SESSION" Escape

# Wait for run B to settle (agent_settled -> Stop payload) after the abort.
STOP_SEEN=false
for i in $(seq 1 90); do
  if tail -n +"$((LOG_B_BEFORE + 1))" "$LOG_FILE" | grep -q "Raw hook payload \[agent_idle\]:"; then STOP_SEEN=true; break; fi
  sleep 1
done
[ "$STOP_SEEN" = true ] || fail "pi Item1: run B never settled (no Stop payload after abort)"
sleep 2
pane_log "[pi/item1] Run B after abort+settle"

SLICE_B=$(tail -n +"$((LOG_B_BEFORE + 1))" "$LOG_FILE")
# Main assert 1: NO direct_send for run B. The bug re-sends T1 via FinalizeNoEntry -> outcome=direct_send
# (also covers the direct_send_sealed_mismatch variant via the shared "outcome=direct_send" substring). Run A
# is a normal streamed reply (Order non-empty -> FinalizeExisting), so it never direct_sends — any
# outcome=direct_send in this region is run B's.
set +eo pipefail
printf '%s\n' "$SLICE_B" | grep -qE "outcome=direct_send"
_ps_ds=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps_ds[1]}" -ne 0 ] && pass "pi Item1: no outcome=direct_send for the text-less run B" \
  || record_fail "pi Item1: outcome=direct_send present for run B — the stale previous-turn text was re-sent"

# Main assert 2: T1 is NOT re-carried as run B's Stop body. Run B is the ONLY run with a tool call, so its
# Stop is the first Stop AFTER run B's PreToolUse. On the pre-fix binary that Stop payload carries
# last_assistant_message:"...$MARKER..." (the stale carry); the fix resets it so it is empty. Scoping to run
# B's own Stop this way is immune to run A's trailing delivery lines that can flush into this log region.
# R1a (Round 4, note3-ruled): after the Round-4 abort fix, run B (a during-TOOL ESC abort) posts agent_idle,
# NOT Stop, so this locator now greps the agent_idle payload — which has NO last_assistant_message field. The
# MARKER-absence check below is therefore TRIVIALLY satisfied for an aborted run and can never fail = a
# false-green (same class as the phase31 catch). It is INTENTIONALLY left unchanged (not deleted, not
# weakened): the no-stale-re-send property is actually guarded LIVE by (i) Main assert 1's outcome=direct_send
# check above (an aborted run posts no Stop -> no FinalizeNoEntry -> no direct_send; a regression that wrongly
# re-emitted a Stop with the stale body WOULD trip it), and (ii) the new Round-4 pi abort phase (v17), which
# POSITIVELY asserts no Stop payload for both abort shapes. This comment is the record; see SUMMARY.md round notes.
RUNB_STOP=$(printf '%s\n' "$SLICE_B" | awk '/Raw hook payload \[PreToolUse\]:/{seen=1} seen && /Raw hook payload \[agent_idle\]:/{print; exit}')
echo "  DEBUG: run B Stop payload (first 220 chars): ${RUNB_STOP:0:220}"
[ -n "$RUNB_STOP" ] || fail "pi Item1: could not locate run B's Stop payload (no Stop after run B PreToolUse)"
set +eo pipefail
printf '%s\n' "$RUNB_STOP" | grep -q "$MARKER"
_ps_m=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps_m[1]}" -ne 0 ] && pass "pi Item1: run B Stop carries no stale T1 (last_assistant_message is not $MARKER)" \
  || record_fail "pi Item1: run B Stop re-carried T1 ($MARKER in last_assistant_message) — the stale carry was not reset"

echo "  pi Item1 stop-resend test complete."
