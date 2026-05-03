#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Multi-question multiSelect AskUserQuestion (hook) ---"

ensure_infrastructure

LOG_BEFORE_MQ=$(wc -l < "$LOG_FILE")

# Send prompt that triggers multi-question AskUserQuestion
pane_log "[multiselect] BEFORE multiQ prompt"
inject_prompt "Answer this question first in 2 sentences: what is the benefit of asking multiple questions in a single AskUserQuestion tool call versus making separate calls? After answering, ask me TWO questions using AskUserQuestion tool with these exact parameters: questions array with 2 items. Question 1: header 'Preference', question 'Which do you prefer?', two options - 'Alpha' with description 'First choice', 'Beta' with description 'Second choice', multiSelect false. Question 2: header 'Colors', question 'Pick colors', three options - 'Red' with description 'Red color', 'Blue' with description 'Blue color', 'Green' with description 'Green color', multiSelect true."
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

wait_for_idle
pane_log "[multiselect] AFTER hook notification detected (idle)"

if [ "$MQ_FOUND" = true ]; then
  pass "Multi-question AskUserQuestion notification received"

  # Verify Update notification sent BEFORE AskUserQuestion (tolerant to Claude skipping intermediate text)
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_MQ + 1))" "$LOG_FILE")
  UPDATE_LINE=$(awk '/Notification sent.*PreToolUse/{print NR; exit}' <<< "$NEW_LOGS")
  AQ_LINE=$(awk '/AskUserQuestion sent/{print NR; exit}' <<< "$NEW_LOGS")
  if [ -z "$AQ_LINE" ]; then
    fail "AskUserQuestion sent log line not found in multiselect"
  elif [ -n "$UPDATE_LINE" ]; then
    if [ "$UPDATE_LINE" -lt "$AQ_LINE" ]; then
      pass "Update notification sent BEFORE AskUserQuestion in multiselect (line $UPDATE_LINE < $AQ_LINE)"
    else
      fail "Update sent AFTER AskUserQuestion in multiselect"
    fi
  elif [[ "$NEW_LOGS" == *"PreToolUse Update skipped"* ]]; then
    pass "PreToolUse Update path exercised in multiselect (skipped: no new assistant text — vacuous pass)"
  else
    fail "Neither PreToolUse Update sent nor skipped log found in multiselect — code path not exercised"
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

  ELAPSED=0
  STOP6_FOUND=false
  while [ $ELAPSED -lt $TIMEOUT ]; do
    if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_STOP6" ]; then
      if tail -n +"$((LOG_BEFORE_STOP6 + 1))" "$LOG_FILE" | grep "Notification sent.*Stop.*body_len=" > /dev/null 2>&1; then
        STOP6_FOUND=true
        break
      fi
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
    echo "  Waiting for Stop after multiQ... ${ELAPSED}s / ${TIMEOUT}s"
  done

  pane_log "[multiselect] AFTER Stop detected"

  if [ "$STOP6_FOUND" = true ]; then
    BODY_LEN=$(tail -n +"$((LOG_BEFORE_STOP6 + 1))" "$LOG_FILE" | grep -oPm1 'Notification sent.*Stop.*body_len=\K[0-9]+' || true)
    if [ -n "$BODY_LEN" ] && [ "$BODY_LEN" -gt 0 ]; then
      pass "Stop notification has content after multiQ (body_len=$BODY_LEN)"
    else
      fail "Stop notification body is empty after multiQ"
    fi

    # Verify Stop notification log contains actual body content
    if tail -n +"$((LOG_BEFORE_STOP6 + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" | grep "body=." > /dev/null 2>&1; then
      pass "Stop notification log contains actual body content in multiselect"
    else
      fail "Stop notification log missing body content in multiselect"
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
    fail "Stop notification not found after multiQ within ${TIMEOUT}s"
  fi
fi
