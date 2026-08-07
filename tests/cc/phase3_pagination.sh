#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Moderate turn stays single rich message (RichMaxRunes=30000) ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-3"

# Record log position before injecting long prompt
LOG_BEFORE_PAGE=$(wc -l < "$LOG_FILE")
TYPING_LOG_BEFORE=$(wc -l < "$TYPING_LOG_FILE" 2>/dev/null || echo 0)

# Wait for Claude to settle after bot_hook
echo "Waiting for Claude to settle..."
wait_for_idle

# Inject a moderate-length prompt (~180-240 words ≈ ~1700 runes, well under RichMaxRunes=30000)
# SPEC 2.3: rich messages stay single up to 32768 chars — this turn must NOT trigger a continuation chunk
LONG_PROMPT="Without using any tools, write a single flowing paragraph of between 180 and 240 words about the history and evolution of computer operating systems — touch on early batch processing, 1960s time-sharing, Unix and its philosophy, the rise of personal computers, Linux and open source, and modern mobile operating systems. Write it as one continuous prose response (no lists, no headings). Do not use any tools."
pane_log "[pagination] BEFORE injecting long prompt"
inject_prompt "$LONG_PROMPT"
echo "Long prompt injected, waiting for Claude to respond (expecting single rich message, no continuation)..."
pane_log "[pagination] AFTER injecting long prompt"

# Wait for any streaming activity (Stream send: or Stream edit:). Mirror wait_for_idle's busy-extend
# (e2e_common.sh:255-298): a genuinely-slow model (mimo took 194s > 180s in the range run) must not
# false-fail. On inner-window timeout, diagnose the session; if it is still busy, extend up to 3 more
# rounds. Break the OUTER loop IMMEDIATELY once STREAM_FOUND is true, before diagnosing.
STREAM_FOUND=false
PAGE_TIMEOUT=180
STREAM_MAX_RETRIES=3
stream_retry=0
while true; do
  ELAPSED=0
  while [ $ELAPSED -lt $PAGE_TIMEOUT ]; do
    LOG_NOW=$(wc -l < "$LOG_FILE")
    if [ "$LOG_NOW" -gt "$LOG_BEFORE_PAGE" ]; then
      NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
      if echo "$NEW_PAGE_LOGS" | grep -E "Stream send:|Stream edit:" > /dev/null 2>&1; then
        STREAM_FOUND=true
        break
      fi
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
    echo "  Waiting for streaming activity... ${ELAPSED}s / ${PAGE_TIMEOUT}s (round $((stream_retry + 1))/$((STREAM_MAX_RETRIES + 1)))"
  done
  # Break the OUTER loop immediately on detection, BEFORE diagnosing the session.
  [ "$STREAM_FOUND" = true ] && break
  # Inner window timed out with no stream — diagnose busy vs idle vs unknown (mirrors wait_for_idle).
  STREAM_DIAG=$(check_session_idle "$E2E_PANE")
  if [ "$STREAM_DIAG" = "busy" ] && [ $stream_retry -lt $STREAM_MAX_RETRIES ]; then
    stream_retry=$((stream_retry + 1))
    echo "  BUSY: LLM still processing, extending stream wait (round ${stream_retry}/${STREAM_MAX_RETRIES}, +${PAGE_TIMEOUT}s)..."
    continue
  fi
  # idle (turn ended, no stream) or unknown or retries exhausted: stop polling, leave STREAM_FOUND=false.
  break
done

wait_for_idle 180
pane_log "[pagination] AFTER stream detected (idle)"

if [ "$STREAM_FOUND" = true ]; then
  # Streaming happened — now assert NO continuation chunk (chunk=1 must be absent)
  NEW_PAGE_LOGS=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
  if echo "$NEW_PAGE_LOGS" | grep -E "Stream send:.*chunk=1" > /dev/null 2>&1; then
    fail "Moderate turn unexpectedly triggered continuation chunk=1 (response exceeded RichMaxRunes=30000)"
  else
    pass "Moderate turn produced a single rich stream message (no continuation at RichMaxRunes)"
  fi
  # v10: the single message is delivered via sendRichMessage (fmt=rich on the stream marker).
  if echo "$NEW_PAGE_LOGS" | grep -qE "Stream (send|edit):.*fmt=rich" 2>/dev/null; then
    pass "Moderate turn streamed via rich message (fmt=rich)"
  else
    fail "Moderate turn stream marker missing fmt=rich (expected rich message)"
  fi
  # Verify there is NO page-turn keyboard (inline keyboard) on streamed messages
  # Streaming never adds keyboards regardless of message length
  if echo "$NEW_PAGE_LOGS" | grep "reply_markup.*page" > /dev/null 2>&1; then
    fail "Streaming message unexpectedly has page-turn keyboard (reply_markup)"
  else
    pass "No page-turn keyboard on streaming messages"
  fi
else
  # No streaming activity — distinguish (a) valid Stop direct_send delivery (model emitted a single final
  # message so the whole response was delivered at Stop time, not incrementally streamed), (b) log activity
  # but no delivery (streaming broken), (c) total no-response.
  LOG_NOW=$(wc -l < "$LOG_FILE")
  PAGE_SLICE=$(tail -n +"$((LOG_BEFORE_PAGE + 1))" "$LOG_FILE")
  if echo "$PAGE_SLICE" | grep -q "Stop terminal: outcome=direct_send"; then
    pass "Moderate turn delivered via Stop direct_send (model emitted a single final message; incremental-streaming/pagination assertions not exercised this run)"
  elif [ "$LOG_NOW" -gt "$LOG_BEFORE_PAGE" ]; then
    fail "Bot log activity present but no streaming (Stream send:/Stream edit:) found — streaming may be broken"
  else
    fail "No bot activity after long prompt injection"
  fi
fi

# Typing continuity: inject → Stop (180-240 word prose, no tools)
check_typing_continuity "$TYPING_LOG_BEFORE" "Stop" "phase3"
