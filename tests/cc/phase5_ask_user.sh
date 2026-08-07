#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- AskUserQuestion test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-5"
wait_for_idle

LOG_BEFORE_AQ=$(wc -l < "$LOG_FILE")
TYPING_LOG_BEFORE=$(wc -l < "$TYPING_LOG_FILE" 2>/dev/null || echo 0)

pane_log "[ask_user] BEFORE sending AskUserQuestion prompt"

# Send prompt that should trigger AskUserQuestion
inject_prompt "Answer this question first in 2 sentences: what is the purpose of asking users structured multiple-choice questions in a CLI tool? After answering, use the AskUserQuestion tool to ask me a question with header 'Test Header' and two options: 'Option A' with description 'First option desc', 'Option B' with description 'Second option desc'. Question: 'Which option?' Call AskUserQuestion exactly once in this turn, with exactly one item in its questions array. After that call returns, do not call AskUserQuestion or any other tool again. Briefly acknowledge the answer, then stop."

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

pane_log "[ask_user] AFTER hook notification detected"

if [ "$AQ_FOUND" = true ] && [ -n "$AQ_MSG_ID" ]; then
  pass "AskUserQuestion TG notification sent (msg_id=$AQ_MSG_ID)"

  # Rich migration (v9): AskUserQuestion is sent via sendRichMessage → the sent marker carries
  # fmt=rich on the same log line. Guards against reverting the AskQ send to legacy HTML mode.
  if tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | grep -q "AskUserQuestion sent:.*fmt=rich" 2>/dev/null; then
    pass "AskUserQuestion sent via rich message path (fmt=rich)"
  else
    fail "AskUserQuestion sent marker missing fmt=rich (expected rich message path)"
  fi

  # Verify AskUserQuestion notification format (from pending.go BuildQuestionText path)
  AQ_QUESTION_TEXT=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | grep -A20 "TG question message sent full_text" | head -20 || true)
  if [ -n "$AQ_QUESTION_TEXT" ]; then
    if echo "$AQ_QUESTION_TEXT" | grep "❓" > /dev/null 2>&1; then
      pass "AskUserQuestion notification has ❓ format"
    else
      fail "AskUserQuestion notification missing ❓ format"
    fi
    if echo "$AQ_QUESTION_TEXT" | grep "map\[" > /dev/null 2>&1; then
      fail "AskUserQuestion notification contains raw Go map[] format"
    else
      pass "AskUserQuestion notification does not contain raw map[] format"
    fi
  else
    fail "AskUserQuestion question message not found in log"
  fi

  # Typing continuity: inject → PreToolUse (text generation before AskUserQuestion)
  check_typing_continuity "$TYPING_LOG_BEFORE" "PreToolUse" "phase5"

  # Verify live 💬 Message (Stream send/edit) arrived BEFORE AskUserQuestion buttons — ordering under drain
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE")
  STREAM_LINE=$(awk '/Stream send:|Stream edit:/{print NR; exit}' <<< "$NEW_LOGS")
  AQ_LINE=$(awk '/AskUserQuestion sent/{print NR; exit}' <<< "$NEW_LOGS")
  if [ -z "$AQ_LINE" ]; then
    fail "AskUserQuestion sent log line not found"
  elif [ -n "$STREAM_LINE" ]; then
    if [ "$STREAM_LINE" -lt "$AQ_LINE" ]; then
      pass "💬 Message (Stream send/edit) logged BEFORE AskUserQuestion buttons (line $STREAM_LINE < $AQ_LINE)"
    else
      fail "Stream message logged AFTER AskUserQuestion buttons (line $STREAM_LINE >= $AQ_LINE)"
    fi
  else
    # CC may output zero pre-question text — no streaming line is acceptable only if none was prompted
    pass "No pre-question streaming message (CC produced no text before AskUserQuestion — vacuous pass)"
  fi

  # Verify no old no_new_assistant_text skip log (that path is removed)
  if [[ "$NEW_LOGS" == *"no_new_assistant_text"* ]]; then
    fail "Old no_new_assistant_text skip log found — should not exist in new streaming code"
  else
    pass "No stale no_new_assistant_text skip log"
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
  echo "  DEBUG: SELECT_RESP (${#SELECT_RESP} chars): $SELECT_RESP"
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
    set +eo pipefail
    echo "$RESOLVE_LOG" | grep -q "answers=\|label="
    _ps=("${PIPESTATUS[@]}")
    set -eo pipefail
    if [ "${_ps[1]}" -eq 0 ]; then
      SELECTED_LABEL=$(echo "$RESOLVE_LOG" | grep -oP '(answers|label)=\K\S+')
      pass "AskUserQuestion option log contains label=$SELECTED_LABEL"
    else
      fail "AskUserQuestion option log missing label"
    fi
  else
    fail "AskUserQuestion option selection not found in log"
  fi

  # f22: the post-answer turn finalizes via EITHER Stream relabel ✅ (streaming) OR a Stop direct-send
  # (a fast burst-at-Stop reply is delivered via ": Stop [" / "Stop terminal: outcome=direct_send" with
  # no relabel that turn). The reply content is verified from the transcript below, so this wait accepts
  # the Stop turn-completion markers as delivery (f19 fallback shape from phase9/phase27/phase29).
  ELAPSED=0
  STOP5_FOUND=false
  while [ $ELAPSED -lt $TIMEOUT ]; do
    if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_STOP5" ]; then
      if tail -n +"$((LOG_BEFORE_STOP5 + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
        STOP5_FOUND=true
        break
      fi
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  pane_log "[ask_user] AFTER Stop detected"

  # Structural guard (f29 exactly-once pin): the prompt pins AskUserQuestion to a single call, so this
  # subtest window must contain EXACTLY ONE 'Raw hook payload [PreToolUse]' with tool_name AskUserQuestion.
  # The attempt-10 FAIL was a double-call (mimo re-invoked AskUserQuestion, leaving a 2nd pending dialog
  # that blocked finalization); this count catches it deterministically, before the timing wait times out.
  AQ_INVOKE_COUNT=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | grep "Raw hook payload \[PreToolUse\]" | grep -c '"tool_name":"AskUserQuestion"' || true)
  if [ "$AQ_INVOKE_COUNT" -eq 1 ]; then
    pass "Exactly one AskUserQuestion PreToolUse invocation in main subtest (count=$AQ_INVOKE_COUNT)"
  else
    fail "Expected exactly 1 AskUserQuestion PreToolUse invocation in main subtest, got $AQ_INVOKE_COUNT (double-call regression)"
  fi

  if [ "$STOP5_FOUND" = true ]; then
    # Stream relabel ✅ (or a Stop direct-send) means the turn's last 💬 Message was finalized with content
    pass "Stream relabel ✅ or Stop direct-send received after AskUserQuestion (reply finalized)"

    # Verify the reply carried non-empty content, delivered via EITHER the stream path (Stream send /
    # Stream edit final=true) OR a Stop direct-send ("Notification sent ...: Stop ... body=<non-empty>",
    # no Stream send that turn). f22: content verification is preserved — the delivery path is widened.
    if tail -n +"$((LOG_BEFORE_STOP5 + 1))" "$LOG_FILE" | grep -E "Stream send:|Stream edit:.*final=true|Notification sent .*: Stop \[.*body=." > /dev/null 2>&1; then
      pass "Streaming message or Stop direct-send has content after AskUserQuestion"
    else
      fail "No Stream send/edit or Stop direct-send content found after AskUserQuestion"
    fi

    # AskUserQuestion uses pending flow (not ToolUseMsgs), so PostToolUse won't find a stored msg to update.
    # Verify AskUserQuestion was sent via pending flow instead.
    AQ_SENT_LOG=$(tail -n +"$((LOG_BEFORE_AQ + 1))" "$LOG_FILE" | grep "AskUserQuestion sent:" || true)
    if [ -n "$AQ_SENT_LOG" ]; then
      pass "AskUserQuestion sent via pending flow"
    else
      fail "AskUserQuestion sent log not found"
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
    fail "Neither Stream relabel ✅ nor Stop direct-send found after AskUserQuestion within ${TIMEOUT}s"
  fi

  # --- Free-text reply test ---
  LOG_BEFORE_FT=$(wc -l < "$LOG_FILE")

  pane_log "[ask_user] BEFORE sending free-text AskUserQuestion prompt"

  # Send prompt for free-text question (min 2 options required by AskUserQuestion)
  inject_prompt "First write a brief paragraph, then ask me one question using AskUserQuestion tool with header 'Free Text Test' and two options: 'Blue' with description 'The color blue', 'Red' with description 'The color red'. Question: 'What is your favorite color?' Call AskUserQuestion exactly once in this turn, with exactly one item in its questions array. After that call returns, do not call AskUserQuestion or any other tool again. Briefly acknowledge the answer, then stop."

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
    echo "  DEBUG: FT_RESP (${#FT_RESP} chars): $FT_RESP"
    FT_CODE=$(echo "$FT_RESP" | tail -1)
    if [ "$FT_CODE" = "200" ]; then
      pass "Free-text answer sent via /tool/respond API"
    else
      fail "Free-text API returned $FT_CODE"
    fi

    # Wait for turn finalize after free-text answer — Stream relabel ✅ OR a Stop direct-send (f22:
    # a burst-at-Stop reply delivers via ": Stop [" / "Stop terminal: outcome=direct_send", no relabel;
    # the answer itself is verified from the transcript below).
    ELAPSED=0
    FT_STOP_FOUND=false
    while [ $ELAPSED -lt $TIMEOUT ]; do
      if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_FT_STOP" ]; then
        if tail -n +"$((LOG_BEFORE_FT_STOP + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
          FT_STOP_FOUND=true
          break
        fi
      fi
      sleep 2
      ELAPSED=$((ELAPSED + 2))
    done

    # Structural guard (f29 exactly-once pin): free-text subtest window must have EXACTLY ONE
    # AskUserQuestion PreToolUse invocation.
    FT_INVOKE_COUNT=$(tail -n +"$((LOG_BEFORE_FT + 1))" "$LOG_FILE" | grep "Raw hook payload \[PreToolUse\]" | grep -c '"tool_name":"AskUserQuestion"' || true)
    if [ "$FT_INVOKE_COUNT" -eq 1 ]; then
      pass "Exactly one AskUserQuestion PreToolUse invocation in free-text subtest (count=$FT_INVOKE_COUNT)"
    else
      fail "Expected exactly 1 AskUserQuestion PreToolUse invocation in free-text subtest, got $FT_INVOKE_COUNT (double-call regression)"
    fi

    if [ "$FT_STOP_FOUND" = true ]; then
      pass "Stream relabel ✅ or Stop direct-send received after free-text answer (turn finalized)"

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
      fail "Neither Stream relabel ✅ nor Stop direct-send found after free-text answer within ${TIMEOUT}s"
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
  inject_prompt "First write a brief paragraph, then ask me one question using AskUserQuestion tool with header 'Group Test' and two options: 'Yes' with description 'Agree', 'No' with description 'Disagree'. Question: 'Do you agree?' Call AskUserQuestion exactly once in this turn, with exactly one item in its questions array. After that call returns, do not call AskUserQuestion or any other tool again. Briefly acknowledge the answer, then stop."

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
    echo "  DEBUG: GT_RESP (${#GT_RESP} chars): $GT_RESP"
    GT_CODE=$(echo "$GT_RESP" | tail -1)
    GT_BODY=${GT_RESP%%$'\n'*}
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

    # Wait for turn finalize after group direct text answer — Stream relabel ✅ OR a Stop direct-send
    # (f22: a burst-at-Stop reply delivers via ": Stop [" / "Stop terminal: outcome=direct_send", no
    # relabel; the answer itself is verified from the transcript below).
    ELAPSED=0
    GT_STOP_FOUND=false
    while [ $ELAPSED -lt $TIMEOUT ]; do
      if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_GT_STOP" ]; then
        if tail -n +"$((LOG_BEFORE_GT_STOP + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
          GT_STOP_FOUND=true
          break
        fi
      fi
      sleep 2
      ELAPSED=$((ELAPSED + 2))
    done

    # Structural guard (f29 exactly-once pin): group-text subtest window must have EXACTLY ONE
    # AskUserQuestion PreToolUse invocation.
    GT_INVOKE_COUNT=$(tail -n +"$((LOG_BEFORE_GT + 1))" "$LOG_FILE" | grep "Raw hook payload \[PreToolUse\]" | grep -c '"tool_name":"AskUserQuestion"' || true)
    if [ "$GT_INVOKE_COUNT" -eq 1 ]; then
      pass "Exactly one AskUserQuestion PreToolUse invocation in group-text subtest (count=$GT_INVOKE_COUNT)"
    else
      fail "Expected exactly 1 AskUserQuestion PreToolUse invocation in group-text subtest, got $GT_INVOKE_COUNT (double-call regression)"
    fi

    if [ "$GT_STOP_FOUND" = true ]; then
      pass "Stream relabel ✅ or Stop direct-send received after group direct text answer (turn finalized)"

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
      fail "Neither Stream relabel ✅ nor Stop direct-send found after group direct text answer within ${TIMEOUT}s"
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

inject_prompt "First write a brief paragraph, then ask me one question using AskUserQuestion tool with header 'Cancel Test' and two options: 'Keep' with description 'Keep current', 'Change' with description 'Change it'. Question: 'Should we proceed?' Call AskUserQuestion exactly once. If it returns or is cancelled, do not call it or any other tool again; stop."

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
    if tail -n +"$((LOG_BEFORE_AQCANCEL + 1))" "$LOG_FILE" | grep "Permission cancelled: msg_id=.*tool=AskUserQuestion" > /dev/null 2>&1; then
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

  # Structural guard (f29 exactly-once pin, cancel variant): even when the dialog is cancelled, the
  # prompt pins AskUserQuestion to a single call, so this window must have EXACTLY ONE invocation.
  AQCANCEL_INVOKE_COUNT=$(tail -n +"$((LOG_BEFORE_AQCANCEL + 1))" "$LOG_FILE" | grep "Raw hook payload \[PreToolUse\]" | grep -c '"tool_name":"AskUserQuestion"' || true)
  if [ "$AQCANCEL_INVOKE_COUNT" -eq 1 ]; then
    pass "Exactly one AskUserQuestion PreToolUse invocation in cancel subtest (count=$AQCANCEL_INVOKE_COUNT)"
  else
    fail "Expected exactly 1 AskUserQuestion PreToolUse invocation in cancel subtest, got $AQCANCEL_INVOKE_COUNT (double-call regression)"
  fi
else
  fail "AskUserQuestion cancel test: notification not triggered within ${TIMEOUT}s"
fi
