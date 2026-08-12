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
# rewrite this into one.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi Item1: no stale re-send on a text-less run ---"

ensure_infrastructure
start_pi "e2e-pi-11"

MARKER="ALPHA_MARKER_ONE_TWO_THREE"

# ---------- Run A: a normal prompt producing distinctive assistant text T1 = $MARKER ----------
LOG_A_BEFORE=$(wc -l < "$LOG_FILE")
pane_log "[pi/item1] Run A before inject"
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
[ "${_ps_a[1]}" -eq 0 ] && pass "pi Item1: run A delivered T1 ($MARKER streamed to TG)" \
  || fail "pi Item1: run A did not deliver T1 ($MARKER absent from streamed content) — cannot test re-send"

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

# Abort run B before the tool returns (Escape) — pi aborts the running turn on Escape.
$TMUX_TEST send-keys -t "$E2E_SESSION" Escape
sleep 1
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
