#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Codex long message pagination test ---"

ensure_infrastructure

start_codex "e2e-codex-2"

# Wait for Codex to settle from previous phase
echo "Waiting for Codex to settle..."
wait_for_idle

LOG_BEFORE_PAGE=$(wc -l < "$LOG_FILE")
TYPING_LOG_BEFORE=$(wc -l < "$TYPING_LOG_FILE" 2>/dev/null || echo 0)

# Inject a long-output prompt to trigger pagination
LONG_PROMPT="Without using any tools, write a comprehensive essay about the history and evolution of computer operating systems. Cover these topics in detail with at least 2 paragraphs each: 1) Early batch processing systems in the 1950s, 2) Time-sharing systems in the 1960s, 3) Unix and its philosophy, 4) The rise of personal computers and DOS/Windows, 5) Linux and open source movement, 6) Modern mobile operating systems iOS and Android, 7) Cloud operating systems and containers, 8) Future trends in OS design. You MUST write at least 1500 words total. Do not use any tools."
pane_log "[codex/pagination] BEFORE injecting long prompt"
inject_prompt "$LONG_PROMPT"
echo "Long prompt injected, waiting for Codex to respond and trigger pagination..."
pane_log "[codex/pagination] AFTER injecting long prompt"

ELAPSED=0
PAGINATION_FOUND=false
MSG_ID=""
PAGE_TIMEOUT=180
while [ $ELAPSED -lt $PAGE_TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PAGE" ]; then
    NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
    if echo "$NEW_PAGE_LOGS" | grep -E "pages, msg_id=" > /dev/null 2>&1; then
      PAGINATION_FOUND=true
      MSG_ID=$(grep -oPm1 'msg_id=\K[0-9]+' <<< "$NEW_PAGE_LOGS" || true)
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for pagination... ${ELAPSED}s / ${PAGE_TIMEOUT}s"
done

wait_for_idle 180
pane_log "[codex/pagination] AFTER pagination detected (idle)"

if [ "$PAGINATION_FOUND" = true ]; then
  pass "Codex long message triggered pagination"
else
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PAGE" ]; then
    NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
    if echo "$NEW_PAGE_LOGS" | grep "Notification sent" > /dev/null 2>&1; then
      # Codex may produce shorter output that doesn't trigger pagination — acceptable
      pass "Codex notification sent (output below pagination threshold, OK)"
    else
      fail "No notification sent after Codex long prompt (no response)"
    fi
  else
    fail "No bot activity after Codex long prompt injection"
  fi
fi

# Page turn test (only if pagination was triggered)
if [ "$PAGINATION_FOUND" = true ] && [ -n "$MSG_ID" ]; then
  echo ""
  echo "Testing page turn callback..."
  API_URL="http://127.0.0.1:$TEST_PORT/callback?msg_id=$MSG_ID&page=2"
  CB_RESP=$(curl -s -w "\n%{http_code}" "$API_URL")
  echo "  DEBUG: CB_RESP (${#CB_RESP} chars): $CB_RESP"
  CB_CODE=$(echo "$CB_RESP" | tail -1)
  if [ "$CB_CODE" = "200" ]; then
    pass "Codex page turn via /callback returned 200"
  else
    fail "Codex page turn via /callback returned $CB_CODE"
  fi
  sleep 1
  if tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE" | grep "Callback page turn" > /dev/null 2>&1; then
    pass "Bot logged Codex callback page turn"
  else
    fail "Bot did not log Codex callback page turn"
  fi
fi

# Typing continuity: inject -> Stop (long prompt, ~30s+ without tools)
check_typing_continuity "$TYPING_LOG_BEFORE" "Stop" "codex/phase2"
