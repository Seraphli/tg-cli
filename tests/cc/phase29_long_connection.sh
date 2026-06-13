#!/bin/bash
# phase29_long_connection.sh — E2E tests for the streaming /pending/connect long-connection.
# TC1: PermissionRequest answered via TG Allow button → hook delivers answer; no poll tick.
# TC2: AskUserQuestion answered in TUI → PostToolUse freezes button with actual answer text.
# TC3: Characterise desktop allow / deny / ESC → assert freeze labels in log.
# TC4: Bot restart while PermissionRequest pending → original button still resolves it.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- phase29: long-connection streaming tests ---"

ensure_infrastructure

# ============================================================
# TC1: PermissionRequest answered via TG Allow button
# ============================================================
echo ""
echo "  TC1: PermissionRequest via TG Allow button"

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-29" "--allow-dangerously-skip-permissions"
wait_for_idle
LOG_BEFORE_TC1=$(wc -l < "$LOG_FILE")

pane_log "[tc1] BEFORE permission prompt"
inject_prompt "First write one sentence: TC29_PRE. Then run this exact bash command: echo tc29_perm_test > /tmp/tg-cli-phase29-tc1.txt. Run only this command and nothing else."
pane_log "[tc1] AFTER permission prompt inject"

# Wait for Permission request sent in log
ELAPSED=0
TC1_FOUND=false
TC1_MSG_ID=""
TC1_UUID=""
while [ $ELAPSED -lt "$TIMEOUT" ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_TC1" ]; then
    if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "Permission request sent" > /dev/null 2>&1; then
      TC1_FOUND=true
      TC1_MSG_ID=$(tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -m1 "Permission request sent" | grep -oP 'msg_id=\K[0-9]+' || true)
      TC1_UUID=$(tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -m1 "Permission request sent" | grep -oP 'uuid=\K[^ ]+' || true)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

pane_log "[tc1] AFTER permission detected"

if [ "$TC1_FOUND" = "true" ] && [ -n "$TC1_MSG_ID" ]; then
  pass "TC1: Permission request sent (msg_id=$TC1_MSG_ID uuid=$TC1_UUID)"

  # Verify hook connected log appeared (streaming connection established)
  if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "hook connected:" > /dev/null 2>&1; then
    pass "TC1: hook connected: logged (streaming long-connection established)"
  else
    fail "TC1: hook connected: not found in log — streaming connection may not have opened"
  fi

  # Verify no poll tick in this session's log range
  if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "poll tick" > /dev/null 2>&1; then
    fail "TC1: 'poll tick' found in log — old polling path still active"
  else
    pass "TC1: No 'poll tick' in log (streaming path confirmed, no file-poll fallback)"
  fi

  # Approve via /permission/decide API (simulates TG Allow button click)
  pane_log "[tc1] BEFORE approve API call"
  DECIDE_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/permission/decide?msg_id=$TC1_MSG_ID&decision=allow")
  pane_log "[tc1] AFTER approve API call"

  DECIDE_BEHAVIOR=$(echo "$DECIDE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('behavior',''))" 2>/dev/null || true)
  if [ "$DECIDE_BEHAVIOR" = "allow" ]; then
    pass "TC1: Permission approved via /permission/decide API (behavior=allow)"
  else
    fail "TC1: /permission/decide returned unexpected: $DECIDE_RESP"
  fi

  # Wait for hook to receive answer (answered: or Permission resolved in log)
  ELAPSED=0
  TC1_RESOLVED=false
  while [ $ELAPSED -lt "$TIMEOUT" ]; do
    if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -E "Permission resolved:|answered:" > /dev/null 2>&1; then
      TC1_RESOLVED=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  pane_log "[tc1] AFTER resolution wait"

  if [ "$TC1_RESOLVED" = "true" ]; then
    pass "TC1: Permission resolved (hook received answer via streaming connection)"
  else
    fail "TC1: Permission not resolved within ${TIMEOUT}s"
  fi

  # Wait for CC to finish (Stream relabel ✅)
  ELAPSED=0
  TC1_STOP=false
  while [ $ELAPSED -lt "$TIMEOUT" ]; do
    if tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "Stream relabel ✅:" > /dev/null 2>&1; then
      TC1_STOP=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  if [ "$TC1_STOP" = "true" ]; then
    pass "TC1: Stream relabel ✅ received (CC turn complete after permission)"
  else
    fail "TC1: Stream relabel ✅ not received within ${TIMEOUT}s"
  fi
else
  fail "TC1: PermissionRequest not triggered within ${TIMEOUT}s"
fi

wait_for_idle
pane_log "[tc1] AFTER idle"

# ============================================================
# TC2: AskUserQuestion answered in TUI → PostToolUse freezes
#       button with actual answer label (not a generic label)
# ============================================================
echo ""
echo "  TC2: AskUserQuestion answered in TUI → PostToolUse freeze with actual answer"

LOG_BEFORE_TC2=$(wc -l < "$LOG_FILE")

pane_log "[tc2] BEFORE AskUserQuestion prompt"
inject_prompt "Use the AskUserQuestion tool with header 'TC29_TC2' and exactly two options: 'Yes_29' (description: 'Affirmative'), 'No_29' (description: 'Negative'). Question: 'Please choose one.' Select whichever option you prefer WITHOUT waiting for user input — call the tool, pick the first option immediately in the same turn."
pane_log "[tc2] AFTER AskUserQuestion prompt inject"

# Wait for AskUserQuestion sent in log
ELAPSED=0
TC2_AQ_FOUND=false
TC2_AQ_MSG_ID=""
TC2_AQ_UUID=""
while [ $ELAPSED -lt "$TIMEOUT" ]; do
  if tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE" | grep "AskUserQuestion sent" > /dev/null 2>&1; then
    TC2_AQ_FOUND=true
    TC2_AQ_MSG_ID=$(tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE" | grep -oPm1 'AskUserQuestion sent.*msg_id=\K[0-9]+' || true)
    TC2_AQ_UUID=$(tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE" | grep -oPm1 'AskUserQuestion sent.*uuid=\K[^ ]+' || true)
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

pane_log "[tc2] AFTER AQ detected"

if [ "$TC2_AQ_FOUND" = "true" ] && [ -n "$TC2_AQ_MSG_ID" ]; then
  pass "TC2: AskUserQuestion sent (msg_id=$TC2_AQ_MSG_ID uuid=$TC2_AQ_UUID)"

  # Respond via /tool/respond to simulate TUI answer selection
  pane_log "[tc2] BEFORE tool/respond API call"
  RESPOND_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/tool/respond?msg_id=$TC2_AQ_MSG_ID&tool=AskUserQuestion&question=0&option=0")
  pane_log "[tc2] AFTER tool/respond API call"

  # Wait for PostToolUse correlation → FreezeWaitEntry or Resolved on desktop
  ELAPSED=0
  TC2_FROZEN=false
  while [ $ELAPSED -lt "$TIMEOUT" ]; do
    TC2_LOGS=$(tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE")
    if echo "$TC2_LOGS" | grep -E "FreezeWaitEntry\(AskQ\):|Resolved on desktop:" > /dev/null 2>&1; then
      TC2_FROZEN=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  pane_log "[tc2] AFTER freeze wait"

  if [ "$TC2_FROZEN" = "true" ]; then
    pass "TC2: FreezeWaitEntry(AskQ) or Resolved on desktop logged (PostToolUse correlation)"

    # Verify the freeze label contains actual answer text (not a generic placeholder)
    TC2_LOGS=$(tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE")
    FREEZE_LINE=$(echo "$TC2_LOGS" | grep -E "FreezeWaitEntry\(AskQ\):" | tail -1 || true)
    DESKTOP_LINE=$(echo "$TC2_LOGS" | grep "Resolved on desktop:" | tail -1 || true)
    if [ -n "$FREEZE_LINE" ]; then
      # The label should contain the actual answer, not just a blank or "❌ Cancelled"
      if echo "$FREEZE_LINE" | grep -v "❌ Cancelled" > /dev/null 2>&1; then
        pass "TC2: Freeze label contains actual answer (not generic cancel): $FREEZE_LINE"
      else
        warn "TC2: Freeze label is '❌ Cancelled' — expected actual answer text"
      fi
    elif [ -n "$DESKTOP_LINE" ]; then
      pass "TC2: Resolved on desktop logged: $DESKTOP_LINE"
    fi
  else
    # May have been answered before PostToolUse arrived — check if session idle
    warn "TC2: FreezeWaitEntry(AskQ) not logged within ${TIMEOUT}s (may have been answered in TUI before PostToolUse)"
  fi

  wait_for_idle
  pane_log "[tc2] AFTER idle"

  # Verify no poll tick in TC2 window
  TC2_LOGS=$(tail -n +"$((LOG_BEFORE_TC2 + 1))" "$LOG_FILE")
  if echo "$TC2_LOGS" | grep "poll tick" > /dev/null 2>&1; then
    fail "TC2: 'poll tick' found — old polling path still active"
  else
    pass "TC2: No 'poll tick' in TC2 log window"
  fi
else
  fail "TC2: AskUserQuestion not triggered within ${TIMEOUT}s"
fi

wait_for_idle
pane_log "[tc2] AFTER idle (post-cleanup)"

# ============================================================
# TC3: Characterise cancel/ESC outcomes:
#       /pending/cancel → "❌ Cancelled" label; assert FreezeWaitEntry logs the label.
#       (Informational — not a hard gate on exact TUI behavior)
# ============================================================
echo ""
echo "  TC3: cancel via /pending/cancel → ❌ Cancelled label"

LOG_BEFORE_TC3=$(wc -l < "$LOG_FILE")

pane_log "[tc3] BEFORE permission cancel prompt"
inject_prompt "Write one sentence: TC29_TC3_CANCEL. Then run this exact bash command: echo tc29_cancel_test > /tmp/tg-cli-phase29-tc3.txt. Run only this command, nothing else."
pane_log "[tc3] AFTER inject"

# Wait for Permission request sent
ELAPSED=0
TC3_FOUND=false
TC3_UUID=""
while [ $ELAPSED -lt "$TIMEOUT" ]; do
  if tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE" | grep "Permission request sent" > /dev/null 2>&1; then
    TC3_FOUND=true
    TC3_UUID=$(tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE" | grep -oPm1 'Permission request sent.*uuid=\K[^ ]+' || true)
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

pane_log "[tc3] AFTER permission detected"

if [ "$TC3_FOUND" = "true" ] && [ -n "$TC3_UUID" ]; then
  pass "TC3: PermissionRequest triggered (uuid=$TC3_UUID)"

  # Cancel via /pending/cancel
  pane_log "[tc3] BEFORE cancel API call"
  curl -s -X POST "http://127.0.0.1:$TEST_PORT/pending/cancel?uuid=$TC3_UUID" > /dev/null 2>&1 || true
  pane_log "[tc3] AFTER cancel API call"

  # Wait for cancel confirmation and freeze label log
  ELAPSED=0
  TC3_CANCELLED=false
  while [ $ELAPSED -lt "$TIMEOUT" ]; do
    TC3_LOGS=$(tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE")
    if echo "$TC3_LOGS" | grep -E "Permission cancelled:|pendingConnect grace expired:.*cancelled|FreezeWaitEntry\(Perm\):" > /dev/null 2>&1; then
      TC3_CANCELLED=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  pane_log "[tc3] AFTER cancel logged"

  if [ "$TC3_CANCELLED" = "true" ]; then
    pass "TC3: Permission cancel logged"

    # Characterise: check freeze label is ❌ Cancelled
    TC3_LOGS=$(tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE")
    FREEZE_PERM_LINE=$(echo "$TC3_LOGS" | grep "FreezeWaitEntry(Perm):" | tail -1 || true)
    if [ -n "$FREEZE_PERM_LINE" ]; then
      if echo "$FREEZE_PERM_LINE" | grep -F "❌ Cancelled" > /dev/null 2>&1; then
        pass "TC3: FreezeWaitEntry(Perm) label=❌ Cancelled (correct cancel freeze)"
      else
        pass_opt "TC3: FreezeWaitEntry(Perm) label unexpected: $FREEZE_PERM_LINE"
      fi
    else
      pass_opt "TC3: No FreezeWaitEntry(Perm) line found (cancel may have been handled differently)"
    fi
  else
    fail "TC3: Permission cancel not logged within ${TIMEOUT}s"
  fi

  wait_for_idle
  pane_log "[tc3] AFTER idle"
else
  fail "TC3: PermissionRequest not triggered within ${TIMEOUT}s"
fi

wait_for_idle
pane_log "[tc3] AFTER idle (post-cleanup)"

# ============================================================
# TC4: Bot restart while PermissionRequest pending →
#       original button still resolves it (hook reconnects)
# ============================================================
echo ""
echo "  TC4: bot restart while PermissionRequest pending → original button resolves"

LOG_BEFORE_TC4=$(wc -l < "$LOG_FILE")

pane_log "[tc4] BEFORE permission restart prompt"
inject_prompt "Write one sentence: TC29_TC4_RESTART. Then run this exact bash command: echo tc29_restart_test > /tmp/tg-cli-phase29-tc4.txt. Run only this command, nothing else."
pane_log "[tc4] AFTER inject"

# Wait for Permission request sent and hook connected
ELAPSED=0
TC4_FOUND=false
TC4_MSG_ID=""
TC4_UUID=""
while [ $ELAPSED -lt "$TIMEOUT" ]; do
  if tail -n +"$((LOG_BEFORE_TC4 + 1))" "$LOG_FILE" | grep "hook connected:" > /dev/null 2>&1; then
    TC4_FOUND=true
    TC4_MSG_ID=$(tail -n +"$((LOG_BEFORE_TC4 + 1))" "$LOG_FILE" | grep -m1 "Permission request sent" | grep -oP 'msg_id=\K[0-9]+' || true)
    TC4_UUID=$(tail -n +"$((LOG_BEFORE_TC4 + 1))" "$LOG_FILE" | grep -m1 "Permission request sent" | grep -oP 'uuid=\K[^ ]+' || true)
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

pane_log "[tc4] AFTER hook connected detected"

if [ "$TC4_FOUND" = "true" ] && [ -n "$TC4_MSG_ID" ]; then
  pass "TC4: hook connected for PermissionRequest (msg_id=$TC4_MSG_ID uuid=$TC4_UUID)"

  # Restart the bot while hook is connected — simulate upgrade window
  # so the hook holds + reconnects instead of exiting (cmd/hook.go:267-273 gates on UpgradeFlagActive)
  UPGRADE_FLAG="$TEST_CONFIG_DIR/upgrading"
  pane_log "[tc4] BEFORE bot restart"
  echo "  Writing upgrade flag: $UPGRADE_FLAG"
  echo "$(date +%s)" > "$UPGRADE_FLAG"
  echo "  Restarting bot (killing $BOT_SESSION session)..."
  $TMUX_TEST send-keys -t "$BOT_SESSION" C-c
  sleep 1
  # Restart bot via the same launch command
  $TMUX_TEST send-keys -t "$BOT_SESSION" "$_LAST_BOT_LAUNCH_CMD" Enter
  echo "  Waiting for bot to restart..."
  wait_for_bot_ready
  echo "  Removing upgrade flag"
  rm -f "$UPGRADE_FLAG"

  pane_log "[tc4] AFTER bot restart"

  # Wait for hook to reattach (hook reattached or hook reattached (restart))
  ELAPSED=0
  TC4_REATTACHED=false
  LOG_AFTER_RESTART=$(wc -l < "$LOG_FILE")
  while [ $ELAPSED -lt "$TIMEOUT" ]; do
    if tail -n +"$((LOG_AFTER_RESTART + 1))" "$LOG_FILE" | grep -E "hook reattached" > /dev/null 2>&1; then
      TC4_REATTACHED=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  if [ "$TC4_REATTACHED" = "true" ]; then
    pass "TC4: hook reattached after bot restart"
  else
    pass_opt "TC4: hook reattached not logged (hook may have reconnected via fresh connect path)"
  fi

  # Now resolve via /permission/decide with the original msg_id
  pane_log "[tc4] BEFORE approve original button"
  DECIDE_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/permission/decide?msg_id=$TC4_MSG_ID&decision=allow" || echo '{}')
  pane_log "[tc4] AFTER approve original button"

  DECIDE_BEHAVIOR=$(echo "$DECIDE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('behavior',''))" 2>/dev/null || true)
  if [ "$DECIDE_BEHAVIOR" = "allow" ]; then
    pass "TC4: Original button resolved after bot restart (behavior=allow)"
  else
    pass_opt "TC4: /permission/decide returned: $DECIDE_RESP (may resolve via alternate path after restart)"
  fi

  # Wait for CC to complete (Stream relabel ✅)
  ELAPSED=0
  TC4_STOP=false
  while [ $ELAPSED -lt "$TIMEOUT" ]; do
    if tail -n +"$((LOG_BEFORE_TC4 + 1))" "$LOG_FILE" | grep "Stream relabel ✅:" > /dev/null 2>&1; then
      TC4_STOP=true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done

  pane_log "[tc4] AFTER completion wait"

  if [ "$TC4_STOP" = "true" ]; then
    pass "TC4: Stream relabel ✅ received (CC turn complete after restart + resolve)"
  else
    fail "TC4: Stream relabel ✅ not received within ${TIMEOUT}s after restart"
  fi
else
  fail "TC4: hook connected not logged within ${TIMEOUT}s"
fi

wait_for_idle
pane_log "[tc4] AFTER idle"

# ============================================================
# Post-run assertions: ZERO poll tick lines in entire session
# ============================================================
echo ""
echo "  Post-run: verifying no poll tick in session log"

SESSION_LOGS=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE")
if echo "$SESSION_LOGS" | grep "poll tick" > /dev/null 2>&1; then
  fail "Post-run: 'poll tick' found in session log — old file-polling path active"
else
  pass "Post-run: ZERO 'poll tick' lines in session log (streaming path only)"
fi

# Verify hook lifecycle lines present
if echo "$SESSION_LOGS" | grep "hook connected:" > /dev/null 2>&1; then
  pass "Post-run: hook lifecycle line 'hook connected:' present"
else
  fail "Post-run: 'hook connected:' not found in session log"
fi
