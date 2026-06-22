#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Hook cancel (TUI answer) test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-8" "--allow-dangerously-skip-permissions"

# Record log position
LOG_BEFORE_CANCEL=$(wc -l < "$LOG_FILE")

# Send command that triggers a PermissionRequest (same pattern as permission)
pane_log "[hook_cancel] BEFORE permission prompt"
inject_prompt "First write a brief paragraph explaining what you are about to do, then run this exact bash command: echo hook_cancel_test_ok > /tmp/tg-cli-hook-cancel-test.txt. Run only this one command and nothing else, do not verify or cat the file."
pane_log "[hook_cancel] AFTER sending permission prompt"

# Wait for PermissionRequest notification in bot log (hook blocking on the streaming connection)
ELAPSED=0
PERM_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_CANCEL" ]; then
    if tail -n +"$((LOG_BEFORE_CANCEL + 1))" "$LOG_FILE" | grep "Permission request sent" > /dev/null 2>&1; then
      PERM_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for PermissionRequest... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[hook_cancel] AFTER permission detected"

if [ "$PERM_FOUND" = false ]; then
  fail "PermissionRequest not triggered within ${TIMEOUT}s"
  exit 1
fi
pass "PermissionRequest triggered (hook blocking)"

# Instead of approving via API, approve via TUI: press Enter in Claude pane
# This simulates user answering in TUI while hook is still blocking
pane_log "[hook_cancel] BEFORE TUI Enter (approve in TUI)"
$TMUX_TEST send-keys -t "$E2E_SESSION" Enter
pane_log "[hook_cancel] AFTER TUI Enter"

# Approve-in-TUI is detected by PostToolUse correlation: the Bash tool runs → bot freezes the
# button to "✅ Allowed on desktop" and pushes a cancel so the blocked hook stands down.
# (File-based pending mechanism removed in the file-free streaming refactor.)
ELAPSED=0
RESOLVED_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_CANCEL + 1))" "$LOG_FILE" | grep "Resolved on desktop:.*✅ Allowed on desktop" > /dev/null 2>&1; then
    RESOLVED_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for desktop-resolve log... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$RESOLVED_FOUND" = true ]; then
  pass "TUI approve correlated via PostToolUse (Resolved on desktop: ✅ Allowed on desktop)"
else
  fail "Desktop-resolve log not found within ${TIMEOUT}s"
fi

# TG button frozen to "✅ Allowed on desktop"
if tail -n +"$((LOG_BEFORE_CANCEL + 1))" "$LOG_FILE" | grep "FreezeWaitEntry:.*✅ Allowed on desktop" > /dev/null 2>&1; then
  pass "TG button frozen to '✅ Allowed on desktop'"
else
  fail "FreezeWaitEntry '✅ Allowed on desktop' not found in log"
fi

  # Freeze edit must actually succeed — ZERO 'message text is empty' errors
  if tail -n +"$((LOG_BEFORE_CANCEL + 1))" "$LOG_FILE" | grep -i "message text is empty" > /dev/null 2>&1; then
    fail "Freeze edit failed: 'message text is empty' error in bot log (MsgText was empty)"
  else
    pass "Zero empty-text errors in freeze edits"
  fi

# Blocked hook released by the bot after the TUI answer (session continued in TUI)
if tail -n +"$((LOG_BEFORE_CANCEL + 1))" "$LOG_FILE" | grep -F "[HOOK] cancelled by bot (session continued in TUI)" > /dev/null 2>&1; then
  pass "Blocked hook released after TUI answer ([HOOK] cancelled by bot)"
else
  fail "Blocked hook release log not found ([HOOK] cancelled by bot)"
fi

# CC turn complete (idle polling — no Stop log in new streaming code)
wait_for_idle
pass "CC turn complete after TUI answer (idle confirmed)"
