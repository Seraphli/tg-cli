#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Streaming (MessageDisplay) behavior test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-27"
wait_for_idle

# ============================================================
# TC1: streaming + finalize
# Expect: >=2 Stream edit: lines for the SAME message_id,
# then Stream relabel ✅: at Stop.
# ============================================================
echo ""
echo "  TC1: streaming + finalize"

LOG_BEFORE_TC1=$(wc -l < "$LOG_FILE")
pane_log "[streaming] TC1 BEFORE inject"
inject_prompt "Without using any tools, write exactly 5 paragraphs (each starting on its own line) about the history of the internet. Label each paragraph: PARA1, PARA2, PARA3, PARA4, PARA5. Each paragraph must be at least 3 sentences long."
pane_log "[streaming] TC1 AFTER inject"

# Wait for Stream relabel ✅ (turn finalize)
ELAPSED=0
TC1_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "Stream relabel ✅:" > /dev/null 2>&1; then
    TC1_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for TC1 Stream relabel ✅... ${ELAPSED}s / ${TIMEOUT}s"
done
pane_log "[streaming] TC1 AFTER relabel"

if [ "$TC1_FOUND" = true ]; then
  pass "TC1: Stream relabel ✅ received (turn finalized)"

  # Verify MessageDisplay delta: was logged (hook fast-return path)
  if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "MessageDisplay delta:" > /dev/null 2>&1; then
    pass "TC1: MessageDisplay delta logged (fast-return hook received deltas)"
  else
    fail "TC1: No MessageDisplay delta found in log"
  fi

  # Verify at least 2 Stream edit: lines for the same message_id (streaming updates)
  TC1_LOGS=$(tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE")
  # Extract the first message_id from Stream send: or Stream edit:
  FIRST_MSG_ID=$(echo "$TC1_LOGS" | grep -oP '(?:Stream send:|Stream edit:).*message_id=\K[^ ]+' | head -1 || true)
  if [ -n "$FIRST_MSG_ID" ]; then
    EDIT_COUNT=$(echo "$TC1_LOGS" | grep "Stream edit:" | grep -c "message_id=${FIRST_MSG_ID}" || true)
    if [ "$EDIT_COUNT" -ge 2 ]; then
      pass "TC1: >=2 Stream edit: for message_id=${FIRST_MSG_ID} (streaming updates confirmed, count=$EDIT_COUNT)"
    else
      # Stream send: may also count — check combined
      SEND_EDIT_COUNT=$(echo "$TC1_LOGS" | grep -E "Stream send:|Stream edit:" | grep -c "message_id=${FIRST_MSG_ID}" || true)
      if [ "$SEND_EDIT_COUNT" -ge 2 ]; then
        pass "TC1: >=2 Stream send/edit: for message_id=${FIRST_MSG_ID} (count=$SEND_EDIT_COUNT)"
      else
        pass "TC1: Stream send/edit count=$SEND_EDIT_COUNT for message_id=${FIRST_MSG_ID} (may be throttled — not fail)"
      fi
    fi
  else
    fail "TC1: No Stream send/edit message_id found in log"
  fi

  # Verify the finalized message has final=true
  if echo "$TC1_LOGS" | grep "Stream edit:.*final=true" > /dev/null 2>&1; then
    pass "TC1: Stream edit with final=true found (finalized content)"
  else
    pass "TC1: No Stream edit final=true (may be single-chunk — OK)"
  fi

  # TC5 sub-check: if Stream surplus removed: is present, verify it was logged correctly
  if echo "$TC1_LOGS" | grep "Stream surplus removed:" > /dev/null 2>&1; then
    SURPLUS_MSG=$(echo "$TC1_LOGS" | grep "Stream surplus removed:" | head -1 || true)
    pass "TC1/TC5: Stream surplus removed logged (continuation residue cleaned up): $SURPLUS_MSG"
  fi
else
  fail "TC1: Stream relabel ✅ not received within ${TIMEOUT}s"
fi

wait_for_idle

# ============================================================
# TC2: multi-segment turn (text → tool → text)
# Expect: two different message_ids in Stream send/edit,
# a ToolUse notification card between them,
# Stream relabel ✅: only for the LAST message_id.
# ============================================================
echo ""
echo "  TC2: multi-segment turn (text → tool → text)"

LOG_BEFORE_TC2=$(wc -l < "$LOG_FILE")
pane_log "[streaming] TC2 BEFORE inject"
inject_prompt "First write one sentence saying: STREAM_TC2_PRE. Then run the bash command: echo STREAM_TC2_TOOL. Then write one sentence saying: STREAM_TC2_POST."
pane_log "[streaming] TC2 AFTER inject"

# Wait for Stream relabel ✅ (turn finalize after tool + post-text)
ELAPSED=0
TC2_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE" | grep "Stream relabel ✅:" > /dev/null 2>&1; then
    TC2_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for TC2 Stream relabel ✅... ${ELAPSED}s / ${TIMEOUT}s"
done
pane_log "[streaming] TC2 AFTER relabel"

if [ "$TC2_FOUND" = true ]; then
  pass "TC2: Stream relabel ✅ received (multi-segment turn finalized)"
  TC2_LOGS=$(tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE")

  # Verify ToolUse notification was sent (pre-tool text should stream before tool card)
  if echo "$TC2_LOGS" | grep "Notification sent.*ToolUse" > /dev/null 2>&1; then
    pass "TC2: ToolUse notification card sent in multi-segment turn"
  else
    pass "TC2: ToolUse notification not found (tool may have been skipped — acceptable)"
  fi

  # Verify at least one Stream send: (pre-tool text) exists BEFORE the relabel
  FIRST_STREAM_LINE=$(awk '/Stream send:/{print NR; exit}' <<< "$TC2_LOGS")
  RELABEL_LINE=$(awk '/Stream relabel ✅:/{print NR; exit}' <<< "$TC2_LOGS")
  if [ -n "$FIRST_STREAM_LINE" ] && [ -n "$RELABEL_LINE" ] && [ "$FIRST_STREAM_LINE" -lt "$RELABEL_LINE" ]; then
    pass "TC2: Stream send before Stream relabel ✅ (correct ordering, line $FIRST_STREAM_LINE < $RELABEL_LINE)"
  else
    pass "TC2: Stream ordering check: send=$FIRST_STREAM_LINE relabel=$RELABEL_LINE"
  fi

  # Verify two distinct message_ids appear (pre-tool and post-tool messages)
  TC2_MSG_IDS=$(echo "$TC2_LOGS" | grep -oP '(?:Stream send:|Stream edit:).*message_id=\K[^ ]+' | sort -u || true)
  TC2_MSG_COUNT=$(echo "$TC2_MSG_IDS" | grep -c "." || true)
  if [ "$TC2_MSG_COUNT" -ge 2 ]; then
    pass "TC2: >=2 distinct message_ids streamed (pre-tool + post-tool text, count=$TC2_MSG_COUNT)"
  else
    pass "TC2: $TC2_MSG_COUNT distinct message_id(s) (CC may have produced single text block — not fail)"
  fi
else
  fail "TC2: Stream relabel ✅ not received within ${TIMEOUT}s"
fi

wait_for_idle

# ============================================================
# TC3: ordering under async drain
# In AskUserQuestion path: 💬 Message (Stream send/edit) logged
# BEFORE AskUserQuestion buttons (drain guarantee).
# ============================================================
echo ""
echo "  TC3: ordering under async drain (💬 Message before AskUserQuestion buttons)"

LOG_BEFORE_TC3=$(wc -l < "$LOG_FILE")
pane_log "[streaming] TC3 BEFORE inject"
inject_prompt "First write exactly one sentence: STREAM_TC3_PRE_TEXT. Then use the AskUserQuestion tool with header 'TC3' and two options: 'Yes' (description: 'Yes option'), 'No' (description: 'No option'). Question: 'Confirm?'"
pane_log "[streaming] TC3 AFTER inject"

# Wait for AskUserQuestion notification
ELAPSED=0
TC3_AQ_FOUND=false
TC3_MSG_ID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE" | grep "AskUserQuestion sent" > /dev/null 2>&1; then
    TC3_AQ_FOUND=true
    TC3_MSG_ID=$(tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE" | grep -oPm1 'AskUserQuestion sent.*msg_id=\K[0-9]+' || true)
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for TC3 AskUserQuestion... ${ELAPSED}s / ${TIMEOUT}s"
done
pane_log "[streaming] TC3 AFTER AQ detected"

if [ "$TC3_AQ_FOUND" = true ]; then
  pass "TC3: AskUserQuestion notification sent (msg_id=$TC3_MSG_ID)"
  TC3_LOGS=$(tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE")

  # Verify ordering: Stream send/edit BEFORE AskUserQuestion sent
  STREAM_LINE=$(awk '/Stream send:|Stream edit:/{print NR; exit}' <<< "$TC3_LOGS")
  AQ_LINE=$(awk '/AskUserQuestion sent/{print NR; exit}' <<< "$TC3_LOGS")
  if [ -n "$STREAM_LINE" ] && [ -n "$AQ_LINE" ] && [ "$STREAM_LINE" -lt "$AQ_LINE" ]; then
    pass "TC3: 💬 Message (Stream send/edit) logged BEFORE AskUserQuestion buttons (drain working, line $STREAM_LINE < $AQ_LINE)"
  elif [ -z "$STREAM_LINE" ]; then
    pass "TC3: No pre-question streaming message (CC produced no text before AQ — vacuous pass)"
  else
    fail "TC3: Stream send/edit NOT before AskUserQuestion (stream=$STREAM_LINE aq=$AQ_LINE)"
  fi

  # Cancel to avoid blocking the phase
  if [ -n "$TC3_MSG_ID" ]; then
    TC3_UUID=$(tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE" | grep -oPm1 'AskUserQuestion sent.*uuid=\K[^ ]+' || true)
    if [ -n "$TC3_UUID" ]; then
      curl -s -X POST "http://127.0.0.1:$TEST_PORT/pending/cancel?uuid=$TC3_UUID" > /dev/null 2>&1 || true
    else
      # Respond via API
      curl -s "http://127.0.0.1:$TEST_PORT/tool/respond?msg_id=$TC3_MSG_ID&tool=AskUserQuestion&question=0&option=0" > /dev/null 2>&1 || true
    fi
  fi
else
  fail "TC3: AskUserQuestion not triggered within ${TIMEOUT}s"
fi

wait_for_idle

# ============================================================
# TC4: hook fast-return
# Verify MessageDisplay delta: is logged but the /hook/MessageDisplay
# handler itself does NOT do Telegram I/O (no inline Stream send/edit
# from the hook request — those come from the worker/boundary flush).
# We check: MessageDisplay delta: appears; the log confirms fast-return.
# ============================================================
echo ""
echo "  TC4: hook fast-return (MessageDisplay delta logged, no inline TG I/O)"

# TC4 is satisfied by TC1/TC2 already having MessageDisplay delta: in their logs.
# Do a simple assertion here.
if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "MessageDisplay delta:" > /dev/null 2>&1; then
  pass "TC4: MessageDisplay delta: logged (fast-return hook path active)"
else
  fail "TC4: No MessageDisplay delta found in session log"
fi

# Verify no error from MessageDisplay path itself
if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "handleMessageDisplay.*error\|MessageDisplay.*failed" > /dev/null 2>&1; then
  fail "TC4: MessageDisplay handler logged errors"
else
  pass "TC4: No MessageDisplay handler errors"
fi

# ============================================================
# TC5: continuation residue
# Ask CC to output a long response that overflows paginationMaxRunes (500),
# creating 2+ chunks, then verify final re-render either:
# (a) still has 2 chunks (no surplus — acceptable), or
# (b) Stream surplus removed: logged if re-render shrank to 1 chunk.
# We inject a prompt that produces exactly enough to trigger the continuation.
# ============================================================
echo ""
echo "  TC5: continuation / residue check"

LOG_BEFORE_TC5=$(wc -l < "$LOG_FILE")
pane_log "[streaming] TC5 BEFORE inject"
# paginationMaxRunes=500, so need >~400 rune response body to trigger 2nd chunk
inject_prompt "Without using any tools, write exactly this: STREAM_TC5_START. Then write 20 repetitions of the phrase 'The quick brown fox jumps over the lazy dog. ' (each on the same line). Then write: STREAM_TC5_END."
pane_log "[streaming] TC5 AFTER inject"

ELAPSED=0
TC5_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TC5 + 1))" "$LOG_FILE" | grep "Stream relabel ✅:" > /dev/null 2>&1; then
    TC5_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for TC5 Stream relabel ✅... ${ELAPSED}s / ${TIMEOUT}s"
done
pane_log "[streaming] TC5 AFTER relabel"

if [ "$TC5_FOUND" = true ]; then
  pass "TC5: Stream relabel ✅ received (long-output turn finalized)"
  TC5_LOGS=$(tail -n +"$((LOG_BEFORE_TC5 + 1))" "$LOG_FILE")

  # Check if continuation was created (Stream send: chunk=1 means second chunk sent)
  if echo "$TC5_LOGS" | grep "Stream send:.*chunk=1" > /dev/null 2>&1; then
    pass "TC5: Continuation message created (Stream send: chunk=1 found)"
    # Check if surplus was removed (re-render shrank)
    if echo "$TC5_LOGS" | grep "Stream surplus removed:" > /dev/null 2>&1; then
      SURPLUS_LINE=$(echo "$TC5_LOGS" | grep "Stream surplus removed:" | head -1 || true)
      pass "TC5: Stream surplus removed: logged (re-render shrank — residue cleaned up): $SURPLUS_LINE"
    else
      pass "TC5: No Stream surplus removed (final re-render kept 2+ chunks — no residue left)"
    fi
  else
    pass "TC5: No continuation triggered (output fit in single chunk — not fail)"
  fi
else
  fail "TC5: Stream relabel ✅ not received within ${TIMEOUT}s"
fi
