#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Phase 8: Group routing test ---"

ensure_infrastructure

# Extract tmux_target from SessionStart log
TMUX_TARGET=""
SESSION_START_LINE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -m1 "Notification sent.*SessionStart" || true)
if [ -n "$SESSION_START_LINE" ]; then
  # Extract tmux target like "session:window.pane" from the log body
  TMUX_TARGET=$(echo "$SESSION_START_LINE" | grep -oP 'tmux=\K[^[:space:]]+' || true)
fi

if [ -n "$TMUX_TARGET" ]; then
  pass "Extracted tmux_target from SessionStart log: $TMUX_TARGET"
else
  fail "Could not extract tmux_target from SessionStart log"
  exit 1
fi

# Call POST /route/bind
echo "  Binding route: tmux=$TMUX_TARGET → chat=$DEFAULT_CHAT_ID"
BIND_PAYLOAD=$(jq -n --arg t "$TMUX_TARGET" --argjson c "$DEFAULT_CHAT_ID" '{tmux_target: $t, chat_id: $c}')
BIND_RESP=$(curl -s -w "\n%{http_code}" -X POST \
  -H "Content-Type: application/json" \
  -d "$BIND_PAYLOAD" \
  "http://127.0.0.1:$TEST_PORT/route/bind")
BIND_CODE=$(echo "$BIND_RESP" | tail -1)
if [ "$BIND_CODE" = "200" ]; then
  pass "/route/bind returned 200"
else
  fail "/route/bind returned $BIND_CODE"
fi

# Call GET /route/list and verify the binding
LIST_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/route/list")
if echo "$LIST_RESP" | jq -e ".routes[\"$TMUX_TARGET\"] == ($DEFAULT_CHAT_ID | tonumber)" > /dev/null 2>&1; then
  pass "/route/list contains bound route"
else
  fail "/route/list missing bound route"
fi

# Inject new prompt to trigger route resolution
LOG_BEFORE_ROUTE=$(wc -l < "$LOG_FILE")
pane_log "[Phase 8] BEFORE 'say test routing' prompt"
inject_prompt "say test routing"
pane_log "[Phase 8] AFTER routing prompt"

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

pane_log "[Phase 8] AFTER Stop detected"

if [ "$ROUTE_STOP_FOUND" = true ]; then
  pass "Stop notification received after routing prompt"
else
  fail "Stop notification not received within ${TIMEOUT}s"
fi

# Verify "Route resolved" log line exists
if tail -n +"$((LOG_BEFORE_ROUTE + 1))" "$LOG_FILE" | grep "Route resolved: tmux=" > /dev/null 2>&1; then
  ROUTE_LOG=$(tail -n +"$((LOG_BEFORE_ROUTE + 1))" "$LOG_FILE" | grep -m1 "Route resolved: tmux=" || true)
  pass "Route resolved log found: $ROUTE_LOG"
else
  fail "Route resolved log not found"
fi

# Call POST /route/unbind
echo "  Unbinding route: tmux=$TMUX_TARGET"
UNBIND_PAYLOAD=$(jq -n --arg t "$TMUX_TARGET" '{tmux_target: $t}')
UNBIND_RESP=$(curl -s -w "\n%{http_code}" -X POST \
  -H "Content-Type: application/json" \
  -d "$UNBIND_PAYLOAD" \
  "http://127.0.0.1:$TEST_PORT/route/unbind")
UNBIND_CODE=$(echo "$UNBIND_RESP" | tail -1)
if [ "$UNBIND_CODE" = "200" ]; then
  pass "/route/unbind returned 200"
else
  fail "/route/unbind returned $UNBIND_CODE"
fi

# Verify routes is now empty
LIST_RESP_AFTER=$(curl -s "http://127.0.0.1:$TEST_PORT/route/list")
ROUTE_COUNT=$(echo "$LIST_RESP_AFTER" | jq '.routes | length')
if [ "$ROUTE_COUNT" = "0" ]; then
  pass "/route/list is empty after unbind"
else
  fail "/route/list still has $ROUTE_COUNT routes after unbind"
fi

# Inject another prompt (should fall back to default chat)
LOG_BEFORE_DEFAULT=$(wc -l < "$LOG_FILE")
# Count existing "Route resolved" lines before this test
ROUTE_COUNT_BEFORE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -c "Route resolved: tmux=" || echo 0)

pane_log "[Phase 8] BEFORE 'say test default' prompt"
inject_prompt "say test default"
pane_log "[Phase 8] AFTER default prompt"

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

pane_log "[Phase 8] AFTER default Stop detected"

if [ "$DEFAULT_STOP_FOUND" = true ]; then
  pass "Stop notification received after default routing prompt"
else
  fail "Stop notification not received within ${TIMEOUT}s"
fi

# Verify NO NEW "Route resolved" line appeared (should fall back to default)
ROUTE_COUNT_AFTER=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -c "Route resolved: tmux=" || echo 0)
if [ "$ROUTE_COUNT_AFTER" = "$ROUTE_COUNT_BEFORE" ]; then
  pass "No new route resolution after unbind (fell back to default)"
else
  fail "Unexpected route resolution after unbind"
fi
