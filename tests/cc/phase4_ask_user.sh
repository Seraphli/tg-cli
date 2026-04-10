#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- AskUserQuestion test ---"

ensure_infrastructure
wait_for_idle

LOG_BEFORE_AQ=$(wc -l < "$LOG_FILE")
TYPING_LOG_BEFORE=$(wc -l < "$TYPING_LOG_FILE" 2>/dev/null || echo 0)

pane_log "[ask_user] BEFORE sending AskUserQuestion prompt"

# Send prompt that should trigger AskUserQuestion
inject_prompt "Answer this question first in 2 sentences: what is the purpose of asking users structured multiple-choice questions in a CLI tool? After answering, use the AskUserQuestion tool to ask me a question with header 'Test Header' and two options: 'Option A' with description 'First option desc', 'Option B' with description 'Second option desc'. Question: 'Which option?'"

pane_log "[ask_user] AFTER sending prompt"

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

wait_for_idle
pane_log "[ask_user] AFTER hook notification (idle)"

if [ "$AQ_FOUND" = true ] && [ -n "$AQ_MSG_ID" ]; then
  pass "AskUserQuestion TG notification sent (msg_id=$AQ_MSG_ID)"

  # Verify AskUserQuestion ToolUse notification format (from BuildToolNotifyText AskUserQuestion case)
  AQ_TOOLUSE_TEXT=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | grep -A20 "TG message sent \[ToolUse\] full_text" | head -20 || true)
  if [ -n "$AQ_TOOLUSE_TEXT" ]; then
    if echo "$AQ_TOOLUSE_TEXT" | grep "❓" > /dev/null 2>&1; then
      pass "AskUserQuestion ToolUse notification has ❓ format"
    else
      fail "AskUserQuestion ToolUse notification missing ❓ format"
    fi
    if echo "$AQ_TOOLUSE_TEXT" | grep "map\[" > /dev/null 2>&1; then
      fail "AskUserQuestion ToolUse notification contains raw Go map[] format"
    else
      pass "AskUserQuestion ToolUse notification does not contain raw map[] format"
    fi
  else
    fail "AskUserQuestion ToolUse full_text not found in log"
  fi

  # Typing continuity: inject → PreToolUse (text generation before AskUserQuestion)
  check_typing_continuity "$TYPING_LOG_BEFORE" "PreToolUse" "phase4"

  # Verify Update notification sent BEFORE AskUserQuestion (tolerant to Claude skipping intermediate text)
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE")
  UPDATE_LINE=$(awk '/Notification sent.*PreToolUse/{print NR; exit}' <<< "$NEW_LOGS")
  AQ_LINE=$(awk '/AskUserQuestion sent/{print NR; exit}' <<< "$NEW_LOGS")
  if [ -z "$AQ_LINE" ]; then
    fail "AskUserQuestion sent log line not found"
  elif [ -n "$UPDATE_LINE" ]; then
    if [ "$UPDATE_LINE" -lt "$AQ_LINE" ]; then
      pass "Update notification sent BEFORE AskUserQuestion (line $UPDATE_LINE < $AQ_LINE)"
    else
      fail "Update sent AFTER AskUserQuestion"
    fi
  elif [[ "$NEW_LOGS" == *"PreToolUse Update skipped"* ]]; then
    pass "PreToolUse Update path exercised (skipped: no new assistant text — vacuous pass)"
  else
    fail "Neither PreToolUse Update sent nor skipped log found — code path not exercised"
  fi

  # Verify AskUserQuestion sent log contains non-empty content
  AQ_CONTENT=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | grep -m1 "AskUserQuestion sent" | grep -oP 'content=\K.+' || true)
  if [ -n "$AQ_CONTENT" ]; then
    pass "AskUserQuestion sent log contains content: $AQ_CONTENT"
  else
    fail "AskUserQuestion sent log missing content"
  fi

  pane_log "[ask_user] BEFORE option selection API"

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

  wait_for_idle
  pane_log "[ask_user] AFTER option selection API (idle)"

  # Verify bot logged the selection with label
  sleep 2
  RESOLVE_LOG=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | grep "AskUserQuestion responded\|AskUserQuestion option" | tail -1)
  if [ -n "$RESOLVE_LOG" ]; then
    pass "AskUserQuestion option selection logged"
    if echo "$RESOLVE_LOG" | grep -q "answers=\|label="; then
      SELECTED_LABEL=$(echo "$RESOLVE_LOG" | grep -oP '(answers|label)=\K\S+')
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

  pane_log "[ask_user] AFTER Stop detected"

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

    # Verify PostToolUse result format for AskUserQuestion (→ format instead of raw map)
    AQ_POST_LOG=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | grep "PostToolUse: updated msg_id=" || true)
    if [ -n "$AQ_POST_LOG" ]; then
      pass "PostToolUse updated AskUserQuestion ToolUse message"
    else
      # PostToolUse may not fire if AskUserQuestion ToolUse notification was not sent (e.g. not in toolNotifyList)
      fail "PostToolUse update not detected for AskUserQuestion"
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

  pane_log "[ask_user] BEFORE sending free-text AskUserQuestion prompt"

  # Send prompt for free-text question (min 2 options required by AskUserQuestion)
  inject_prompt "First write a brief paragraph, then ask me one question using AskUserQuestion tool with header 'Free Text Test' and two options: 'Blue' with description 'The color blue', 'Red' with description 'The color red'. Question: 'What is your favorite color?'"

  pane_log "[ask_user] AFTER sending free-text prompt"

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

    pane_log "[ask_user] BEFORE free-text API call"

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
  # Extract tmux_target from SessionStart log (same pattern as group_routing)
  TMUX_TARGET=""
  SESSION_START_LINE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -m1 "Notification sent.*SessionStart" || true)
  if [ -n "$SESSION_START_LINE" ]; then
    TMUX_TARGET=$(echo "$SESSION_START_LINE" | grep -oP 'tmux=\K[^[:space:]]+' || true)
  fi
  if [ -z "$TMUX_TARGET" ]; then
    fail "Could not extract tmux_target from SessionStart log for group-text test"
  fi

  LOG_BEFORE_GT=$(wc -l < "$LOG_FILE")

  pane_log "[ask_user] BEFORE sending group-text AskUserQuestion prompt"

  # Send prompt for group direct text question
  inject_prompt "First write a brief paragraph, then ask me one question using AskUserQuestion tool with header 'Group Test' and two options: 'Yes' with description 'Agree', 'No' with description 'Disagree'. Question: 'Do you agree?'"

  pane_log "[ask_user] AFTER sending group-text prompt"

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

    pane_log "[ask_user] BEFORE group-text API call"

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

    wait_for_idle
    pane_log "[ask_user] AFTER group-text API call (idle)"

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

# --- AskUserQuestion Cancel button test ---
echo ""
echo "--- AskUserQuestion Cancel button test ---"

wait_for_idle
LOG_BEFORE_AQCANCEL=$(wc -l < "$LOG_FILE")

pane_log "[ask_cancel] BEFORE sending AskUserQuestion cancel prompt"

inject_prompt "First write a brief paragraph, then ask me one question using AskUserQuestion tool with header 'Cancel Test' and two options: 'Keep' with description 'Keep current', 'Change' with description 'Change it'. Question: 'Should we proceed?'"

pane_log "[ask_cancel] AFTER sending cancel prompt"

# Wait for AskUserQuestion notification
ELAPSED=0
AQCANCEL_FOUND=false
AQCANCEL_UUID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_AQCANCEL" ]; then
    NEW_LOGS=$(tail -n +"$((LOG_BEFORE_AQCANCEL + 1))" "$LOG_FILE")
    if echo "$NEW_LOGS" | grep "AskUserQuestion sent" > /dev/null 2>&1; then
      AQCANCEL_FOUND=true
      AQCANCEL_UUID=$(grep -oPm1 'AskUserQuestion sent.*uuid=\K[^ ]+' <<< "$NEW_LOGS" || true)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

pane_log "[ask_cancel] AFTER AskUserQuestion detected"

if [ "$AQCANCEL_FOUND" = true ] && [ -n "$AQCANCEL_UUID" ]; then
  pass "AskUserQuestion cancel test: notification sent (uuid=$AQCANCEL_UUID)"

  # Cancel via /pending/cancel API
  pane_log "[ask_cancel] BEFORE cancel API call"
  CANCEL_URL="http://127.0.0.1:$TEST_PORT/pending/cancel?uuid=$AQCANCEL_UUID"
  echo "  API call: POST $CANCEL_URL"
  CANCEL_RESP=$(curl -s -X POST "$CANCEL_URL")
  pane_log "[ask_cancel] AFTER cancel API call"

  # Wait for cancel confirmation in log
  ELAPSED=0
  AQCANCEL_LOGGED=false
  while [ $ELAPSED -lt $TIMEOUT ]; do
    if tail -n +"$((LOG_BEFORE_AQCANCEL + 1))" "$LOG_FILE" | grep "AskUserQuestion cancelled: msg_id=" > /dev/null 2>&1; then
      AQCANCEL_LOGGED=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  if [ "$AQCANCEL_LOGGED" = true ]; then
    pass "AskUserQuestion cancelled via /pending/cancel API"
  else
    fail "AskUserQuestion cancel log not found within ${TIMEOUT}s"
  fi

  wait_for_idle
  pane_log "[ask_cancel] AFTER CC idle"
else
  fail "AskUserQuestion cancel test: notification not triggered within ${TIMEOUT}s"
fi
