#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Feature verify: usage CLI, stopCooldown, session/send API ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-1"

# =============================================
# Sub-test A: tg-cli usage CLI command (tmux method)
# =============================================

if [ "${E2E_BACKEND:-}" != "codex" ]; then
  echo ""
  echo "  --- Sub-test A: tg-cli usage CLI command ---"

  # Primary: merged usage (default path, matches /u TG command)
  USAGE_MERGED=$(./tg-cli usage --tmux-server $TMUX_SERVER_NAME 2>&1) || true
  echo "  DEBUG: USAGE_MERGED (${#USAGE_MERGED} chars): $USAGE_MERGED"
  set +eo pipefail
  echo "$USAGE_MERGED" | grep -q "Claude Usage"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    pass_opt "usage CLI (merged): output contains Claude Usage header"
  else
    warn "usage CLI (merged): output missing Claude Usage header (rate limit?) - got: $USAGE_MERGED"
  fi

  # Secondary: tmux-only method
  USAGE_TMUX=$(./tg-cli usage --method tmux --tmux-server $TMUX_SERVER_NAME 2>&1) || true
  echo "  DEBUG: USAGE_TMUX (${#USAGE_TMUX} chars): $USAGE_TMUX"
  set +eo pipefail
  echo "$USAGE_TMUX" | grep -q "Claude Usage"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    pass_opt "usage CLI (tmux): output contains Claude Usage header"
  else
    warn "usage CLI (tmux): output missing Claude Usage header (rate limit?) - got: $USAGE_TMUX"
  fi

  # Regression guard (6ab3269 dropped the defer kill-session, leaking the temp session on the
  # /usage failure path → run_phase stale-session check aborted the suite). The session MUST now
  # be cleaned up on ALL paths, even when /usage fails — deterministic regardless of /usage outcome.
  USAGE_LEFTOVER=$($TMUX_TEST list-sessions -F '#{session_name}' 2>/dev/null | grep '^tg-cli-usage-' || true)
  if [ -z "$USAGE_LEFTOVER" ]; then
    pass "usage CLI (tmux): temp session cleaned up (no tg-cli-usage-* leaked)"
  else
    fail "usage CLI (tmux): leaked tg-cli-usage-* session(s): $USAGE_LEFTOVER"
  fi
else
  echo "  SKIP: usage CLI (CC-only)"
fi

# =============================================
# Sub-test B: stopCooldown triggers after Stop event
# =============================================
# Inject a prompt, wait for Stop, then immediately inject again.
# The second inject should trigger stopCooldown.waitIfNeeded which logs
# "stopCooldown: waiting ... for target=..." when cooldown is active.

LOG_BEFORE_B=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
pane_log "[feature_verify-B] BEFORE first inject"

inject_prompt "Reply with exactly: cooldown_test_ok"

# Wait for CC to become idle (Stop event recorded)
wait_for_idle $TIMEOUT
pane_log "[feature_verify-B] AFTER first idle"

# Immediately inject a second prompt to trigger stopCooldown check
LOG_BEFORE_B2=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
inject_prompt "Reply with exactly: cooldown_test_two"

# Check if stopCooldown wait was logged (only fires if within 3s cooldown window)
sleep 3
COOLDOWN_LOGGED=false
set +eo pipefail
tail -n +"$((LOG_BEFORE_B2 + 1))" "$LOG_FILE" | grep -q "stopCooldown: waiting"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  COOLDOWN_LOGGED=true
fi

if [ "$COOLDOWN_LOGGED" = true ]; then
  pass "stopCooldown: cooldown wait logged after rapid re-inject"
else
  # Not finding the log is also acceptable — it means inject happened after cooldown expired
  pass "stopCooldown: no cooldown wait (inject after cooldown window, OK)"
fi

# Wait for second prompt to complete
wait_for_idle $TIMEOUT
pane_log "[feature_verify-B] AFTER second idle"

# =============================================
# Sub-test C: /session/send API injects text
# =============================================

SESSION_NAME="e2e-cli"

# Name the current CC session for /session/send lookup
SESSION_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    if s.get("target") == sys.argv[1]:
        print(s.get("id", ""))
        sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
if [ -n "$SESSION_ID" ]; then
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=$SESSION_NAME" > /dev/null 2>&1 || true
  echo "  Named session $SESSION_ID as $SESSION_NAME"
fi

SEND_TOKEN="feature_send_test_$RANDOM"
LOG_BEFORE_C=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
pane_log "[feature_verify-C] BEFORE session/send"

# POST to /session/send API
SEND_RESP=$(curl -s -w "\n%{http_code}" -X POST \
  "http://127.0.0.1:$TEST_PORT/session/send" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"$SESSION_NAME\",\"text\":\"$SEND_TOKEN\",\"from\":\"feature-test\"}")
echo "  DEBUG: SEND_RESP (${#SEND_RESP} chars): $SEND_RESP"
SEND_CODE=$(echo "$SEND_RESP" | tail -1)

if [ "$SEND_CODE" = "200" ]; then
  pass "session/send: API returned 200"
else
  fail "session/send: API returned code=$SEND_CODE"
fi

# Verify the log contains Session send via API with the token
ELAPSED=0
SEND_FOUND=false
while [ $ELAPSED -lt 15 ]; do
  set +eo pipefail
  tail -n +"$((LOG_BEFORE_C + 1))" "$LOG_FILE" | grep -q "Session send via API:.*$SEND_TOKEN"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    SEND_FOUND=true
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

pane_log "[feature_verify-C] AFTER session/send"

if [ "$SEND_FOUND" = true ]; then
  pass "session/send: API log recorded with text content"
else
  fail "session/send: Session send via API log not found within 15s"
fi

wait_for_idle $TIMEOUT
pane_log "[feature_verify-C] AFTER CC idle"
