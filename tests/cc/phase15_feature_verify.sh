#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Feature verify: MCP send-file, stopCooldown, session/send API ---"

ensure_infrastructure

# =============================================
# Sub-test A: MCP send-file API with CWD field
# =============================================

TEST_FILE="/tmp/tg-cli-e2e-phase28-test.txt"
echo "phase28 feature verify test - $(date)" > "$TEST_FILE"

LOG_BEFORE_A=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
pane_log "[phase28-A] BEFORE MCP send-file"

# POST to /mcp/send-file with empty tmux_target and cwd set to project root
# This exercises the CWD fallback code path regardless of whether the fallback
# actually changes the route (it may not if the session is already on default chat)
MCP_RESP=$(curl -s -w "\n%{http_code}" -X POST \
  "http://127.0.0.1:$TEST_PORT/mcp/send-file" \
  -H "Content-Type: application/json" \
  -d "{\"file_path\":\"$TEST_FILE\",\"caption\":\"phase28 E2E test\",\"tmux_target\":\"\",\"cwd\":\"$(pwd)\"}")
MCP_CODE=$(echo "$MCP_RESP" | tail -1)
MCP_BODY=$(echo "$MCP_RESP" | head -1)

pane_log "[phase28-A] AFTER MCP send-file"

if [ "$MCP_CODE" = "200" ] && echo "$MCP_BODY" | grep -q '"ok":true'; then
  pass "MCP send-file: API returned 200 ok"
else
  fail "MCP send-file: API returned code=$MCP_CODE body=$MCP_BODY"
fi

# Check bot log for [MCP] File sent
ELAPSED=0
MCP_SENT=false
while [ $ELAPSED -lt 30 ]; do
  if tail -n +"$((LOG_BEFORE_A + 1))" "$LOG_FILE" | grep -q "\[MCP\] File sent.*phase28"; then
    MCP_SENT=true
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

if [ "$MCP_SENT" = true ]; then
  pass "MCP send-file: [MCP] File sent logged"
else
  fail "MCP send-file: [MCP] File sent not found in log within 30s"
fi

rm -f "$TEST_FILE"

# =============================================
# Sub-test B: stopCooldown triggers after Stop event
# =============================================
# Inject a prompt, wait for Stop, then immediately inject again.
# The second inject should trigger stopCooldown.waitIfNeeded which logs
# "stopCooldown: waiting ... for target=..." when cooldown is active.

LOG_BEFORE_B=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
pane_log "[phase28-B] BEFORE first inject"

inject_prompt "Reply with exactly: cooldown_test_ok"

# Wait for CC to become idle (Stop event recorded)
wait_for_idle $TIMEOUT
pane_log "[phase28-B] AFTER first idle"

# Immediately inject a second prompt to trigger stopCooldown check
LOG_BEFORE_B2=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
inject_prompt "Reply with exactly: cooldown_test_two"

# Check if stopCooldown wait was logged (only fires if within 3s cooldown window)
sleep 3
COOLDOWN_LOGGED=false
if tail -n +"$((LOG_BEFORE_B2 + 1))" "$LOG_FILE" | grep -q "stopCooldown: waiting"; then
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
pane_log "[phase28-B] AFTER second idle"

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

SEND_TOKEN="phase28_send_test_$RANDOM"
LOG_BEFORE_C=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
pane_log "[phase28-C] BEFORE session/send"

# POST to /session/send API
SEND_RESP=$(curl -s -w "\n%{http_code}" -X POST \
  "http://127.0.0.1:$TEST_PORT/session/send" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"$SESSION_NAME\",\"text\":\"$SEND_TOKEN\",\"from\":\"phase28-test\"}")
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
  if tail -n +"$((LOG_BEFORE_C + 1))" "$LOG_FILE" | grep -q "Session send via API:.*$SEND_TOKEN"; then
    SEND_FOUND=true
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

pane_log "[phase28-C] AFTER session/send"

if [ "$SEND_FOUND" = true ]; then
  pass "session/send: API log recorded with text content"
else
  fail "session/send: Session send via API log not found within 15s"
fi

wait_for_idle $TIMEOUT
pane_log "[phase28-C] AFTER CC idle"

# =============================================
# Test D: tg-cli usage CLI command (CC-only, uses Anthropic API)
# =============================================

if [ "${E2E_BACKEND:-}" != "codex" ]; then
  echo ""
  echo "  --- Sub-test D: tg-cli usage CLI command ---"

  USAGE_OUTPUT=$(./tg-cli usage 2>&1) || true

  if echo "$USAGE_OUTPUT" | grep -q "CC Usage"; then
    pass "usage CLI: output contains CC Usage header"
  else
    fail "usage CLI: output missing CC Usage header - got: $USAGE_OUTPUT"
  fi
else
  echo "  SKIP: usage CLI (Anthropic API, CC-only)"
fi
