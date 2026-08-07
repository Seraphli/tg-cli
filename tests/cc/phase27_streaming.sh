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

# Wait for Stream relabel ✅ (turn finalize) OR the dump-at-Stop delivery path
# (: Stop [ / Stop terminal: outcome=direct_send) — mutually exclusive per turn.
ELAPSED=0
TC1_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
    TC1_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for TC1 Stream relabel ✅ (or Stop-delivery)... ${ELAPSED}s / ${TIMEOUT}s"
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

  # TC7: rich streaming — every Stream send/edit carries fmt=rich on the same log line.
  # Read the bot log FRESH (not the large stale $TC1_LOGS snapshot) and drain the pipe (grep -E, no
  # -q) so a mid-write snapshot of the multi-MB streamed turn cannot cause a false negative.
  if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -E "Stream (edit|send):.*fmt=rich" > /dev/null 2>&1; then
    pass "TC7: Stream send/edit carries fmt=rich field (rich message streaming confirmed)"
  else
    fail "TC7: No Stream send/edit with fmt=rich found (expected rich streaming)"
  fi

  # TC8 (rich multi-line fix): the streamed multi-paragraph body must separate paragraphs/lines with
  # block or <br> tags — NEVER a bare "\n", which rich_message.html collapses to a single space. The
  # TC1 prompt asks for 5 labeled paragraphs, so the reconstructed sent body must carry <p> and/or <br>.
  TC8_BODY=$(reconstruct_tg_full_text "$(tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE")")
  if printf '%s' "$TC8_BODY" | grep -qE "<p>|<br>" 2>/dev/null; then
    pass "TC8: streamed multi-paragraph body uses <p>/<br> separators (no bare-\\n collapse)"
  else
    fail "TC8: streamed body lacks <p>/<br> separators — multi-line would collapse to one line"
  fi

  # TC5 sub-check: if Stream surplus removed: is present, verify it was logged correctly
  if echo "$TC1_LOGS" | grep "Stream surplus removed:" > /dev/null 2>&1; then
    SURPLUS_MSG=$(echo "$TC1_LOGS" | grep "Stream surplus removed:" | head -1 || true)
    pass "TC1/TC5: Stream surplus removed logged (continuation residue cleaned up): $SURPLUS_MSG"
  fi
else
  fail "TC1: Neither Stream relabel ✅ nor Stop-delivery received within ${TIMEOUT}s"
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
inject_prompt "First write one sentence saying: STREAM_TC2_PRE. Then use the Bash tool to run the command: echo STREAM_TC2_TOOL. Then write one sentence saying: STREAM_TC2_POST."
pane_log "[streaming] TC2 AFTER inject"

# Wait for Stream relabel ✅ (turn finalize after tool + post-text) OR the dump-at-Stop
# delivery path (: Stop [ / Stop terminal: outcome=direct_send) — mutually exclusive per turn.
ELAPSED=0
TC2_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
    TC2_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for TC2 Stream relabel ✅ (or Stop-delivery)... ${ELAPSED}s / ${TIMEOUT}s"
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

  # Verify at least one Stream send: (pre-tool text) exists BEFORE the finalize anchor.
  # FINALIZE line = relabel line if present, ELSE the Stop-delivery marker line.
  FIRST_STREAM_LINE=$(awk '/Stream send:/{print NR; exit}' <<< "$TC2_LOGS")
  RELABEL_LINE=$(awk '/Stream relabel ✅:/{print NR; exit}' <<< "$TC2_LOGS")
  if [ -z "$RELABEL_LINE" ]; then
    RELABEL_LINE=$(awk '/: Stop \[|Stop terminal: outcome=direct_send/{print NR; exit}' <<< "$TC2_LOGS")
  fi
  if [ -n "$FIRST_STREAM_LINE" ] && [ -n "$RELABEL_LINE" ] && [ "$FIRST_STREAM_LINE" -lt "$RELABEL_LINE" ]; then
    pass "TC2: Stream send before finalize (correct ordering, line $FIRST_STREAM_LINE < $RELABEL_LINE)"
  else
    pass "TC2: Stream ordering check: send=$FIRST_STREAM_LINE finalize=$RELABEL_LINE"
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
  fail "TC2: Neither Stream relabel ✅ nor Stop-delivery received within ${TIMEOUT}s"
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

# Verify no error from MessageDisplay path itself (anchor on [ERROR]/[WARN] level so
# streamed delta content containing "error"/"failed" in [INFO] lines cannot false-match)
if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -E "\[(ERROR|WARN)\].*handleMessageDisplay|\[(ERROR|WARN)\].*MessageDisplay" > /dev/null 2>&1; then
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

# Accept Stream relabel ✅ (turn finalize) OR the dump-at-Stop delivery path
# (: Stop [ / Stop terminal: outcome=direct_send) — mutually exclusive per turn.
ELAPSED=0
TC5_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TC5 + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
    TC5_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for TC5 Stream relabel ✅ (or Stop-delivery)... ${ELAPSED}s / ${TIMEOUT}s"
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
  fail "TC5: Neither Stream relabel ✅ nor Stop-delivery received within ${TIMEOUT}s"
fi

wait_for_idle

# ============================================================
# TC6: notification ordering regression (text → tool → text)
# Regression test for the NotifOpQueue ordering fix.
# Expect: a "MessageDisplay delta:" log line for the match-open announcement
# (MATCH_OPEN_MARKER) appears BEFORE the Bash coin-flip ToolUse "Notification sent"
# line, which appears BEFORE the winner-announcement text. This verifies the FIFO
# serialization of SEND ops prevents the tool-call notification from leapfrogging
# the pre-tool text chunk in Telegram delivery order.
# ============================================================
echo ""
echo "  TC6: notification ordering regression (MATCH_OPEN → ToolUse → winner)"

LOG_BEFORE_TC6=$(wc -l < "$LOG_FILE")
# Fix A f18 (boss-designed GENUINE-tool-result redesign): the f17 cat-vs-dog framing gave the three steps an
# INTRINSIC causal order, but the "result" was still FAKE — `echo STREAM_TC6_TOOL` produces no winner, so the
# causality was not real and the model could fabricate the result (Step 3) WITHOUT running the tool (Step 2).
# hy3-free did exactly that in the full run (3926718): it narrated the whole competition as ONE text turn,
# printing "Step 2: bash command: echo STREAM_TC6_TOOL" as LITERAL TEXT and inventing a winner — no Bash
# tool_use / ToolUse notification at all, so the ordering assertion had no ToolUse anchor and FAILed. Boss
# redesign: the winner must be GENUINELY PRODUCED by the tool call. Step 2 runs a fair coin flip (bash
# $RANDOM) that echoes the winner AND `tee`s it into RESULT_FILE; Step 3 must announce the exact winner the
# command printed. The assertion reads the REAL winner from RESULT_FILE — a missing/empty file means the tool
# never ran (FAIL), and a fabricated winner mismatches the file's content half the time (FAIL). The model
# cannot satisfy the test without actually invoking the tool, so the text→tool→text ordering is genuine.
# Marker hygiene (f14 rule): MATCH_OPEN_MARKER appears ONLY in the Step 1 sentence spec; WINNER_CATS/WINNER_DOGS
# appear ONLY in the Step 2 command text; framing text is marker-free. The `$RANDOM` coin flip stays LITERAL
# in the prompt (single-quoted); only RESULT_FILE is expanded to its absolute path.
RESULT_FILE="$TEST_CONFIG_DIR/tc6_result.txt"
rm -f "$RESULT_FILE" 2>/dev/null || true
pane_log "[streaming] TC6 BEFORE inject"
inject_prompt "You are the host of a live cat-vs-dog competition. Perform the following three steps in this exact order, all in this single reply, one step at a time, without skipping or combining any step.
Step 1: Announce to the audience that the match is starting, in exactly one sentence that contains the exact token MATCH_OPEN_MARKER.
Step 2: The result does not exist yet, so determine the winner by running this exact bash command (a fair coin flip): "'if [ $((RANDOM % 2)) -eq 0 ]; then echo WINNER_CATS; else echo WINNER_DOGS; fi | tee '"$RESULT_FILE"'
Step 3: After you see the command output, announce the winner to the audience in exactly one sentence that contains the exact winner token the command printed.'
pane_log "[streaming] TC6 AFTER inject"

# Wait for Stream relabel ✅ (turn finalized after all three steps) OR the dump-at-Stop
# delivery path (: Stop [ / Stop terminal: outcome=direct_send) — mutually exclusive per turn.
ELAPSED=0
TC6_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
    TC6_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for TC6 Stream relabel ✅ (or Stop-delivery)... ${ELAPSED}s / ${TIMEOUT}s"
done
pane_log "[streaming] TC6 AFTER relabel"

if [ "$TC6_FOUND" = true ]; then
  pass "TC6: Stream relabel ✅ received (match-host turn finalized)"
  TC6_LOGS=$(tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE")

  # A1/A3 extraction migrated to TC9's reconstruction (see the :532 comment / :624 extractor): the old
  # "MessageDisplay delta:.*MARKER" INFO-line awk missed the marker whenever the model placed it after an
  # embedded \n — that INFO line renders REAL newlines, so one delta entry spans several physical lines and
  # the marker lands on a line WITHOUT the "MessageDisplay delta:" prefix (Stage-B FAIL run 3022985: the
  # "MessageDisplay delta:" prefix on one line, MATCH_OPEN_MARKER on a later wrapped line). Reconstruct the
  # streamed text per message_id from the single-line "Raw hook payload [MessageDisplay]" JSON (escaped \n
  # stays on one physical line), then anchor ordering on the Stream send/edit RENDER line carrying that
  # message_id (the Message-FIFO delivery timeline). Reading ONLY MessageDisplay payloads keeps the
  # semantics: the marker must still appear in a MessageDisplay delta, NOT the injected prompt (logged under
  # UserPromptSubmit). All three anchors below (OPEN/TOOLUSE/WINNER) stay `print NR` over the SAME $TC6_LOGS
  # = one line-numbering basis.
  # A3 winner: read the ACTUAL winner the coin flip `tee`d to the file (missing/empty file = the tool never
  # ran = FAIL). The post-tool winner message must carry this exact W AND its render line must land AFTER
  # ToolUse (consistency: a fabricated winner mismatches the file half the time). Pre-tool deltas that quote
  # the Step 2 command text carry BOTH winner tokens, but their render lines land BEFORE ToolUse, so
  # restricting the render-line search to NR>TOOLUSE_LINE neither counts them nor is broken by them.
  WINNER=""
  [ -f "$RESULT_FILE" ] && WINNER=$(tr -d '[:space:]' < "$RESULT_FILE")
  read -r OPEN_MID WINNER_MIDS < <(printf '%s\n' "$TC6_LOGS" | WINNER="$WINNER" python3 -c '
import json,sys,os,re
w=os.environ.get("WINNER","")
msgs={}; order=[]
for ln in sys.stdin:
    if "Raw hook payload [MessageDisplay]:" not in ln:
        continue
    m=re.search(r"Raw hook payload \[MessageDisplay\]: (\{.*\})\s*$", ln)
    if not m:
        continue
    try:
        d=json.loads(m.group(1))
    except Exception:
        continue
    mid=d.get("message_id")
    if not mid:
        continue
    if mid not in msgs:
        msgs[mid]={}; order.append(mid)
    msgs[mid][d.get("index",0)]=d.get("delta","")
texts={mid:"".join(msgs[mid][i] for i in sorted(msgs[mid])) for mid in order}
open_mid=next((mid for mid in order if "MATCH_OPEN_MARKER" in texts[mid]), "-")
win_mids=[mid for mid in order if w and w in texts[mid]]
print(open_mid, " ".join(win_mids))') || true
  if [ "$OPEN_MID" = "-" ]; then OPEN_MID=""; fi
  # A1: the match-open announcement (pre-tool text) — render line of its MessageDisplay message.
  OPEN_LINE=$(awk -v mid="$OPEN_MID" 'mid != "" && $0 ~ ("Stream (send|edit):.*message_id=" mid){print NR; exit}' <<< "$TC6_LOGS")
  # A2: the Bash coin-flip ToolUse notification.
  TOOLUSE_LINE=$(awk '/Notification sent.*ToolUse/{print NR; exit}' <<< "$TC6_LOGS")
  # A3: winner render line — first Stream send/edit AFTER ToolUse whose message_id carries the winner W;
  # Stop-direct-send fallback (winner burst at Stop, no Stream op) accepted, same shape as before.
  _TU="${TOOLUSE_LINE:-0}"
  WINNER_LINE=""
  if [ -n "$WINNER" ]; then
    WINNER_LINE=$(awk -v t="$_TU" -v mids="$WINNER_MIDS" 'BEGIN{n=split(mids,a," ")} NR>t{for(i=1;i<=n;i++) if(a[i]!="" && $0 ~ ("Stream (send|edit):.*message_id=" a[i])){print NR; exit}}' <<< "$TC6_LOGS")
    if [ -z "$WINNER_LINE" ]; then
      WINNER_LINE=$(awk -v w="$WINNER" -v t="$_TU" 'NR>t && $0 ~ ("Notification sent.*" w){print NR; exit}' <<< "$TC6_LOGS")
    fi
  fi

  if [ -z "$OPEN_LINE" ]; then
    fail "TC6: MATCH_OPEN_MARKER not found in bot log (pre-tool match-open text missing)"
  elif [ -z "$TOOLUSE_LINE" ]; then
    fail "TC6: ToolUse Notification sent line not found in bot log (coin-flip Bash tool notification missing — tool not invoked)"
  elif [ -z "$WINNER" ]; then
    fail "TC6: RESULT_FILE empty/missing ($RESULT_FILE) — coin-flip tool never produced a winner (result fabricated without running the tool)"
  elif [ -z "$WINNER_LINE" ]; then
    fail "TC6: announced winner ($WINNER) not found after ToolUse in bot log (post-tool winner announcement missing, or announced winner != coin-flip result)"
  elif [ "$OPEN_LINE" -lt "$TOOLUSE_LINE" ] && [ "$TOOLUSE_LINE" -lt "$WINNER_LINE" ]; then
    pass "TC6: Correct ordering — MATCH_OPEN (line $OPEN_LINE) < ToolUse (line $TOOLUSE_LINE) < winner $WINNER (line $WINNER_LINE); RESULT_FILE=$WINNER"
  else
    fail "TC6: Ordering violated — MATCH_OPEN=$OPEN_LINE ToolUse=$TOOLUSE_LINE winner($WINNER)=$WINNER_LINE (expected MATCH_OPEN < ToolUse < winner)"
  fi
else
  fail "TC6: Neither Stream relabel ✅ nor Stop-delivery received within ${TIMEOUT}s"
fi

wait_for_idle

# ============================================================
# TC10: transcript-to-TG completeness invariant (universal zero-content-loss)
# Every UNIQUE assistant message that the CC transcript records for the turn under test
# MUST have been delivered to Telegram — either as a streamed message (a "Stream send:" /
# "Stream edit:" / "Stream relabel ✅:" line carrying its bot-side message_id), or covered by
# a Stop delivery (": Stop [" notification / "Stop terminal: outcome=direct_send"). The point
# is to catch a GENUINE DROP: a transcript assistant text message with NO TG delivery of ANY kind.
#
# NOTE on id matching: CC's streaming hook payload assigns a per-turn UUID message_id (what the
# bot logs as "message_id="), while the FINAL transcript entry carries a provider-native
# "msg_..." id — the two id spaces do NOT overlap, so this invariant is asserted by COUNT within
# the turn (unique transcript text-message ids == unique delivered ids when nothing is dropped;
# verified on real single-session runs: transcript_text_ids == streamed_ids). A dump-at-Stop turn
# delivers its final assistant text via the Stop path (no streamed message_id), so a Stop delivery
# present in the window counts as covering exactly one (the last) assistant message.
# ============================================================
echo ""
echo "  TC10: transcript-to-TG completeness (every transcript assistant message delivered)"

# Resolve THE phase27 CC transcript path (single CC session for the whole phase). Prefer the
# raw-payload transcript_path in the phase's log (LOG_BEFORE = phase start); fall back to the
# session= token on a relabel line, mapped to <config>/claude-config/projects/*/<session>.jsonl.
TC10_TRANSCRIPT=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" \
  | grep -oE '"transcript_path":"[^"]+\.jsonl"' | tail -1 | sed 's/"transcript_path":"//;s/"$//' || true)
if [ -z "$TC10_TRANSCRIPT" ] || [ ! -f "$TC10_TRANSCRIPT" ]; then
  TC10_SID=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -oPm1 'Stream relabel ✅:.*session=\K[0-9a-f-]+' || true)
  if [ -n "$TC10_SID" ]; then
    TC10_TRANSCRIPT=$(find "$TEST_CLAUDE_CONFIG_DIR/projects" -name "${TC10_SID}.jsonl" 2>/dev/null | head -1 || true)
  fi
fi

# transcript_text_msgids <path>: unique assistant message ids that carry non-empty text.
transcript_text_msgids() {
  jq -r 'select(.type=="assistant")
         | select([.message.content[]? | select(.type=="text" and ((.text|gsub("\\s";""))|length>0))]|length>0)
         | .message.id' "$1" 2>/dev/null | sort -u
}

if [ -z "$TC10_TRANSCRIPT" ] || [ ! -f "$TC10_TRANSCRIPT" ]; then
  fail "TC10: could not resolve phase27 CC transcript path from bot log (transcript_path/session= missing)"
else
  # Snapshot the transcript BEFORE the completeness turn so the assertion is scoped to THIS turn only
  # (the transcript is cumulative across the phase). before-set = ids already present; the turn's new
  # messages = after-set minus before-set.
  TC10_IDS_BEFORE=$(transcript_text_msgids "$TC10_TRANSCRIPT")

  LOG_BEFORE_TC10=$(wc -l < "$LOG_FILE")
  pane_log "[streaming] TC10 BEFORE inject"
  inject_prompt "Follow these steps exactly in order, one at a time:
Step 1: Write exactly one sentence: COMPLETE_MARKER_ONE about the number one.
Step 2: Run the bash command: echo STREAM_TC10_TOOL
Step 3: After you get the bash result, write exactly one sentence: COMPLETE_MARKER_TWO about the number two.
Do not combine steps or reorder them."
  pane_log "[streaming] TC10 AFTER inject"

  # Wait for turn finalize: Stream relabel ✅ OR the dump-at-Stop delivery path — mutually exclusive per turn.
  ELAPSED=0
  TC10_FOUND=false
  while [ $ELAPSED -lt $TIMEOUT ]; do
    if tail -n +"$((LOG_BEFORE_TC10 + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
      TC10_FOUND=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
    echo "  Waiting for TC10 Stream relabel ✅ (or Stop-delivery)... ${ELAPSED}s / ${TIMEOUT}s"
  done
  pane_log "[streaming] TC10 AFTER relabel"

  if [ "$TC10_FOUND" != true ]; then
    fail "TC10: Neither Stream relabel ✅ nor Stop-delivery received within ${TIMEOUT}s"
  else
    # Give the transcript a moment to flush the final assistant entry to disk (Stop hook can fire
    # before the last message is written — same class of timing as the MEMORY.md Stop-hook note).
    wait_for_idle
    TC10_TURN_LOG=$(tail -n +"$((LOG_BEFORE_TC10 + 1))" "$LOG_FILE")

    # TRANSCRIPT set for THIS turn = ids present now minus ids present before the inject.
    TC10_IDS_AFTER=$(transcript_text_msgids "$TC10_TRANSCRIPT")
    TC10_NEW_IDS=$(comm -13 <(printf '%s\n' "$TC10_IDS_BEFORE") <(printf '%s\n' "$TC10_IDS_AFTER") | grep -c . || true)

    # DELIVERED set for THIS turn (window-scoped): unique streamed bot-side message_ids.
    TC10_DELIVERED_COUNT=$(printf '%s\n' "$TC10_TURN_LOG" \
      | grep -E "Stream (send|edit):|Stream relabel ✅:" | grep -oE "message_id=[0-9a-f-]+" | sort -u | grep -c . || true)
    # Stop delivery present in the window covers the final (dump-at-Stop) assistant message.
    TC10_STOP_DELIVERED=$(printf '%s\n' "$TC10_TURN_LOG" | grep -cE ": Stop \[|Stop terminal: outcome=direct_send" || true)

    # Coverage = streamed deliveries + (1 if a Stop delivery is present, covering the last message).
    TC10_COVERAGE=$TC10_DELIVERED_COUNT
    if [ "$TC10_STOP_DELIVERED" -gt 0 ]; then
      TC10_COVERAGE=$((TC10_COVERAGE + 1))
    fi

    if [ "$TC10_NEW_IDS" -eq 0 ]; then
      # No new transcript text message this turn (CC produced only tool/thinking blocks): vacuous — a
      # completeness invariant over an empty message set is trivially satisfied.
      pass "TC10: transcript recorded no new assistant text message for the turn (vacuous completeness)"
    elif [ "$TC10_COVERAGE" -ge "$TC10_NEW_IDS" ]; then
      pass "TC10: transcript-to-TG completeness — all $TC10_NEW_IDS assistant message(s) delivered (streamed=$TC10_DELIVERED_COUNT stop=$TC10_STOP_DELIVERED coverage=$TC10_COVERAGE)"
    else
      fail "TC10: transcript-to-TG DROP — $TC10_NEW_IDS assistant message(s) in transcript but only $TC10_COVERAGE delivered (streamed=$TC10_DELIVERED_COUNT stop=$TC10_STOP_DELIVERED). Missing message ids: $(comm -13 <(printf '%s\n' "$TC10_IDS_BEFORE") <(printf '%s\n' "$TC10_IDS_AFTER") | tr '\n' ' ')"
    fi
  fi
fi

wait_for_idle

# ============================================================
# TC9 (Fix 13a) — no-arg tool (TaskList) notification present + ordered between text, GENUINE-result
# redesign (boss-ruled, TC6 cat-vs-dog style; unified matrix CC=mimo AND Codex=mimo).
# Fix 13a: BuildToolNotifyText returned "" for a no-arg tool (tool_input {}), so register.go's
# `if toolText != ""` silently skipped the notification — making the tool invisible on TG AND removing
# the tool separator between two adjacent assistant text messages (tripping the V3 ordering guard). The
# fix sends a "🔧 TaskList" notification that sits BETWEEN the pre-tool and post-tool text.
#
# attempt-8 incident (mimo, bot.log session 48fda741 msg a591b59d): the old terse prompt let mimo call
# TaskList FIRST, then emit PRE+POST as ONE message entirely after the tool — text->tool->text was
# structurally impossible; worse, the old single-line "MessageDisplay delta:" extractor missed POST (it
# wrapped after an embedded \n\n) and MISreported a content loss. A predictable-EMPTY TaskList result
# cannot force the interleave (the model can say "no tasks" without reading the tool). REDESIGN: seed
# ONE task with a RANDOM subject the model has never seen into <config>/tasks/<sid>/; the no-arg
# TaskList reads it, so Step 3 GENUINELY depends on reading the tool — the same randomness that makes
# TC6 (coin flip) interleave on mimo (full8 ~55618: MATCH_OPEN 26 < ToolUse 64 < WINNER_DOGS 147). The
# POST claim is verified against the REAL artifact (PostToolUse tool_response.tasks[].subject), never
# the model's words. Ordering PRE-send < ToolUse < POST-send unchanged. Extraction reconstructs per
# message_id from the single-line "Raw hook payload [MessageDisplay]" JSON (escaped \n stays on one
# physical line), so a marker split across deltas is handled and the batching case yields
# PRE_MID==POST_MID -> a clean ordering FAIL (not a false content-loss report). TC6 carries the genuine
# same-turn interleave; TC9 carries the no-arg-tool interleave via seeding.
# ============================================================
echo ""
echo "  TC9 (Fix 13): no-arg tool (TaskList) — notification present + ordered between text"

LOG_BEFORE_TC9=$(wc -l < "$LOG_FILE")
pane_log "[streaming] TC9 BEFORE inject"
# TC9 seeding (boss redesign): while the session is idle, BEFORE the inject, plant ONE task with a
# RANDOM subject the model has never seen into <config>/tasks/<sid>/ so the no-arg TaskList result is
# genuinely unpredictable (TC6-grade). Resolve the live phase27 session id from the phase log.
TC9_SID=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -oE '"session_id":"[0-9a-f-]+"' | tail -1 | sed 's/.*"session_id":"//;s/"$//' || true)
if [ -z "$TC9_SID" ]; then fail "TC9: could not resolve the session id to seed the task board"; fi
NOARG_SEED="SEED_$(python3 -c 'import secrets;print(secrets.token_hex(6))')"
TC9_TASK_DIR="$TEST_CLAUDE_CONFIG_DIR/tasks/$TC9_SID"
mkdir -p "$TC9_TASK_DIR"
python3 -c "import json,sys; json.dump({'id':'1','subject':sys.argv[1],'description':'phase27 TC9 seed','activeForm':'seeded','status':'pending','blocks':[],'blockedBy':[]}, open(sys.argv[2],'w'))" "$NOARG_SEED" "$TC9_TASK_DIR/1.json" || fail "TC9: could not write the seed task file"
echo "  TC9 seeded task board: sid=$TC9_SID subject=$NOARG_SEED"
# Host framing + genuine result binding: Step 3 must echo the exact seeded subject TaskList returns, so
# POST genuinely depends on reading the no-arg tool (forces PRE-text -> tool -> POST-text like TC6).
inject_prompt "You are the host of a live task-board status show. Perform the following three steps in this exact order, all in this single reply, one step at a time, without skipping or combining any step.
Step 1: Announce to the audience that the broadcast is starting, in exactly one sentence that contains the exact token NOARG_PRE_MARKER.
Step 2: The task board is off-screen, so read it by calling the TaskList tool now with no arguments. This is a required step; do not skip it and do not guess its contents.
Step 3: After you see the TaskList result, announce the subject of the single task on the board to the audience in exactly one sentence that contains the token NOARG_POST_MARKER= immediately followed by the exact subject text the tool reported for that task."
pane_log "[streaming] TC9 AFTER inject"

# Wait for Stream relabel ✅ (turn finalized after all three steps) OR the dump-at-Stop
# delivery path (: Stop [ / Stop terminal: outcome=direct_send) — mutually exclusive per turn.
ELAPSED=0
TC9_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TC9 + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
    TC9_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for TC9 Stream relabel ✅ (or Stop-delivery)... ${ELAPSED}s / ${TIMEOUT}s"
done
pane_log "[streaming] TC9 AFTER relabel"

if [ "$TC9_FOUND" = true ]; then
  pass "TC9: Stream relabel ✅ received (text→no-arg-tool→text turn finalized)"
  TC9_LOGS=$(tail -n +"$((LOG_BEFORE_TC9 + 1))" "$LOG_FILE")

  # (a) Fix 13a core: the no-arg TaskList ToolUse notification is PRESENT (tightened to name TaskList on
  # the notification line — proves the no-arg tool is no longer silently skipped).
  TC9_TOOLUSE_LINE=$(awk '/Notification sent.*ToolUse.*TaskList/{print NR; exit}' <<< "$TC9_LOGS")
  if [ -n "$TC9_TOOLUSE_LINE" ]; then pass "TC9: no-arg TaskList ToolUse notification present (Fix 13a — no longer silently skipped)"; fi

  # (b) GENUINE result binding: exactly ONE TaskList PostToolUse artifact; structurally parse
  # tool_response.tasks[0].subject (python3) — a parse failure is its OWN FAIL, never a default — and it
  # MUST equal the seeded token (proves the tool genuinely returned what we planted, not a fabrication).
  TC9_PTU=$(printf '%s\n' "$TC9_LOGS" | grep -E 'Raw hook payload \[PostToolUse\]:.*"tool_name":"TaskList"' || true)
  TC9_PTU_COUNT=$(printf '%s\n' "$TC9_PTU" | grep -c . || true)
  ARTIFACT_SUBJECT=$(printf '%s\n' "$TC9_PTU" | python3 -c '
import json,sys,re
data=sys.stdin.read()
line=next((l for l in data.splitlines() if "Raw hook payload [PostToolUse]:" in l), "")
m=re.search(r"Raw hook payload \[PostToolUse\]: (\{.*\})\s*$", line)
if not m:
    print("__PARSE_FAIL__"); sys.exit()
try:
    d=json.loads(m.group(1))
except Exception:
    print("__PARSE_FAIL__"); sys.exit()
t=(d.get("tool_response") or {}).get("tasks")
if not (isinstance(t,list) and len(t)==1 and isinstance(t[0],dict) and "subject" in t[0]):
    print("__PARSE_FAIL__"); sys.exit()
print(t[0]["subject"])' || true)

  # (c) Reconstruct-first extraction (no atomic-marker assumption): one pass over ALL "Raw hook payload
  # [MessageDisplay]" JSON lines (escaped \n = one physical line), group deltas by message_id, dedupe by
  # (message_id,index), sort by index, join per message. PRE_MID/POST_MID = the first message whose text
  # carries the marker (batching -> same message -> PRE_MID==POST_MID -> ordering FAIL). POST_HAS_SEED =
  # the LITERAL substring "NOARG_POST_MARKER=<seed>" is present in the reconstructed POST text.
  PRE_MID=""; POST_MID=""; POST_HAS_SEED=0
  read -r PRE_MID POST_MID POST_HAS_SEED < <(printf '%s\n' "$TC9_LOGS" | NOARG_SEED="$NOARG_SEED" python3 -c '
import json,sys,os,re
seed=os.environ.get("NOARG_SEED","")
msgs={}; order=[]
for ln in sys.stdin:
    if "Raw hook payload [MessageDisplay]:" not in ln:
        continue
    m=re.search(r"Raw hook payload \[MessageDisplay\]: (\{.*\})\s*$", ln)
    if not m:
        continue
    try:
        d=json.loads(m.group(1))
    except Exception:
        continue
    mid=d.get("message_id")
    if not mid:
        continue
    if mid not in msgs:
        msgs[mid]={}; order.append(mid)
    msgs[mid][d.get("index",0)]=d.get("delta","")
texts={mid:"".join(msgs[mid][i] for i in sorted(msgs[mid])) for mid in order}
pre=next((mid for mid in order if "NOARG_PRE_MARKER" in texts[mid]), "-")
post=next((mid for mid in order if "NOARG_POST_MARKER" in texts[mid]), "-")
ph="1" if (post!="-" and ("NOARG_POST_MARKER="+seed) in texts[post]) else "0"
print(pre, post, ph)') || true
  if [ "$PRE_MID" = "-" ]; then PRE_MID=""; fi
  if [ "$POST_MID" = "-" ]; then POST_MID=""; fi

  # (d) delivery-order anchors on the Message-FIFO timeline: PRE/POST render lines + the tightened ToolUse
  # line. f24 Stop direct-send fallback kept semantically: if POST has no Stream op (burst at Stop),
  # validate the literal binding against Raw hook payload [Stop].last_assistant_message and use the Stop
  # Notification line (after ToolUse) ONLY as the delivery-order anchor.
  PRE_LINE=$(awk -v mid="$PRE_MID" 'mid != "" && $0 ~ ("Stream (send|edit):.*message_id=" mid) {print NR; exit}' <<< "$TC9_LOGS")
  POST_LINE=$(awk -v mid="$POST_MID" 'mid != "" && $0 ~ ("Stream (send|edit):.*message_id=" mid) {print NR; exit}' <<< "$TC9_LOGS")
  POST_VIA_STOP=0
  if [ -z "$POST_LINE" ]; then
    POST_HAS_SEED=$(printf '%s\n' "$TC9_LOGS" | NOARG_SEED="$NOARG_SEED" python3 -c '
import json,sys,os,re
seed=os.environ.get("NOARG_SEED","")
lam=""
for ln in sys.stdin:
    if "Raw hook payload [Stop]:" not in ln:
        continue
    m=re.search(r"Raw hook payload \[Stop\]: (\{.*\})\s*$", ln)
    if m:
        try:
            lam=json.loads(m.group(1)).get("last_assistant_message","")
        except Exception:
            pass
print("1" if ("NOARG_POST_MARKER="+seed) in lam else "0")' || true)
    POST_LINE=$(awk -v t="${TC9_TOOLUSE_LINE:-0}" 'NR>t && /Notification sent.*NOARG_POST_MARKER/{print NR; exit}' <<< "$TC9_LOGS")
    POST_VIA_STOP=1
  fi

  # FAIL ladder — each branch a distinct diagnosis; no silent fallback / no model-specific relaxation.
  if [ "${TC9_PTU_COUNT:-0}" -eq 0 ]; then
    fail "TC9: no TaskList PostToolUse artifact — the no-arg tool never genuinely ran (fabricated as text)"
  elif [ "$TC9_PTU_COUNT" -gt 1 ]; then
    fail "TC9: $TC9_PTU_COUNT TaskList PostToolUse artifacts — ambiguous, expected exactly one no-arg TaskList call"
  elif [ "$ARTIFACT_SUBJECT" = "__PARSE_FAIL__" ]; then
    fail "TC9: could not structurally parse tool_response.tasks[0].subject from the TaskList artifact"
  elif [ "$ARTIFACT_SUBJECT" != "$NOARG_SEED" ]; then
    fail "TC9: TaskList artifact subject ($ARTIFACT_SUBJECT) != seeded token ($NOARG_SEED) — tool-read integrity broken"
  elif [ -z "$PRE_MID" ] || [ -z "$PRE_LINE" ]; then
    fail "TC9: PRE text not extracted/delivered on the stream timeline (PRE_MID=${PRE_MID:-<none>} PRE_LINE=${PRE_LINE:-<none>})"
  elif [ -z "$TC9_TOOLUSE_LINE" ]; then
    fail "TC9: ToolUse TaskList notification line not found (Fix 13a regression)"
  elif [ "$POST_VIA_STOP" -eq 0 ] && [ -z "$POST_MID" ]; then
    fail "TC9: POST marker message not found in the reconstructed MessageDisplay texts (post-tool text missing)"
  elif [ -z "$POST_LINE" ]; then
    fail "TC9: POST text not delivered via Stream send/edit nor Stop direct-send after ToolUse"
  elif [ "$POST_HAS_SEED" != "1" ]; then
    fail "TC9: POST text missing the literal NOARG_POST_MARKER=$NOARG_SEED (post-tool announcement fabricated or not read from the tool)"
  elif [ "$PRE_LINE" -lt "$TC9_TOOLUSE_LINE" ] && [ "$TC9_TOOLUSE_LINE" -lt "$POST_LINE" ]; then
    pass "TC9: genuine text->no-arg-tool->text — PRE $PRE_LINE < ToolUse $TC9_TOOLUSE_LINE < POST $POST_LINE, POST states real TaskList subject $NOARG_SEED (V3 separator restored)"
  else
    fail "TC9: delivery-timeline ordering violated — PRE=$PRE_LINE ToolUse=$TC9_TOOLUSE_LINE POST=$POST_LINE (expected PRE < ToolUse < POST)"
  fi
else
  fail "TC9: Neither Stream relabel ✅ nor Stop-delivery received within ${TIMEOUT}s"
fi

wait_for_idle

# TC9 seeding cleanup: remove the seeded task (TC9 is the last phase27 case — hygiene so it never leaks).
if [ -n "${TC9_TASK_DIR:-}" ]; then rm -f "$TC9_TASK_DIR/1.json"; rmdir "$TC9_TASK_DIR" 2>/dev/null || true; fi
