#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Merge mode test ---"

ensure_infrastructure

# Bind route for merge test
AGENT_NAME="e2e-cli"
SESSION_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    if s.get("target") == sys.argv[1]:
        print(s.get("id", ""))
        sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
if [ -z "$SESSION_ID" ]; then
  # Fallback: try matching by target substring (Codex targets may have @socket suffix)
  SESSION_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
pane = sys.argv[1]
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t.startswith(pane):
        print(s.get("id", ""))
        sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
fi
if [ -z "$SESSION_ID" ]; then
  fail "Could not extract session ID for merge test"
  exit 1
fi

BIND_PAYLOAD=$(jq -n --arg n "$AGENT_NAME" --argjson c "$DEFAULT_CHAT_ID" '{name: $n, chat_id: $c, topic_id: 0}')
curl -s -X POST -H "Content-Type: application/json" -d "$BIND_PAYLOAD" "http://127.0.0.1:$TEST_PORT/route/bind" > /dev/null
pass "Route bound for merge test"

LOG_BEFORE_MERGE=$(wc -l < "$LOG_FILE")

# Start merge mode
ENCODED_PANE=$(printf '%s' "$E2E_PANE" | jq -sRr @uri)
START_CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$TEST_PORT/merge/start?target=$ENCODED_PANE")
if [ "$START_CODE" = "200" ]; then
  pass "/merge/start returned 200"
else
  fail "/merge/start returned $START_CODE"
fi

# Add instruction
ADD1_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" \
  -d '{"text":"You will receive multiple lines below. Repeat each line exactly as-is."}' \
  "http://127.0.0.1:$TEST_PORT/merge/add")
if [ "$ADD1_CODE" = "200" ]; then
  pass "/merge/add instruction returned 200"
else
  fail "/merge/add instruction returned $ADD1_CODE"
fi

# Add unique tokens
ADD2_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" \
  -d '{"text":"MERGE_TOKEN_ALPHA_7x9k"}' \
  "http://127.0.0.1:$TEST_PORT/merge/add")
if [ "$ADD2_CODE" = "200" ]; then
  pass "/merge/add token ALPHA returned 200"
else
  fail "/merge/add token ALPHA returned $ADD2_CODE"
fi

ADD3_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" \
  -d '{"text":"MERGE_TOKEN_BETA_3m2p"}' \
  "http://127.0.0.1:$TEST_PORT/merge/add")
if [ "$ADD3_CODE" = "200" ]; then
  pass "/merge/add token BETA returned 200"
else
  fail "/merge/add token BETA returned $ADD3_CODE"
fi

# Submit merged content
SUBMIT_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "http://127.0.0.1:$TEST_PORT/merge/submit")
if [ "$SUBMIT_CODE" = "200" ]; then
  pass "/merge/submit returned 200"
else
  fail "/merge/submit returned $SUBMIT_CODE"
fi

# Verify bot log: "Merge submitted via API" with items=3
ELAPSED=0
MERGE_LOG_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_MERGE + 1))" "$LOG_FILE" | grep "Merge submitted via API.*items=3" > /dev/null 2>&1; then
    MERGE_LOG_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for merge submitted log... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$MERGE_LOG_FOUND" = true ]; then
  pass "Merge submitted log found with items=3"
else
  fail "Merge submitted log not found within ${TIMEOUT}s"
fi

# Wait for CC to finish processing
wait_for_idle
pane_log "[merge_mode] AFTER CC idle"

# Verify Stop notification body contains both unique tokens
ELAPSED=0
STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_MERGE + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
    STOP_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for Stop notification... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$STOP_FOUND" = true ]; then
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_MERGE + 1))" "$LOG_FILE")
  if echo "$NEW_LOGS" | grep "MERGE_TOKEN_ALPHA_7x9k" > /dev/null 2>&1; then
    pass "CC output contains MERGE_TOKEN_ALPHA_7x9k"
  else
    fail "CC output missing MERGE_TOKEN_ALPHA_7x9k"
  fi
  if echo "$NEW_LOGS" | grep "MERGE_TOKEN_BETA_3m2p" > /dev/null 2>&1; then
    pass "CC output contains MERGE_TOKEN_BETA_3m2p"
  else
    fail "CC output missing MERGE_TOKEN_BETA_3m2p"
  fi
else
  fail "Stop notification not received after merge within ${TIMEOUT}s"
fi

# Test edge cases: submit without active merge
EMPTY_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "http://127.0.0.1:$TEST_PORT/merge/submit")
if [ "$EMPTY_CODE" = "404" ]; then
  pass "Submit without active merge returned 404"
else
  fail "Submit without active merge returned $EMPTY_CODE (expected 404)"
fi

# Cleanup: unbind route
UNBIND_PAYLOAD=$(jq -n --arg n "$AGENT_NAME" '{name: $n}')
curl -s -X POST -H "Content-Type: application/json" -d "$UNBIND_PAYLOAD" "http://127.0.0.1:$TEST_PORT/route/unbind" > /dev/null
pass "Route unbound after merge test"
