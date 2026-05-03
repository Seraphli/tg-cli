#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Long message pagination test ---"

ensure_infrastructure

# Record log position before injecting long prompt
LOG_BEFORE_PAGE=$(wc -l < "$LOG_FILE")
TYPING_LOG_BEFORE=$(wc -l < "$TYPING_LOG_FILE" 2>/dev/null || echo 0)

# Wait for Claude to settle after bot_hook
echo "Waiting for Claude to settle..."
wait_for_idle

# Inject a long-output prompt to trigger pagination
LONG_PROMPT="Without using any tools, write a comprehensive essay about the history and evolution of computer operating systems. Cover these topics in detail with at least 2 paragraphs each: 1) Early batch processing systems in the 1950s, 2) Time-sharing systems in the 1960s, 3) Unix and its philosophy, 4) The rise of personal computers and DOS/Windows, 5) Linux and open source movement, 6) Modern mobile operating systems iOS and Android, 7) Cloud operating systems and containers, 8) Future trends in OS design. You MUST write at least 1500 words total. Do not use any tools."
pane_log "[pagination] BEFORE injecting long prompt"
inject_prompt "$LONG_PROMPT"
echo "Long prompt injected, waiting for Claude to respond and trigger pagination..."
pane_log "[pagination] AFTER injecting long prompt"

# Wait for bot log to contain multi-page notification indicator
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
pane_log "[pagination] AFTER pagination detected (idle)"

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
  echo "  DEBUG: CB_RESP (${#CB_RESP} chars): $CB_RESP"
  CB_CODE=$(echo "$CB_RESP" | tail -1)
  if [ "$CB_CODE" = "200" ]; then
    pass "Page turn simulation via /callback returned 200"
  else
    fail "Page turn simulation via /callback returned $CB_CODE"
  fi

  # Verify bot logged the page turn (within this phase's log range)
  sleep 1
  PAGE_TURN_LOG=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE" | grep "Callback page turn" | tail -1 || true)
  if [ -n "$PAGE_TURN_LOG" ]; then
    pass "Bot logged callback page turn"
  else
    fail "Bot did not log callback page turn"
  fi
  # Bug 1 regression guard: rebuilt page must preserve cli / context_pct / agent fields.
  # The enhanced callback log (cmd/api/pagination.go) emits all three fields pulled from PageEntry.
  # If PageEntry extension breaks, these fields will be missing or empty and grep will fail.
  # Numerical correctness of the restored values is covered by TestPageEntry_RoundtripExtendedFields.
  set +eo pipefail
  echo "$PAGE_TURN_LOG" | grep -qE 'cli="[^"]*" context_pct=-?[0-9]+ agent="[^"]*"'
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  echo "  DEBUG: grep 'extended PageEntry fields' PIPESTATUS=${_ps[*]}"
  if [ "${_ps[1]}" -eq 0 ]; then
    pass "Bug 1 regression guard: callback log contains extended PageEntry fields"
  else
    fail "Bug 1: callback log missing extended PageEntry fields: $PAGE_TURN_LOG"
  fi
elif [ "$PAGINATION_FOUND" = false ]; then
  echo "  Skipping page turn test (pagination was not triggered)"
fi

# Typing continuity: inject → Stop (long prompt, ~30s+ without tools)
check_typing_continuity "$TYPING_LOG_BEFORE" "Stop" "phase2"
