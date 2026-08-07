#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Post-Stop late MessageDisplay redesign (commit 18) ---"

ensure_infrastructure

# Deterministic + fully synthetic (no CC dependency): drive the post-Stop late-MD state machine directly via
# /hook/Stop + /hook/MessageDisplay. ResolveChat falls back to the paired chat for any tmux_target, so a
# synthetic target delivers to the E2E chat. Each sub-test uses a UNIQUE session_id/target so the Hook FIFOs
# do not cross-talk. Assertions grep the bot log for the new post-Stop markers (filtered by session_id).

CWD_NOW="$(pwd)"

post_hook() {
  local event="$1"
  local payload="$2"
  curl -s -X POST "http://127.0.0.1:${TEST_PORT}/hook/${event}" \
    -H "Content-Type: application/json" -d "$payload" > /dev/null 2>&1 || true
}

# wait_for_log polls the bot log slice (from $1) for a pattern ($2) up to $3 seconds.
wait_for_log() {
  local from="$1" pat="$2" timeout="$3" elapsed=0
  while [ "$elapsed" -lt "$timeout" ]; do
    if tail -n +"$((from + 1))" "$LOG_FILE" | grep -qE "$pat"; then
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  return 1
}

PHASE_LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# =============================================================================
# TC1: total-inversion — the whole final message arrives AFTER Stop as fragments that verbatim-match the Stop
# body. Expect: Stop delivers the full body once (FinalizeNoEntry direct-send), the late fragment is classified
# STOP-COPY and dropped (no duplicate).
# =============================================================================
echo ""
echo "--- TC1: total-inversion (Stop direct-send + stop-copy drop, no duplicate) ---"
SID1="poststop-inv-$RANDOM"
TGT1="%97@/tmp/tmux-1000/tg-cli-test-poststop1"
BODY1="Inversion body paragraph one.\n\nInversion body paragraph two."
LOG_BEFORE_TC1=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
# Stop first (no prior MessageDisplay) → FinalizeNoEntry → direct-send the full body + mark Stopped.
post_hook Stop "{\"session_id\":\"$SID1\",\"tmux_target\":\"$TGT1\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"Stop\",\"last_assistant_message\":\"$BODY1\"}"
wait_for_log "$LOG_BEFORE_TC1" "Stop terminal: outcome=direct_send session=$SID1" 15 || true
sleep 1
# The inverted final message arrives late as a fragment == the Stop body → STOP-COPY → dropped.
post_hook MessageDisplay "{\"session_id\":\"$SID1\",\"tmux_target\":\"$TGT1\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"MessageDisplay\",\"message_id\":\"mInv\",\"turn_id\":\"tInv\",\"prompt_id\":\"pInv\",\"index\":0,\"delta\":\"$BODY1\",\"final\":true}"
wait_for_log "$LOG_BEFORE_TC1" "post-stop late MD dropped \(stop-copy\): session=$SID1 message_id=mInv" 15 || true

if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -q "Stop terminal: outcome=direct_send session=$SID1"; then
  pass "TC1: FinalizeNoEntry terminal direct-send delivered the Stop body (functional)"
else
  fail "TC1: FinalizeNoEntry terminal direct-send not found for $SID1"
fi
if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -q "post-stop late MD dropped (stop-copy): session=$SID1 message_id=mInv"; then
  pass "TC1: late fragment matching the Stop body dropped as STOP-COPY (no duplicate)"
else
  fail "TC1: STOP-COPY drop marker not found for $SID1 message_id=mInv"
fi
if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -q "post-stop late MD accepted (stream): session=$SID1 message_id=mInv"; then
  fail "TC1: STOP-COPY fragment was WRONGLY accepted as a stream (duplicate delivery)"
else
  pass "TC1: STOP-COPY fragment never streamed (exactly one delivery)"
fi

# =============================================================================
# TC2: genuinely-new post-Stop multi-delta message → streamed in full and relabeled ✅ on completion.
# =============================================================================
echo ""
echo "--- TC2: genuinely-new post-Stop multi-delta streamed in full ---"
SID2="poststop-new-$RANDOM"
TGT2="%97@/tmp/tmux-1000/tg-cli-test-poststop2"
LOG_BEFORE_TC2=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
post_hook Stop "{\"session_id\":\"$SID2\",\"tmux_target\":\"$TGT2\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"Stop\",\"last_assistant_message\":\"Stop authoritative body two.\"}"
wait_for_log "$LOG_BEFORE_TC2" "Stop terminal: outcome=direct_send session=$SID2" 15 || true
sleep 1
post_hook MessageDisplay "{\"session_id\":\"$SID2\",\"tmux_target\":\"$TGT2\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"MessageDisplay\",\"message_id\":\"mNew2\",\"turn_id\":\"tNew2\",\"prompt_id\":\"pNew2\",\"index\":0,\"delta\":\"POSTSTOP_STREAM_MARKER fresh part one \",\"final\":false}"
sleep 1
post_hook MessageDisplay "{\"session_id\":\"$SID2\",\"tmux_target\":\"$TGT2\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"MessageDisplay\",\"message_id\":\"mNew2\",\"turn_id\":\"tNew2\",\"prompt_id\":\"pNew2\",\"index\":1,\"delta\":\"and part two complete.\",\"final\":true}"
wait_for_log "$LOG_BEFORE_TC2" "Stream relabel ✅: .*message_id=mNew2" 20 || true

if tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE" | grep -q "post-stop late MD accepted (stream): session=$SID2 message_id=mNew2"; then
  pass "TC2: genuinely-new post-Stop message accepted as a stream"
else
  fail "TC2: post-stop accepted-stream marker not found for $SID2 message_id=mNew2"
fi
if tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE" | grep -qE "Stream (send|edit):.*message_id=mNew2"; then
  pass "TC2: post-Stop NEW message rendered via streaming (Stream send/edit)"
else
  fail "TC2: no Stream send/edit for the post-Stop NEW message mNew2"
fi
if tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE" | grep -q "Stream relabel ✅: .*message_id=mNew2"; then
  pass "TC2: completion boundary relabeled the post-Stop NEW message ✅"
else
  fail "TC2: no Stream relabel ✅ for mNew2 (completion boundary not reached)"
fi
if tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE" | grep -q "post-stop late MD dropped (stop-copy): session=$SID2 message_id=mNew2"; then
  fail "TC2: genuinely-new message was WRONGLY dropped as stop-copy"
else
  pass "TC2: genuinely-new message not dropped as stop-copy"
fi

# =============================================================================
# TC3: single-delta new post-Stop message → delivered exactly once.
# =============================================================================
echo ""
echo "--- TC3: single-delta new post-Stop message delivered once ---"
SID3="poststop-one-$RANDOM"
TGT3="%97@/tmp/tmux-1000/tg-cli-test-poststop3"
LOG_BEFORE_TC3=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
post_hook Stop "{\"session_id\":\"$SID3\",\"tmux_target\":\"$TGT3\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"Stop\",\"last_assistant_message\":\"Stop body three.\"}"
wait_for_log "$LOG_BEFORE_TC3" "Stop terminal: outcome=direct_send session=$SID3" 15 || true
sleep 1
post_hook MessageDisplay "{\"session_id\":\"$SID3\",\"tmux_target\":\"$TGT3\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"MessageDisplay\",\"message_id\":\"mOne\",\"turn_id\":\"tOne\",\"prompt_id\":\"pOne\",\"index\":0,\"delta\":\"POSTSTOP_SINGLE_MARKER single new post-stop message.\",\"final\":true}"
wait_for_log "$LOG_BEFORE_TC3" "Stream relabel ✅: .*message_id=mOne" 20 || true

if tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE" | grep -q "post-stop late MD accepted (stream): session=$SID3 message_id=mOne"; then
  pass "TC3: single-delta new post-Stop message accepted as a stream"
else
  fail "TC3: post-stop accepted-stream marker not found for $SID3 message_id=mOne"
fi
SEND_COUNT_TC3=$(tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE" | grep -E "Stream send:.*message_id=mOne" | grep -c "message_id=mOne" || true)
if [ "$SEND_COUNT_TC3" -eq 1 ]; then
  pass "TC3: single-delta message sent exactly once (Stream send count=$SEND_COUNT_TC3)"
else
  fail "TC3: expected exactly one Stream send for mOne, got $SEND_COUNT_TC3"
fi

# =============================================================================
# TC4: SealedMismatch terminal direct-send remains functional. A pre-tool text bubble is streamed + sealed,
# then Stop arrives with a DIFFERENT authoritative body → the sealed last entry mismatches → direct_send.
# =============================================================================
echo ""
echo "--- TC4: SealedMismatch terminal direct-send functional ---"
SID4="poststop-mism-$RANDOM"
TGT4="%97@/tmp/tmux-1000/tg-cli-test-poststop4"
LOG_BEFORE_TC4=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
# A complete pre-tool bubble (NOT stopped) → the ticker renders + seals it.
post_hook MessageDisplay "{\"session_id\":\"$SID4\",\"tmux_target\":\"$TGT4\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"MessageDisplay\",\"message_id\":\"mPre\",\"turn_id\":\"tPre\",\"prompt_id\":\"pPre\",\"index\":0,\"delta\":\"Pre-tool text bubble content.\",\"final\":true}"
# Wait for mPre to render (the render op seals a complete entry) before Stop, so the last entry IS sealed.
if wait_for_log "$LOG_BEFORE_TC4" "Stream (send|edit):.*message_id=mPre" 20; then
  sleep 2 # let the render op set Sealed under DataMu
  post_hook Stop "{\"session_id\":\"$SID4\",\"tmux_target\":\"$TGT4\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"Stop\",\"last_assistant_message\":\"Different post-tool authoritative text.\"}"
  wait_for_log "$LOG_BEFORE_TC4" "Stop terminal: outcome=direct_send_sealed_mismatch session=$SID4" 15 || true
  if tail -n +"$((LOG_BEFORE_TC4 + 1))" "$LOG_FILE" | grep -q "Stop terminal: outcome=direct_send_sealed_mismatch session=$SID4"; then
    pass "TC4: SealedMismatch terminal direct-send functional"
  else
    fail "TC4: SealedMismatch terminal direct-send marker not found for $SID4"
  fi
else
  fail "TC4: pre-tool bubble mPre never rendered — cannot exercise SealedMismatch"
fi

# =============================================================================
# TC5: no chat=0 render ops and no RICH_FALLBACK errors across the whole post-Stop phase.
# =============================================================================
echo ""
echo "--- TC5: no chat=0 / RICH_FALLBACK errors in the post-Stop phase ---"
if tail -n +"$((PHASE_LOG_BEFORE + 1))" "$LOG_FILE" | grep -E "poststop-(inv|new|one|mism)" | grep -qE "chat=0|RICH_FALLBACK"; then
  echo "  offending lines:"
  tail -n +"$((PHASE_LOG_BEFORE + 1))" "$LOG_FILE" | grep -E "poststop-(inv|new|one|mism)" | grep -E "chat=0|RICH_FALLBACK" | head -5
  fail "TC5: chat=0 or RICH_FALLBACK error found in the post-Stop phase"
else
  pass "TC5: no chat=0 / RICH_FALLBACK errors in the post-Stop phase"
fi

echo ""
echo "--- Post-Stop late MessageDisplay phase complete ---"
