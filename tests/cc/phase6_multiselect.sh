#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Multi-question multiSelect AskUserQuestion (hook) ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-6"

LOG_BEFORE_MQ=$(wc -l < "$LOG_FILE")

# Send prompt that triggers multi-question AskUserQuestion
pane_log "[multiselect] BEFORE multiQ prompt"
inject_prompt "Answer this question first in 2 sentences: what is the benefit of asking multiple questions in a single AskUserQuestion tool call versus making separate calls? After answering, ask me TWO questions using AskUserQuestion tool with these exact parameters: questions array with 2 items. Question 1: header 'Preference', question 'Which do you prefer?', two options - 'Alpha' with description 'First choice', 'Beta' with description 'Second choice', multiSelect false. Question 2: header 'Colors', question 'Pick colors', three options - 'Red' with description 'Red color', 'Blue' with description 'Blue color', 'Green' with description 'Green color', multiSelect true. Call AskUserQuestion exactly once in this turn. That single call must contain exactly two items in its questions array; do not make separate calls. After it returns, do not call AskUserQuestion or any other tool again. Briefly acknowledge both answers, then stop."
pane_log "[multiselect] AFTER sending multiQ prompt"

# Wait for AskUserQuestion notification
ELAPSED=0
MQ_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_MQ" ]; then
    if tail -n +"$((LOG_BEFORE_MQ + 1))" "$LOG_FILE" | grep "AskUserQuestion sent: msg_id=" > /dev/null 2>&1; then
      MQ_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for multiQ AskUserQuestion... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[multiselect] AFTER hook notification detected"

if [ "$MQ_FOUND" = true ]; then
  pass "Multi-question AskUserQuestion notification received"

  # Rich migration (v9): multi-question AskUserQuestion is also sent via sendRichMessage.
  if tail -n +"$((LOG_BEFORE_MQ + 1))" "$LOG_FILE" | grep -q "AskUserQuestion sent:.*fmt=rich" 2>/dev/null; then
    pass "Multi-question AskUserQuestion sent via rich message path (fmt=rich)"
  else
    fail "Multi-question AskUserQuestion sent marker missing fmt=rich (expected rich message path)"
  fi

  # Verify live 💬 Message (Stream send/edit) arrived BEFORE AskUserQuestion — ordering under drain
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_MQ + 1))" "$LOG_FILE")
  STREAM_LINE=$(awk '/Stream send:|Stream edit:/{print NR; exit}' <<< "$NEW_LOGS")
  AQ_LINE=$(awk '/AskUserQuestion sent/{print NR; exit}' <<< "$NEW_LOGS")
  if [ -z "$AQ_LINE" ]; then
    fail "AskUserQuestion sent log line not found in multiselect"
  elif [ -n "$STREAM_LINE" ]; then
    if [ "$STREAM_LINE" -lt "$AQ_LINE" ]; then
      pass "💬 Message (Stream send/edit) logged BEFORE AskUserQuestion in multiselect (line $STREAM_LINE < $AQ_LINE)"
    else
      fail "Stream message logged AFTER AskUserQuestion in multiselect (line $STREAM_LINE >= $AQ_LINE)"
    fi
  else
    pass "No pre-question streaming message in multiselect (CC produced no text — vacuous pass)"
  fi

  # Verify AskUserQuestion sent log contains non-empty content
  MQ_CONTENT=$(tail -n +"$((LOG_BEFORE_MQ + 1))" "$LOG_FILE" | grep -m1 "AskUserQuestion sent: msg_id=" | grep -oP 'content=\K.+' || true)
  if [ -n "$MQ_CONTENT" ]; then
    pass "AskUserQuestion sent log contains content in multiselect: $MQ_CONTENT"
  else
    fail "AskUserQuestion sent log missing content in multiselect"
  fi

else
  fail "Multi-question AskUserQuestion notification not received within ${TIMEOUT}s"
fi

# Extract msg_id from bot log
MQ_MSG_ID=$(tail -n +"$((LOG_BEFORE_MQ + 1))" "$LOG_FILE" | grep -oPm1 'AskUserQuestion.*msg_id=\K[0-9]+' || true)
echo "Multi-question msg_id: $MQ_MSG_ID"

if [ -z "$MQ_MSG_ID" ]; then
  fail "Could not extract multi-question msg_id from bot log"
else
  # TC (Fix 15): toggle Q2 (multiSelect: Colors) via the /test/callback "tool" path — the SAME callback
  # re-edit code the TG button uses (RetryFreezeEditAuto on the stored rich MsgText). Before Fix 15 the
  # rich AskUserQuestion had an empty .Text, so a plain RetryEdit with empty text FAILED and the ✅ never
  # appeared. Assert status=ok (the rich re-edit succeeded) AND the toggled option now has a ✅ prefix.
  pane_log "[multiselect] BEFORE Q2 callback toggle (Fix 15)"
  TOGGLE_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$MQ_MSG_ID&unique=tool&data=1:2")
  echo "  DEBUG: Fix15 callback toggle RESP: $TOGGLE_RESP"
  set +eo pipefail
  echo "$TOGGLE_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
labels=d.get('labels',[])
sys.exit(0 if d.get('status')=='ok' and any(l.startswith('✅') for l in labels) else 1)
"
  _rc=$?
  set -eo pipefail
  if [ "$_rc" -eq 0 ]; then
    pass "Fix 15: multiSelect toggle via callback path succeeded and shows ✅ on the selected option"
  else
    fail "Fix 15: callback toggle did not succeed with a ✅-prefixed label (resp=$TOGGLE_RESP)"
  fi
  # TC (Fix 18): the rebuilt keyboard after a toggle must STILL contain the ❌ Cancel button (it was
  # being dropped by RebuildAskMarkup, so Cancel disappeared after any toggle).
  set +eo pipefail
  echo "$TOGGLE_RESP" | python3 -c "
import sys,json
d=json.load(sys.stdin)
sys.exit(0 if any(l=='❌ Cancel' for l in d.get('labels',[])) else 1)
"
  _rc=$?
  set -eo pipefail
  if [ "$_rc" -eq 0 ]; then
    pass "Fix 18: rebuilt AskUserQuestion keyboard keeps the ❌ Cancel button after toggle"
  else
    fail "Fix 18: ❌ Cancel button missing from rebuilt keyboard after toggle (resp=$TOGGLE_RESP)"
  fi
  # Toggle the same option OFF again to restore state before the subsequent API toggles/submit.
  curl -s "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$MQ_MSG_ID&unique=tool&data=1:2" > /dev/null 2>&1 || true
  sleep 1

  # Select Q1 option 0 (Alpha) — single select
  pane_log "[multiselect] BEFORE Q1 select API"
  API_URL="http://127.0.0.1:$TEST_PORT/tool/respond?msg_id=$MQ_MSG_ID&tool=AskUserQuestion&question=0&option=0"
  echo "  API call: GET $API_URL"
  RESP=$(curl -s -w "\n%{http_code}" "$API_URL")
  echo "  DEBUG: RESP (${#RESP} chars): $RESP"
  RESP_CODE=$(echo "$RESP" | tail -1)
  if [ "$RESP_CODE" = "200" ]; then
    pass "Q1 option selected via API (Alpha)"
  else
    fail "Q1 option select API returned $RESP_CODE"
  fi
  sleep 2
  pane_log "[multiselect] 2s AFTER Q1 select API"

  # Toggle Q2 option 0 (Red) — multiSelect
  pane_log "[multiselect] BEFORE Q2 toggle 0 API"
  API_URL="http://127.0.0.1:$TEST_PORT/tool/respond?msg_id=$MQ_MSG_ID&tool=AskUserQuestion&question=1&option=0"
  echo "  API call: GET $API_URL"
  RESP=$(curl -s -w "\n%{http_code}" "$API_URL")
  RESP_CODE=$(echo "$RESP" | tail -1)
  if [ "$RESP_CODE" = "200" ]; then
    pass "Q2 option 0 toggled via API (Red)"
  else
    fail "Q2 option 0 toggle API returned $RESP_CODE"
  fi
  sleep 2
  pane_log "[multiselect] 2s AFTER Q2 toggle 0 API"

  # Toggle Q2 option 1 (Blue) — multiSelect
  pane_log "[multiselect] BEFORE Q2 toggle 1 API"
  API_URL="http://127.0.0.1:$TEST_PORT/tool/respond?msg_id=$MQ_MSG_ID&tool=AskUserQuestion&question=1&option=1"
  echo "  API call: GET $API_URL"
  RESP=$(curl -s -w "\n%{http_code}" "$API_URL")
  RESP_CODE=$(echo "$RESP" | tail -1)
  if [ "$RESP_CODE" = "200" ]; then
    pass "Q2 option 1 toggled via API (Blue)"
  else
    fail "Q2 option 1 toggle API returned $RESP_CODE"
  fi
  sleep 2
  pane_log "[multiselect] 2s AFTER Q2 toggle 1 API"

  # Verify option label in log (after API calls) — search within multiselect log range
  LABEL_LOGS=$(tail -n +"$((LOG_BEFORE_MQ + 1))" "$LOG_FILE" | grep -m3 "AskUserQuestion.*label=" || true)
  if [ -n "$LABEL_LOGS" ]; then
    pass "AskUserQuestion option log contains label in multiselect"
  else
    fail "AskUserQuestion option log missing label in multiselect"
  fi

  # Submit all answers
  pane_log "[multiselect] BEFORE submit API"

  # Record log position BEFORE submitting — Stop fires quickly after CC processes answers
  LOG_BEFORE_STOP6=$(wc -l < "$LOG_FILE")

  API_URL="http://127.0.0.1:$TEST_PORT/tool/respond?msg_id=$MQ_MSG_ID&tool=AskUserQuestion&action=submit"
  echo "  API call: GET $API_URL"
  SUBMIT_RESP=$(curl -s -w "\n%{http_code}" "$API_URL")
  echo "  DEBUG: SUBMIT_RESP (${#SUBMIT_RESP} chars): $SUBMIT_RESP"
  SUBMIT_CODE=$(echo "$SUBMIT_RESP" | tail -1)
  if [ "$SUBMIT_CODE" = "200" ]; then
    pass "Multi-question AskUserQuestion submitted via API"
  else
    fail "Submit API returned $SUBMIT_CODE"
  fi
  wait_for_idle
  pane_log "[multiselect] AFTER submit API (idle)"

  # f22: the post-multiQ turn finalizes via EITHER Stream relabel ✅ (streaming) OR a Stop direct-send
  # (a fast burst-at-Stop reply is delivered via ": Stop [" / "Stop terminal: outcome=direct_send" with
  # no relabel that turn). The multi-question answers are verified from the transcript below, so this
  # wait accepts the Stop turn-completion markers as delivery (f19 shape from phase9/phase27/phase29).
  ELAPSED=0
  STOP6_FOUND=false
  while [ $ELAPSED -lt $TIMEOUT ]; do
    if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_STOP6" ]; then
      if tail -n +"$((LOG_BEFORE_STOP6 + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
        STOP6_FOUND=true
        break
      fi
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
    echo "  Waiting for Stream relabel or Stop direct-send after multiQ... ${ELAPSED}s / ${TIMEOUT}s"
  done

  pane_log "[multiselect] AFTER Stop detected"

  # Structural guard (f29 exactly-once pin): the multiQ prompt pins ONE AskUserQuestion call carrying
  # TWO questions, so this window must have EXACTLY ONE 'Raw hook payload [PreToolUse]' with tool_name
  # AskUserQuestion (the questions=2 / answer-map checks below verify the single call held both).
  MQ_INVOKE_COUNT=$(tail -n +"$((LOG_BEFORE_MQ + 1))" "$LOG_FILE" | grep "Raw hook payload \[PreToolUse\]" | grep -c '"tool_name":"AskUserQuestion"' || true)
  if [ "$MQ_INVOKE_COUNT" -eq 1 ]; then
    pass "Exactly one AskUserQuestion PreToolUse invocation in multiQ (count=$MQ_INVOKE_COUNT)"
  else
    fail "Expected exactly 1 AskUserQuestion PreToolUse invocation in multiQ, got $MQ_INVOKE_COUNT (separate-call regression)"
  fi

  if [ "$STOP6_FOUND" = true ]; then
    pass "Stream relabel ✅ or Stop direct-send received after multiQ (turn finalized)"

    # Verify the reply carried non-empty content, delivered via EITHER the stream path (Stream send /
    # Stream edit final=true) OR a Stop direct-send ("Notification sent ...: Stop ... body=<non-empty>",
    # no Stream send that turn). f22: content verification is preserved — the delivery path is widened.
    if tail -n +"$((LOG_BEFORE_STOP6 + 1))" "$LOG_FILE" | grep -E "Stream send:|Stream edit:.*final=true|Notification sent .*: Stop \[.*body=." > /dev/null 2>&1; then
      pass "Streaming message or Stop direct-send has content after multiQ"
    else
      fail "No Stream send/edit or Stop direct-send content found after multiQ"
    fi

    # Verify transcript contains multi-question answers
    MQ_TRANSCRIPT_PATH=$(tail -n +"$((LOG_BEFORE_MQ + 1))" "$LOG_FILE" | \
      grep -oP '"transcript_path":"[^"]*"' | tail -1 | cut -d'"' -f4 || true)

    if [ -n "$MQ_TRANSCRIPT_PATH" ] && [ -f "$MQ_TRANSCRIPT_PATH" ]; then
      # Parse the LAST toolUseResult.answers from JSONL (the multi-question one)
      ALL_ANSWERS=$(cat "$MQ_TRANSCRIPT_PATH" | while IFS= read -r line; do
        echo "$line" | jq -c 'select(.toolUseResult.answers != null) | .toolUseResult.answers' 2>/dev/null
      done | tail -1)
      if [ -n "$ALL_ANSWERS" ]; then
        echo "  DEBUG: ALL_ANSWERS (${#ALL_ANSWERS} chars): $ALL_ANSWERS"
        Q1_ANS=$(echo "$ALL_ANSWERS" | jq -r '.["Which do you prefer?"] // empty' 2>/dev/null)
        Q2_ANS=$(echo "$ALL_ANSWERS" | jq -r '.["Pick colors"] // empty' 2>/dev/null)
        Q1_OK=false
        Q2_OK=false
        if [ "$Q1_ANS" = "Alpha" ]; then Q1_OK=true; fi
        # Q2 multiSelect answer is "Red, Blue" (comma-space separated from buildAnswers)
        if [ "$Q2_ANS" = "Red, Blue" ]; then Q2_OK=true; fi
        if [ "$Q1_OK" = true ] && [ "$Q2_OK" = true ]; then
          pass "CC received multi-question answers in transcript (Q1=$Q1_ANS, Q2=$Q2_ANS)"
        else
          fail "CC transcript answers wrong (Q1='$Q1_ANS' expect Alpha, Q2='$Q2_ANS' expect 'Red, Blue')"
        fi
      else
        fail "No toolUseResult.answers found in transcript"
      fi
    else
      fail "Multi-question transcript path not found or file missing"
    fi
  else
    fail "Neither Stream relabel ✅ nor Stop direct-send found after multiQ within ${TIMEOUT}s"
  fi
fi
