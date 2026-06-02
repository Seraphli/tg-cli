#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Group routing test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-16"

# Use a fixed agent name for testing
AGENT_NAME="e2e-cli"

# Extract session ID from session list API (works for both CC and Codex)
SESSION_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
pane = sys.argv[1]
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane + "@"):
        print(s.get("id", ""))
        sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
if [ -n "$SESSION_ID" ]; then
  pass "Extracted session ID: $SESSION_ID"
else
  fail "Could not extract session ID from session list API"
  exit 1
fi

# Name the session so resolveChat can match it to the name route
curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=$AGENT_NAME" > /dev/null 2>&1 || true
echo "  Named session $SESSION_ID as $AGENT_NAME"

# Call POST /route/bind
echo "  Binding route: name=$AGENT_NAME → chat=$DEFAULT_CHAT_ID"
BIND_PAYLOAD=$(jq -n --arg n "$AGENT_NAME" --argjson c "$DEFAULT_CHAT_ID" '{name: $n, chat_id: $c, topic_id: 0}')
BIND_RESP=$(curl -s -w "\n%{http_code}" -X POST \
  -H "Content-Type: application/json" \
  -d "$BIND_PAYLOAD" \
  "http://127.0.0.1:$TEST_PORT/route/bind")
echo "  DEBUG: BIND_RESP (${#BIND_RESP} chars): $BIND_RESP"
BIND_CODE=$(echo "$BIND_RESP" | tail -1)
if [ "$BIND_CODE" = "200" ]; then
  pass "/route/bind returned 200"
else
  fail "/route/bind returned $BIND_CODE"
fi

# Call GET /route/list and verify the binding
LIST_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/route/list")
echo "  DEBUG: LIST_RESP (${#LIST_RESP} chars): $LIST_RESP"
if echo "$LIST_RESP" | jq -e ".name_routes[\"$AGENT_NAME\"].chatId == ($DEFAULT_CHAT_ID | tonumber)" > /dev/null 2>&1; then
  pass "/route/list contains bound route"
else
  fail "/route/list missing bound route"
fi

# Inject new prompt to trigger route resolution
LOG_BEFORE_ROUTE=$(wc -l < "$LOG_FILE")
pane_log "[group_routing] BEFORE 'say test routing' prompt"
inject_prompt "Reply with exactly: route_test_ok"
pane_log "[group_routing] AFTER routing prompt"

# Wait for Stop notification
ELAPSED=0
ROUTE_STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_ROUTE" ]; then
    if tail -n +"$((LOG_BEFORE_ROUTE + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
      ROUTE_STOP_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

pane_log "[group_routing] AFTER Stop detected"

if [ "$ROUTE_STOP_FOUND" = true ]; then
  pass "Stop notification received after routing prompt"
else
  fail "Stop notification not received within ${TIMEOUT}s"
fi

# Verify "Route resolved" log line exists
if tail -n +"$((LOG_BEFORE_ROUTE + 1))" "$LOG_FILE" | grep "Route resolved: name=" > /dev/null 2>&1; then
  ROUTE_LOG=$(tail -n +"$((LOG_BEFORE_ROUTE + 1))" "$LOG_FILE" | grep -m1 "Route resolved: name=" || true)
  pass "Route resolved log found: $ROUTE_LOG"
else
  fail "Route resolved log not found"
fi

# Call POST /route/unbind
echo "  Unbinding route: name=$AGENT_NAME"
UNBIND_PAYLOAD=$(jq -n --arg n "$AGENT_NAME" '{name: $n}')
UNBIND_RESP=$(curl -s -w "\n%{http_code}" -X POST \
  -H "Content-Type: application/json" \
  -d "$UNBIND_PAYLOAD" \
  "http://127.0.0.1:$TEST_PORT/route/unbind")
echo "  DEBUG: UNBIND_RESP (${#UNBIND_RESP} chars): $UNBIND_RESP"
UNBIND_CODE=$(echo "$UNBIND_RESP" | tail -1)
if [ "$UNBIND_CODE" = "200" ]; then
  pass "/route/unbind returned 200"
else
  fail "/route/unbind returned $UNBIND_CODE"
fi

# Verify the unbound route is gone
LIST_RESP_AFTER=$(curl -s "http://127.0.0.1:$TEST_PORT/route/list")
echo "  DEBUG: LIST_RESP_AFTER (${#LIST_RESP_AFTER} chars): $LIST_RESP_AFTER"
set +eo pipefail
echo "$LIST_RESP_AFTER" | grep -q "e2e-cli"
_ub=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ub[1]}" -ne 0 ]; then
  pass "/route/list no longer contains e2e-cli after unbind"
else
  fail "/route/list still contains e2e-cli after unbind"
fi

# Inject another prompt (should fall back to default chat)
LOG_BEFORE_DEFAULT=$(wc -l < "$LOG_FILE")
# Count existing "Route resolved" lines before this test
ROUTE_COUNT_BEFORE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -c "Route resolved: name=" || echo 0)

pane_log "[group_routing] BEFORE 'say test default' prompt"
inject_prompt "say test default"
pane_log "[group_routing] AFTER default prompt"

# Wait for Stop notification
ELAPSED=0
DEFAULT_STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if [ "$(wc -l < "$LOG_FILE")" -gt "$LOG_BEFORE_DEFAULT" ]; then
    if tail -n +"$((LOG_BEFORE_DEFAULT + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
      DEFAULT_STOP_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
done

pane_log "[group_routing] AFTER default Stop detected"

if [ "$DEFAULT_STOP_FOUND" = true ]; then
  pass "Stop notification received after default routing prompt"
else
  fail "Stop notification not received within ${TIMEOUT}s"
fi

# Verify NO NEW "Route resolved" line appeared (should fall back to default)
ROUTE_COUNT_AFTER=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -c "Route resolved: name=" || echo 0)
if [ "$ROUTE_COUNT_AFTER" = "$ROUTE_COUNT_BEFORE" ]; then
  pass "No new route resolution after unbind (fell back to default)"
else
  fail "Unexpected route resolution after unbind"
fi
