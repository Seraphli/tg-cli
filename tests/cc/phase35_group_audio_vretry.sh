#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Group audio intake + real vretry callback dispatch test ---"

ensure_infrastructure
pane_log "[group_audio_vretry] BEFORE test"

# ============================================================
# Helper: wait_for_log_pattern <since_line> <pattern> <timeout_s> <label>
# Polls bot log until pattern is found or timeout (local to this phase, mirrors phase31's helper).
# ============================================================
wait_for_log_pattern() {
  local since="$1"
  local pattern="$2"
  local timeout="${3:-$TIMEOUT}"
  local label="${4:-pattern}"
  local elapsed=0
  while [ $elapsed -lt $timeout ]; do
    if tail -n +"$((since + 1))" "$LOG_FILE" | grep -qE "$pattern" 2>/dev/null; then
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  return 1
}

[ -n "${DEFAULT_CHAT_ID:-}" ] || fail "DEFAULT_CHAT_ID not set (pairing missing)"

# ============================================================
# TC-a: a GROUP audio update reaches the OnAudio handler (group/supergroup-only routing, R4).
# Pure synthetic dispatch via /test/update -> real bot.ProcessUpdate -> real bot.Handle(tele.OnAudio, ...).
# The synthetic chat_id is a fake group (no session bound to it); sender_id is the REAL paired user so
# pairing.IsAllowed(userID) passes regardless of the (unpaired) synthetic chat.
# ============================================================
echo ""
echo "  TC-a: group audio /test/update reaches OnAudio"

GROUP_CHAT_ID=-1009999001 # neutral synthetic fake supergroup id, no session bound to it
CUR_A=$(wc -l < "$LOG_FILE")
pane_log "[group_audio_vretry] TC-a BEFORE /test/update audio"
curl -sf -X POST "http://127.0.0.1:$TEST_PORT/test/update" \
  -H 'Content-Type: application/json' \
  -d "{\"chat_id\":$GROUP_CHAT_ID,\"chat_type\":\"group\",\"sender_id\":$DEFAULT_CHAT_ID,\"audio\":{\"file_id\":\"BOGUS_P35_AUDIO\",\"file_name\":\"clip.mp3\"}}" > /dev/null 2>&1 || true
pane_log "[group_audio_vretry] TC-a AFTER /test/update audio"

# "Audio file lookup failed" is logged ONLY from inside downloadAudio (messages.go), reached exclusively
# via a registered handler (OnAudio/OnVoice/OnDocument/vretry) calling handleAudioTranscription. This is
# DISTINCT from "TG recv audio", which LogIncomingUpdate emits at the poller/test-endpoint layer for
# EVERY update regardless of whether any handler exists (this is exactly how a group audio update used
# to be silently dropped before the OnAudio handler existed). Finding this line proves OnAudio was
# REACHED and attempted the download for our fake file_id.
if wait_for_log_pattern "$CUR_A" "Audio file lookup failed" 20 "TC-a OnAudio handler reached"; then
  pass "TC-a: group audio update reached the OnAudio handler (Audio file lookup failed after synthetic dispatch)"
else
  fail "TC-a: OnAudio handler not reached within 20s (group audio update dropped)"
fi

# ============================================================
# TC-b: a REAL "vretry" callback update, dispatched via /test/raw-callback -> real bot.ProcessUpdate ->
# real cbackRx match -> the REAL registered InlineButton{Unique:"vretry"} handler — NOT the /test/callback
# synthetic echo (which returns a generic JSON echo and never invokes any handler). The forced engine is
# asserted via the vretry handler's own observability log line.
# ============================================================
echo ""
echo "  TC-b: real vretry callback dispatch with forced engine"

FORCED_ENGINE="whisper"
CUR_B=$(wc -l < "$LOG_FILE")
pane_log "[group_audio_vretry] TC-b BEFORE /test/raw-callback vretry"
curl -sf -X POST "http://127.0.0.1:$TEST_PORT/test/raw-callback" \
  -H 'Content-Type: application/json' \
  -d "{\"unique\":\"vretry\",\"data\":\"$FORCED_ENGINE|0|0\",\"chat_id\":$DEFAULT_CHAT_ID,\"sender_id\":$DEFAULT_CHAT_ID,\"msg_id\":900555001,\"reply_to\":{\"id\":900555002,\"audio\":{\"file_id\":\"BOGUS_P35_VRETRY\",\"file_name\":\"clip.mp3\"}}}" > /dev/null 2>&1 || true
pane_log "[group_audio_vretry] TC-b AFTER /test/raw-callback vretry"

# "vretry: retrying with forced engine=" is emitted ONLY inside the REAL registered vretry handler
# (cmd/handlers/callbacks.go), after it has validated the callback's ReplyTo media and parsed the forced
# engine out of the payload. It can NEVER come from the /test/callback echo path (that endpoint only
# returns a JSON echo of unique/data and never calls into handler code). Finding it, with our forced
# engine value, proves ProcessUpdate routed the synthetic callback to the real vretry handler.
if wait_for_log_pattern "$CUR_B" "vretry: retrying with forced engine=$FORCED_ENGINE" 20 "TC-b vretry handler reached"; then
  pass "TC-b: real vretry callback dispatched, forced engine=$FORCED_ENGINE (distinct from /test/callback echo)"
else
  fail "TC-b: vretry handler not reached with forced engine within 20s"
fi

# The forced-engine retry shares handleAudioTranscription with TC-a, so the same download attempt marker
# confirms it actually ran the retry logic (not just logged the marker line and stopped).
if wait_for_log_pattern "$CUR_B" "Audio file lookup failed" 20 "TC-b download attempt"; then
  pass "TC-b: forced-engine retry attempted the download (Audio file lookup failed, as expected for a fake file_id)"
else
  fail "TC-b: forced-engine retry did not attempt the download within 20s"
fi

pane_log "[group_audio_vretry] AFTER test"
pass "phase complete"
