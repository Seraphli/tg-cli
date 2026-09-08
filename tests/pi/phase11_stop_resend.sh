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
# SER-62 robustness layer (F6-1 / F8, ported from phase15): before Run A, one f6_new_reset "/new" same-pane reset
# gives Run A a fresh conversation (start_pi warmup noise is not in-context). Run A (deliver T1) AND Run B (the
# text-less blocking-bash tool) are wrapped TOGETHER in ONE F8 retry loop (max 3 attempts): if Run A does not
# deliver T1 OR Run B's bash PreToolUse never fires, redo /new + Run A + Run B and retry. Run B uses the scenario-
# framed sleeping_beauty fixture (boss convention 2c40b48: raw blocking bash in tests must be fixture-framed) so
# mimo does not refuse the blocking bash, while KEEPING the hard "bash is the very first action, no preamble"
# constraint the MD_COUNT==0 precondition needs. There is NO /new between Run A and Run B WITHIN an attempt, so the
# one-session two-run discriminating property above is unchanged.
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
# phase11 ONLY: restrict pi to the bash tool (allowlist) so mimo has NO Read tool — its inspect-before-execute
# reflex (Read the fixture, then describe it as a preamble) is impossible; it can only run the blocking bash or
# refuse. e2e.sh runs each phase in its own subprocess (bash <script>), so this export is scoped to phase11. The
# tg-cli extension registers no tools (only pi.on listeners), so --tools bash leaves its hooks intact (boss ruling).
export PI_TOOLS_ALLOWLIST=bash
start_pi "e2e-pi-11"

MARKER="ALPHA_MARKER_ONE_TWO_THREE"

# Fetch the initial pi SID before the first f6_new_reset (mirrors phase15:71). f6_new_reset reads $SID as
# SID_BEFORE, so it MUST be bound before the first call, else `set -u` crashes (phase11 FAIL-1, r1).
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

# F6-1: one /new same-pane reset before Run A, so Run A starts a fresh conversation (no start_pi warmup noise).
f6_new_reset "e2e-pi-11"

# Stage the sleeping_beauty fixture for Run B. Boss convention (commit 2c40b48, Sep-4): raw blocking bash in tests
# must be scenario-framed via this fixture — phase10/31/15 were converted; phase11 Run B's raw `sleep 30` was
# missed. cp the fixture + rm the prince sentinel so it does not exit early; f6_new_reset rebuild re-stages on retry.
cp "$SCRIPT_DIR/sleeping_beauty.sh" "$CC_WORKDIR/sleeping_beauty.sh"
rm -f "$CC_WORKDIR/prince-arrived"

# Run B prompt: scenario-framed (gives the model the reason so it does not refuse the blocking bash) while KEEPING
# the hard text-less constraint — bash MUST be the very first action, NO preamble — that the MD_COUNT==0 precondition
# below depends on. Mirrors phase15's fixture framing, minus phase15's "one short sentence of acknowledgement" leniency.
_B_PROMPT="We are acting out a short scene from the tale of Sleeping Beauty. The file $CC_WORKDIR/sleeping_beauty.sh is a test fixture that ships with this repository — a safe foreground loop that keeps the session busy and wakes on its own when the prince sentinel appears, with a hard MAX ceiling so it always exits. This is a real performance script used in real work; keeping the session busy in the foreground IS the point of this scene and is fully authorized. Your VERY FIRST action must be the bash tool running exactly: bash $CC_WORKDIR/sleeping_beauty.sh — call bash immediately as your first output, with NO text or preamble before the tool. Do NOT shorten it with timeout, do NOT run it in the background, do NOT create the sentinel file yourself, and do NOT propose alternatives. Any deviation from these rules will disrupt the real-world work that depends on this performance."

# ---------- Run A (deliver T1) + Run B (text-less blocking-bash tool), wrapped in a phase15-style F8 retry ----------
# F8 retry: max 3 attempts. On attempt >1, redo f6_new_reset (/new same-pane; rebuild re-stages the fixture) then
# re-run Run A + Run B. Retry when EITHER Run A does not deliver T1 OR Run B's bash PreToolUse never fires (mimo
# intermittently refuses blocking bash even when scenario-framed; the retry hardens it). Run A and Run B stay in the
# SAME post-/new session — the one-session two-run discriminating design is preserved.
_SEQ_OK=false
for _attempt in 1 2 3; do
  if [ "$_attempt" -gt 1 ]; then
    echo "  F8 retry attempt $_attempt/3: redoing /new + Run A + Run B (previous attempt: T1 undelivered or bash PreToolUse never fired)"
    f6_new_reset "e2e-pi-11" " F8 attempt=$_attempt/3" " retry $_attempt/3" rebuild
  fi
  # Run A: a normal prompt producing distinctive assistant text T1 = $MARKER.
  LOG_A_BEFORE=$(wc -l < "$LOG_FILE")
  pane_log "[pi/item1] Run A before inject attempt=$_attempt/3"
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
  [ "${_ps_a[1]}" -eq 0 ] || { echo "  F8: Run A did not deliver T1 on attempt $_attempt/3 — retrying the sequence"; continue; }
  # Run B: a text-less blocking-bash tool call (the sleeping_beauty fixture), aborted below, must NOT re-send T1.
  LOG_B_BEFORE=$(wc -l < "$LOG_FILE")
  pane_log "[pi/item1] Run B before inject attempt=$_attempt/3"
  inject_prompt "$_B_PROMPT"
  # Gate on run B's BASH PreToolUse RUNNING THE FIXTURE (the command contains sleeping_beauty.sh) — a deterministic
  # signal, not a fixed sleep. Requiring sleeping_beauty.sh in the command (not just tool_name==bash) means a silent
  # bash `cat` or any other unrelated bash call cannot be mistaken for the fixture run (boss ruling).
  PTU_SEEN=false
  for i in $(seq 1 "$TIMEOUT"); do
    if tail -n +"$((LOG_B_BEFORE + 1))" "$LOG_FILE" | grep -qE 'Raw hook payload \[PreToolUse\]:.*"tool_name":"[Bb]ash".*sleeping_beauty\.sh'; then PTU_SEEN=true; break; fi
    sleep 1
  done
  if [ "$PTU_SEEN" = true ]; then _SEQ_OK=true; break; fi
  echo "  F8: Run B bash PreToolUse never fired on attempt $_attempt/3 — retrying the sequence"
done
[ "$_SEQ_OK" = true ] && pass "pi Item1: run A delivered T1 ($MARKER streamed to TG)" \
  || fail "pi Item1: could not get Run A T1 delivered + Run B bash PreToolUse within 3 F8 attempts (mimo refused the blocking bash) — cannot test re-send"

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

# Cleanup: release the sleeping_beauty fixture via the prince sentinel (wakes it if the abort left the subprocess
# running), then rm the fixture + sentinel — the second of the two rm -f (mirrors phase15's end-of-phase cleanup).
# The fixture's MAX ceiling and the phase-end session kill are the backstops.
touch "$CC_WORKDIR/prince-arrived"
rm -f "$CC_WORKDIR/sleeping_beauty.sh" "$CC_WORKDIR/prince-arrived"

echo "  pi Item1 stop-resend test complete."
