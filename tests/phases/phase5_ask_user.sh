#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Phase 5: AskUserQuestion test ---"

ensure_infrastructure

LOG_BEFORE_AQ=$(wc -l < "$LOG_FILE")

pane_log "[Phase 5] BEFORE sending AskUserQuestion prompt"

# Send prompt that should trigger AskUserQuestion
inject_prompt "First write a brief paragraph explaining what you are about to do, then ask me a question using AskUserQuestion tool with header 'Test Header' and two options: 'Option A' with description 'First option desc', 'Option B' with description 'Second option desc'. Question: 'Which option?'"

pane_log "[Phase 5] AFTER sending prompt"

# Wait for AskUserQuestion notification
ELAPSED=0
AQ_FOUND=false
AQ_MSG_ID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_AQ" ]; then
    NEW_LOGS=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE")
    if echo "$NEW_LOGS" | grep "AskUserQuestion sent" > /dev/null 2>&1; then
      AQ_FOUND=true
      AQ_MSG_ID=$(grep -oPm1 'AskUserQuestion sent.*msg_id=\K[0-9]+' <<< "$NEW_LOGS" || true)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

wait_for_cc_idle
pane_log "[Phase 5] AFTER hook notification (idle)"

if [ "$AQ_FOUND" = true ] && [ -n "$AQ_MSG_ID" ]; then
  pass "AskUserQuestion TG notification sent (msg_id=$AQ_MSG_ID)"

  # Verify Update notification sent BEFORE AskUserQuestion
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE")
  UPDATE_LINE=$(awk '/Notification sent.*PreToolUse/{print NR; exit}' <<< "$NEW_LOGS")
  AQ_LINE=$(awk '/AskUserQuestion sent/{print NR; exit}' <<< "$NEW_LOGS")
  if [ -n "$UPDATE_LINE" ] && [ -n "$AQ_LINE" ]; then
    if [ "$UPDATE_LINE" -lt "$AQ_LINE" ]; then
      pass "Update notification sent BEFORE AskUserQuestion (line $UPDATE_LINE < $AQ_LINE)"
    else
      fail "Update sent AFTER AskUserQuestion"
    fi
  else
    [ -z "$UPDATE_LINE" ] && fail "PreToolUse Update not found before AskUserQuestion"
  fi

  # Verify AskUserQuestion sent log contains non-empty content
  AQ_CONTENT=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | grep -m1 "AskUserQuestion sent" | grep -oP 'content=\K.+' || true)
  if [ -n "$AQ_CONTENT" ]; then
    pass "AskUserQuestion sent log contains content: $AQ_CONTENT"
  else
    fail "AskUserQuestion sent log missing content"
  fi

  pane_log "[Phase 5] BEFORE option selection API"

  # Delay 5s before responding to test concurrent hook timing
  echo "  Waiting 5s before responding to first AskUserQuestion..."
  sleep 5

  # Record log position BEFORE selecting — Stop fires quickly after CC processes answers
  LOG_BEFORE_STOP5=$(wc -l < "$LOG_FILE")

  # Select option 1 (Approach B) via API
  API_URL="http://127.0.0.1:$TEST_PORT/tool/respond?msg_id=$AQ_MSG_ID&tool=AskUserQuestion&question=0&option=1"
  echo "  API call: GET $API_URL"
  SELECT_RESP=$(curl -s -w "\n%{http_code}" "$API_URL")
  SELECT_CODE=$(echo "$SELECT_RESP" | tail -1)
  if [ "$SELECT_CODE" = "200" ]; then
    pass "AskUserQuestion option selected via /tool/respond API"
  else
    fail "AskUserQuestion select API returned $SELECT_CODE"
  fi

  wait_for_cc_idle
  pane_log "[Phase 5] AFTER option selection API (idle)"

  # Verify bot logged the selection with label
  sleep 2
  RESOLVE_LOG=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | grep "AskUserQuestion auto-resolved\|AskUserQuestion option" | tail -1)
  if [ -n "$RESOLVE_LOG" ]; then
    pass "AskUserQuestion option selection logged"
    if echo "$RESOLVE_LOG" | grep -q "label=."; then
      SELECTED_LABEL=$(echo "$RESOLVE_LOG" | grep -oP 'label=\K\S+')
      pass "AskUserQuestion option log contains label=$SELECTED_LABEL"
    else
      fail "AskUserQuestion option log missing label"
    fi
  else
    fail "AskUserQuestion option selection not found in log"
  fi

  ELAPSED=0
  STOP5_FOUND=false
  while [ $ELAPSED -lt $TIMEOUT ]; do
    if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_STOP5" ]; then
      if tail -n +"$((LOG_BEFORE_STOP5 + 1))" "$LOG_FILE" | grep "Notification sent.*Stop.*body_len=" > /dev/null 2>&1; then
        STOP5_FOUND=true
        break
      fi
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  pane_log "[Phase 5] AFTER Stop detected"

  if [ "$STOP5_FOUND" = true ]; then
    BODY_LEN=$(tail -n +"$((LOG_BEFORE_STOP5 + 1))" "$LOG_FILE" | grep -oPm1 'Notification sent.*Stop.*body_len=\K[0-9]+' || true)
    if [ -n "$BODY_LEN" ] && [ "$BODY_LEN" -gt 0 ]; then
      pass "Stop notification has content after AskUserQuestion (body_len=$BODY_LEN)"
    else
      fail "Stop notification body is empty after AskUserQuestion"
    fi

    # Verify Stop notification log contains actual body content
    if tail -n +"$((LOG_BEFORE_STOP5 + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" | grep "body=." > /dev/null 2>&1; then
      pass "Stop notification log contains actual body content"
    else
      fail "Stop notification log missing body content"
    fi

    # Extract transcript path from bot log (CC uses snake_case: transcript_path)
    TRANSCRIPT_PATH=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | \
      grep -oP '"transcript_path":"[^"]*"' | tail -1 | cut -d'"' -f4 || true)

    if [ -n "$TRANSCRIPT_PATH" ] && [ -f "$TRANSCRIPT_PATH" ]; then
      # Parse toolUseResult.answers from the JSONL to verify exact answer value
      ACTUAL_ANSWER=$(cat "$TRANSCRIPT_PATH" | while IFS= read -r line; do
        echo "$line" | jq -r 'select(.toolUseResult.answers["Which option?"] != null) | .toolUseResult.answers["Which option?"]' 2>/dev/null
      done | tail -1)
      if [ "$ACTUAL_ANSWER" = "Option B" ]; then
        pass "CC received answer 'Option B' in transcript (toolUseResult.answers)"
      else
        fail "CC transcript answer is '$ACTUAL_ANSWER', expected 'Option B'"
      fi
    else
      fail "Transcript path not found or file missing"
    fi
  else
    fail "Stop notification not found after AskUserQuestion within ${TIMEOUT}s"
  fi

  # --- Free-text reply test ---
  LOG_BEFORE_FT=$(wc -l < "$LOG_FILE")

  pane_log "[Phase 5] BEFORE sending free-text AskUserQuestion prompt"

  # Send prompt for free-text question (min 2 options required by AskUserQuestion)
  inject_prompt "First write a brief paragraph, then ask me one question using AskUserQuestion tool with header 'Free Text Test' and two options: 'Blue' with description 'The color blue', 'Red' with description 'The color red'. Question: 'What is your favorite color?'"

  pane_log "[Phase 5] AFTER sending free-text prompt"

  # Wait for AskUserQuestion notification
  ELAPSED=0
  FT_FOUND=false
  FT_MSG_ID=""
  while [ $ELAPSED -lt $TIMEOUT ]; do
    LOG_NOW=$(wc -l < "$LOG_FILE")
    if [ "$LOG_NOW" -gt "$LOG_BEFORE_FT" ]; then
      NEW_LOGS=$(tail -n +"$((LOG_BEFORE_FT + 1))" "$LOG_FILE")
      if echo "$NEW_LOGS" | grep "AskUserQuestion sent" > /dev/null 2>&1; then
        FT_FOUND=true
        FT_MSG_ID=$(grep -oPm1 'AskUserQuestion sent.*msg_id=\K[0-9]+' <<< "$NEW_LOGS" || true)
        break
      fi
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  if [ "$FT_FOUND" = true ] && [ -n "$FT_MSG_ID" ]; then
    pass "Free-text AskUserQuestion notification sent (msg_id=$FT_MSG_ID)"

    pane_log "[Phase 5] BEFORE free-text API call"

    # Record log position BEFORE sending — Stop fires quickly after CC processes answers
    LOG_BEFORE_FT_STOP=$(wc -l < "$LOG_FILE")

    API_URL="http://127.0.0.1:$TEST_PORT/tool/respond?msg_id=$FT_MSG_ID&tool=AskUserQuestion&action=text&value=my+custom+answer"
    echo "  API call: GET $API_URL"
    FT_RESP=$(curl -s -w "\n%{http_code}" "$API_URL")
    FT_CODE=$(echo "$FT_RESP" | tail -1)
    if [ "$FT_CODE" = "200" ]; then
      pass "Free-text answer sent via /tool/respond API"
    else
      fail "Free-text API returned $FT_CODE"
    fi

    wait_for_cc_idle
    pane_log "[Phase 5] AFTER free-text API call (idle)"

    # Wait for Stop notification
    ELAPSED=0
    FT_STOP_FOUND=false
    while [ $ELAPSED -lt $TIMEOUT ]; do
      if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_FT_STOP" ]; then
        if tail -n +"$((LOG_BEFORE_FT_STOP + 1))" "$LOG_FILE" | grep "Notification sent.*Stop.*body_len=" > /dev/null 2>&1; then
          FT_STOP_FOUND=true
          break
        fi
      fi
      sleep 2
      ELAPSED=$((ELAPSED + 2))
    done

    if [ "$FT_STOP_FOUND" = true ]; then
      pass "Stop notification received after free-text answer"

      # Verify transcript contains custom answer
      FT_TRANSCRIPT_PATH=$(tail -n +"$((LOG_BEFORE_FT + 1))" "$LOG_FILE" | \
        grep -oP '"transcript_path":"[^"]*"' | tail -1 | cut -d'"' -f4 || true)

      if [ -n "$FT_TRANSCRIPT_PATH" ] && [ -f "$FT_TRANSCRIPT_PATH" ]; then
        # Parse toolUseResult.answers to find free-text answer value
        FT_ACTUAL=$(cat "$FT_TRANSCRIPT_PATH" | while IFS= read -r line; do
          echo "$line" | jq -r '
            select(.toolUseResult.answers != null) |
            .toolUseResult.answers | to_entries[] |
            select(.value == "my custom answer") | .value
          ' 2>/dev/null
        done | tail -1)
        if [ "$FT_ACTUAL" = "my custom answer" ]; then
          pass "CC received free-text answer 'my custom answer' in transcript (toolUseResult.answers)"
        else
          fail "CC transcript free-text answer is '$FT_ACTUAL', expected 'my custom answer'"
        fi
      else
        fail "Free-text transcript path not found or file missing"
      fi
    else
      fail "Stop notification not found after free-text answer within ${TIMEOUT}s"
    fi
  else
    fail "Free-text AskUserQuestion not triggered within ${TIMEOUT}s"
  fi

  # --- Group direct free-text test (via /group/text API) ---
  # Extract tmux_target from SessionStart log (same pattern as Phase 8)
  TMUX_TARGET=""
  SESSION_START_LINE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -m1 "Notification sent.*SessionStart" || true)
  if [ -n "$SESSION_START_LINE" ]; then
    TMUX_TARGET=$(echo "$SESSION_START_LINE" | grep -oP 'tmux=\K[^[:space:]]+' || true)
  fi
  if [ -z "$TMUX_TARGET" ]; then
    fail "Could not extract tmux_target from SessionStart log for group-text test"
  fi

  LOG_BEFORE_GT=$(wc -l < "$LOG_FILE")

  pane_log "[Phase 5] BEFORE sending group-text AskUserQuestion prompt"

  # Send prompt for group direct text question
  inject_prompt "First write a brief paragraph, then ask me one question using AskUserQuestion tool with header 'Group Test' and two options: 'Yes' with description 'Agree', 'No' with description 'Disagree'. Question: 'Do you agree?'"

  pane_log "[Phase 5] AFTER sending group-text prompt"

  # Wait for AskUserQuestion notification
  ELAPSED=0
  GT_FOUND=false
  GT_MSG_ID=""
  while [ $ELAPSED -lt $TIMEOUT ]; do
    LOG_NOW=$(wc -l < "$LOG_FILE")
    if [ "$LOG_NOW" -gt "$LOG_BEFORE_GT" ]; then
      NEW_LOGS=$(tail -n +"$((LOG_BEFORE_GT + 1))" "$LOG_FILE")
      if echo "$NEW_LOGS" | grep "AskUserQuestion sent" > /dev/null 2>&1; then
        GT_FOUND=true
        GT_MSG_ID=$(grep -oPm1 'AskUserQuestion sent.*msg_id=\K[0-9]+' <<< "$NEW_LOGS" || true)
        break
      fi
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  if [ "$GT_FOUND" = true ] && [ -n "$GT_MSG_ID" ]; then
    pass "Group-text AskUserQuestion notification sent (msg_id=$GT_MSG_ID)"

    pane_log "[Phase 5] BEFORE group-text API call"

    # Record log position BEFORE sending
    LOG_BEFORE_GT_STOP=$(wc -l < "$LOG_FILE")

    # Use /group/text API to send answer (simulates group direct message)
    # URL-encode TMUX_TARGET because it contains '%' (e.g., %749) which would be decoded by Go's HTTP server
    ENCODED_TARGET=$(printf '%s' "$TMUX_TARGET" | jq -sRr @uri)
    API_URL="http://127.0.0.1:$TEST_PORT/group/text?target=$ENCODED_TARGET&text=group+direct+answer"
    echo "  API call: GET $API_URL"
    GT_RESP=$(curl -s -w "\n%{http_code}" "$API_URL")
    GT_CODE=$(echo "$GT_RESP" | tail -1)
    GT_BODY=$(echo "$GT_RESP" | head -1)
    if [ "$GT_CODE" = "200" ] && [ "$GT_BODY" = "resolved" ]; then
      pass "Group direct text resolved via /group/text API"
    else
      fail "Group text API returned code=$GT_CODE body=$GT_BODY"
    fi

    wait_for_cc_idle
    pane_log "[Phase 5] AFTER group-text API call (idle)"

    # Verify bot log shows resolution via group text API
    GT_RESOLVE_LOG=$(tail -n +"$((LOG_BEFORE_GT + 1))" "$LOG_FILE" | grep -m1 "AskUserQuestion resolved via group text API" || true)
    if [ -n "$GT_RESOLVE_LOG" ]; then
      pass "AskUserQuestion resolved via group text API logged"
    else
      fail "AskUserQuestion group text API resolution not found in log"
    fi

    # Wait for Stop notification
    ELAPSED=0
    GT_STOP_FOUND=false
    while [ $ELAPSED -lt $TIMEOUT ]; do
      if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_GT_STOP" ]; then
        if tail -n +"$((LOG_BEFORE_GT_STOP + 1))" "$LOG_FILE" | grep "Notification sent.*Stop.*body_len=" > /dev/null 2>&1; then
          GT_STOP_FOUND=true
          break
        fi
      fi
      sleep 2
      ELAPSED=$((ELAPSED + 2))
    done

    if [ "$GT_STOP_FOUND" = true ]; then
      pass "Stop notification received after group direct text answer"

      # Verify transcript contains group direct answer
      GT_TRANSCRIPT_PATH=$(tail -n +"$((LOG_BEFORE_GT + 1))" "$LOG_FILE" | \
        grep -oP '"transcript_path":"[^"]*"' | tail -1 | cut -d'"' -f4 || true)

      if [ -n "$GT_TRANSCRIPT_PATH" ] && [ -f "$GT_TRANSCRIPT_PATH" ]; then
        GT_ACTUAL=$(cat "$GT_TRANSCRIPT_PATH" | while IFS= read -r line; do
          echo "$line" | jq -r '
            select(.toolUseResult.answers != null) |
            .toolUseResult.answers | to_entries[] |
            select(.value == "group direct answer") | .value
          ' 2>/dev/null
        done | tail -1)
        if [ "$GT_ACTUAL" = "group direct answer" ]; then
          pass "CC received group direct answer 'group direct answer' in transcript"
        else
          fail "CC transcript group-text answer is '$GT_ACTUAL', expected 'group direct answer'"
        fi
      else
        fail "Group-text transcript path not found or file missing"
      fi
    else
      fail "Stop notification not found after group direct text answer within ${TIMEOUT}s"
    fi
  else
    fail "Group-text AskUserQuestion not triggered within ${TIMEOUT}s"
  fi

else
  fail "AskUserQuestion not triggered within ${TIMEOUT}s"
fi
