#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Long message pagination test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-3"

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

# Wait for streaming continuation (Stream send: chunk=1 means a second TG message was started)
ELAPSED=0
PAGINATION_FOUND=false
PAGE_TIMEOUT=180
while [ $ELAPSED -lt $PAGE_TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PAGE" ]; then
    NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
    # chunk=1 means the second continuation message was sent (0-indexed)
    if echo "$NEW_PAGE_LOGS" | grep -E "Stream send:.*chunk=1" > /dev/null 2>&1; then
      PAGINATION_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for streaming continuation... ${ELAPSED}s / ${PAGE_TIMEOUT}s"
done

wait_for_idle 180
pane_log "[pagination] AFTER pagination detected (idle)"

if [ "$PAGINATION_FOUND" = true ]; then
  pass "Long message triggered streaming continuation (multi-chunk — no page-turn keyboard)"
  # Verify there is NO page-turn keyboard (inline keyboard) on streamed messages
  # sendEventNotification with reply_markup would produce a different log; streaming never adds keyboards
  NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
  if echo "$NEW_PAGE_LOGS" | grep "reply_markup.*page" > /dev/null 2>&1; then
    fail "Streaming message unexpectedly has page-turn keyboard (reply_markup)"
  else
    pass "No page-turn keyboard on streaming continuation messages"
  fi
else
  # Check if Claude sent any streaming message at all
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_PAGE" ]; then
    NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
    if echo "$NEW_PAGE_LOGS" | grep "Stream send:" > /dev/null 2>&1; then
      fail "Long message did not trigger continuation (Claude output too short — no chunk=1)"
    else
      fail "No streaming activity after long prompt (Claude may not have responded)"
    fi
  else
    fail "No bot activity after long prompt injection"
  fi
fi

# Typing continuity: inject → Stop (long prompt, ~30s+ without tools)
check_typing_continuity "$TYPING_LOG_BEFORE" "Stop" "phase3"
