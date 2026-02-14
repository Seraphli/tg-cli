#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Phase 3: Long message pagination test ---"

ensure_infrastructure

# Record log position before injecting long prompt
LOG_BEFORE_PAGE=$(wc -l < "$LOG_FILE")

# Wait for Claude to settle after Phase 2
echo "Waiting for Claude to settle..."
sleep 3

# Inject a long-output prompt to trigger pagination
LONG_PROMPT="list the numbers from 1 to 100, each on its own line, in the format 'Number NNN: test line for pagination verification'"
pane_log "[Phase 3] BEFORE injecting long prompt"
inject_prompt "$LONG_PROMPT"
echo "Long prompt injected, waiting for Claude to respond and trigger pagination..."
sleep 5
pane_log "[Phase 3] 5s AFTER injecting long prompt"

# Wait for bot log to contain multi-page notification indicator
ELAPSED=0
PAGINATION_FOUND=false
MSG_ID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PAGE" ]; then
    NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
    if echo "$NEW_PAGE_LOGS" | grep -E "pages, msg_id=" > /dev/null 2>&1; then
      PAGINATION_FOUND=true
      MSG_ID=$(echo "$NEW_PAGE_LOGS" | grep -oP 'msg_id=\K[0-9]+' | head -1)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for pagination... ${ELAPSED}s / ${TIMEOUT}s"
done

sleep 5
pane_log "[Phase 3] 5s AFTER pagination detected"

if [ "$PAGINATION_FOUND" = true ]; then
  pass "Long message triggered pagination (real Claude output)"
else
  # Check if Claude sent any notification at all
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PAGE" ]; then
    NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
    if echo "$NEW_PAGE_LOGS" | grep "Notification sent" > /dev/null 2>&1; then
      fail "Long message did not trigger pagination (Claude output too short, no multi-page indicator)"
    else
      fail "No notification sent after long prompt (Claude may not have responded)"
    fi
  else
    fail "No bot activity after long prompt injection"
  fi
fi

# Page turn test (only if pagination was triggered)
if [ "$PAGINATION_FOUND" = true ] && [ -n "$MSG_ID" ]; then
  echo ""
  echo "Testing page turn callback..."
  API_URL="http://127.0.0.1:$TEST_PORT/callback?msg_id=$MSG_ID&page=2"
  echo "  API call: GET $API_URL"
  CB_RESP=$(curl -s -w "\n%{http_code}" "$API_URL")
  CB_CODE=$(echo "$CB_RESP" | tail -1)
  if [ "$CB_CODE" = "200" ]; then
    pass "Page turn simulation via /callback returned 200"
  else
    fail "Page turn simulation via /callback returned $CB_CODE"
  fi

  # Verify bot logged the page turn (within this phase's log range)
  sleep 1
  if tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE" | grep "Callback page turn" > /dev/null 2>&1; then
    pass "Bot logged callback page turn"
  else
    fail "Bot did not log callback page turn"
  fi
elif [ "$PAGINATION_FOUND" = false ]; then
  echo "  Skipping page turn test (pagination was not triggered)"
fi
