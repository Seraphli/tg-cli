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
    if grep -qE "$pattern" <<< "$(tail -n +"$((since + 1))" "$LOG_FILE")" 2>/dev/null; then
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

# ============================================================
# TC-c: a REAL audio status lifecycle, dispatched into the REAL paired private chat $DEFAULT_CHAT_ID
# (not TC-a/TC-b's synthetic fake group/sentinel ids, which never reach the real Bot API). Proves the
# status message's send + failure edit + vretry retry re-edit all actually succeed against Telegram,
# not just that handler code runs. SPEC.md TC13(a)-(e).
# ============================================================
echo ""
echo "  TC-c: real-chat audio status lifecycle (send, fail, vretry retry) via the real Bot API"

# (a) Back up config.json BEFORE editing it, and CHAIN our restore onto whatever EXIT trap is already
# active (e2e_common.sh's _e2e_on_exit when E2E_ORCHESTRATED=1, or cleanup_sessions in a standalone run
# — ensure_infrastructure, called at the top of this script, installs whichever applies). A bare
# `trap ... EXIT` would silently REPLACE it and skip the harness's own error-logging/session cleanup.
cp "$TEST_CONFIG_DIR/config.json" "$TEST_CONFIG_DIR/config.json.p35-backup"

TC_C_PREV_EXIT_TRAP="$(trap -p EXIT)"
TC_C_PREV_EXIT_CMD="$(sed -n "s/^trap -- '\(.*\)' EXIT\$/\1/p" <<<"$TC_C_PREV_EXIT_TRAP")"

_tc_c_restore_config() {
  # Captured FIRST, before anything else in this function can overwrite $? or trip the ERR trap on top
  # of it. trap - ERR / set +e come next, BEFORE any restore command: this script runs under
  # e2e_common.sh's set -euo pipefail + set -E + its own installed ERR trap, so without disabling both
  # first, the later `(exit "$saved_rc")` line is itself a "failing command" whenever saved_rc is
  # nonzero — under errexit that aborts this trap before eval can run; under the still-live ERR trap it
  # fires and overwrites the harness's own last-error diagnostic with this restore step's own command
  # instead of the real failure. Verified live with a 20-line harness reproducing set -euo pipefail +
  # set -E + an ERR trap: without trap - ERR/set +e, the chained original EXIT handler never ran on
  # `exit 7` or on a failing command; with them, it ran correctly and the real last error was preserved.
  local saved_rc=$?
  trap - ERR
  set +e
  # G1: config.json is read continuously by the live bot daemon, so the restore must be atomic too — a
  # plain cp truncates the file first, which a concurrently-reading daemon could observe mid-write. Copy
  # the backup to a temp file in the SAME directory, then mv -f it over config.json: a same-filesystem
  # rename is atomic (same guarantee os.replace gives the python edit below).
  cp "$TEST_CONFIG_DIR/config.json.p35-backup" "$TEST_CONFIG_DIR/config.json.p35-restore.tmp" 2>/dev/null || true
  mv -f "$TEST_CONFIG_DIR/config.json.p35-restore.tmp" "$TEST_CONFIG_DIR/config.json" 2>/dev/null || true
  rm -f "$TEST_CONFIG_DIR/config.json.p35-backup" 2>/dev/null || true
  # Reset $? back to saved_rc (the cp/mv/rm above changed it) via a subshell's own exit status — NOTHING
  # else may run between this line and the eval below, or it would overwrite $? again first.
  (exit "$saved_rc")
  eval "$TC_C_PREV_EXIT_CMD"
}
trap '_tc_c_restore_config' EXIT

# G1: written atomically (temp file + os.replace, the tests/e2e_common.sh:853-866 template) — the bot
# daemon reads config.json continuously, and a plain json.dump(open(f,'w')) truncates the file first.
python3 -c "
import json, os, tempfile
path = '$TEST_CONFIG_DIR/config.json'
with open(path) as f:
    cfg = json.load(f)
cfg['whisperPath'] = '/usr/bin/true'
cfg['modelPath'] = '/tmp/p35-dummy-model'
d = os.path.dirname(path)
fd, tmp = tempfile.mkstemp(dir=d, prefix='.config.', suffix='.tmp')
with os.fdopen(fd, 'w') as f:
    json.dump(cfg, f)
os.replace(tmp, path)
"

# (c) A real settings message gives us a message id that genuinely exists in this chat. The dispatched
# voice message's OWN id (msg_id, below) must be this real id: handleAudioTranscription's new-entry send
# is bot.Reply(c.Message(), text), and telebot's Reply sets reply_to_message_id = c.Message().ID — i.e.
# msg_id ITSELF, not the voice message's own ReplyTo field. A synthetic counter id would make the real
# send's success depend on whatever unrelated message happens to occupy that id in chat history.
SET_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/settings_message?chat_id=$DEFAULT_CHAT_ID")
ANCHOR_MSG_ID=$(echo "$SET_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('msg_id',''))")
[ -n "$ANCHOR_MSG_ID" ] || fail "TC-c: settings_message returned no msg_id (resp=$SET_RESP)"

# reply_to_msg_id below is a SEPARATE field: it becomes the voice message's OWN .ReplyTo, which is NEVER
# sent to Telegram (tg-cli never sets AllowWithoutReply) — it only has to be non-zero to satisfy
# OnVoice's private-chat gate (messages.go: early-returns ONLY on "private AND no ReplyTo"). Any nonzero
# value works; it does not need to reference a real message.
TC_C_REPLY_TO_MSG_ID=900555003

CUR_C1=$(wc -l < "$LOG_FILE")
pane_log "[group_audio_vretry] TC-c BEFORE /test/update voice (real chat)"
curl -sf -X POST "http://127.0.0.1:$TEST_PORT/test/update" \
  -H 'Content-Type: application/json' \
  -d "{\"chat_id\":$DEFAULT_CHAT_ID,\"sender_id\":$DEFAULT_CHAT_ID,\"msg_id\":$ANCHOR_MSG_ID,\"reply_to_msg_id\":$TC_C_REPLY_TO_MSG_ID,\"voice\":{\"file_id\":\"BOGUS_P35_REALCHAT\"}}" > /dev/null 2>&1 || true
pane_log "[group_audio_vretry] TC-c AFTER /test/update voice (real chat)"

# (d) The status message newAudioStatus creates is a BRAND-NEW message (bot.Reply(c.Message(), text)),
# with its OWN id assigned by Telegram — NOT $ANCHOR_MSG_ID (that is only the message it replies to).
# Wait for ANY msg_id at state=transcribing in this window, then extract that id as N (requiring exactly
# one such line) and require N != ANCHOR_MSG_ID, proving a genuinely new message was sent. Using
# $ANCHOR_MSG_ID directly in the state patterns, as an earlier draft did, could never match.
TC_C_TRANSCRIBING_ANY="^\[[^]]+\] \[PID=[0-9]+\] \[INFO\] Voice status: msg_id=[0-9]+ state=transcribing\$"

if wait_for_log_pattern "$CUR_C1" "$TC_C_TRANSCRIBING_ANY" 20 "TC-c initial send"; then
  pass "TC-c: initial status send succeeded on the real chat (state=transcribing)"
else
  fail "TC-c: no real-chat initial send (state=transcribing) within 20s"
fi

# Extract N and its occurrence count over the bounded window with a single awk pass whose END block
# always runs and always prints two numbers, so this command's own exit status is always 0 — nothing
# here can trip set -e or the ERR trap. ENVIRON (not awk -v) carries the pattern so its backslash
# escapes reach the regex engine unmolested.
CUR_C1_AFTER=$(wc -l < "$LOG_FILE")
read -r TC_C_N_COUNT N < <(PAT="$TC_C_TRANSCRIBING_ANY" awk -v s="$CUR_C1" -v e="$CUR_C1_AFTER" '
  NR>s && NR<=e && $0 ~ ENVIRON["PAT"] {
    match($0, /msg_id=[0-9]+/)
    val = substr($0, RSTART+7, RLENGTH-7)
    c++
  }
  END { print c+0, val+0 }
' "$LOG_FILE")

if [ "$TC_C_N_COUNT" -eq 1 ]; then
  pass "TC-c: exactly one new-message transcribing status line found in this window"
else
  fail "TC-c: expected exactly one new-message transcribing status line, found $TC_C_N_COUNT"
fi
if [ "$N" != "$ANCHOR_MSG_ID" ]; then
  pass "TC-c: status message id ($N) differs from the settings-message anchor id ($ANCHOR_MSG_ID) — a brand-new status message was sent"
else
  fail "TC-c: status message id ($N) must differ from the anchor id ($ANCHOR_MSG_ID)"
fi

TC_C_FAILED="^\[[^]]+\] \[PID=[0-9]+\] \[INFO\] Voice status: msg_id=$N state=failed\$"
TC_C_RETRYING="^\[[^]]+\] \[PID=[0-9]+\] \[INFO\] Voice status: msg_id=$N state=retrying\$"
TC_C_EDIT_ERROR="^\[[^]]+\] \[PID=[0-9]+\] \[ERROR\] Voice status: msg_id=$N edit_error="
TC_C_SEND_ERROR="^\[[^]]+\] \[PID=[0-9]+\] \[ERROR\] Voice status: send_error="

# Same 20s budget, same since-line (CUR_C1): the failure edit for a download error on a bogus file_id
# follows the initial send almost immediately, well inside the same window — no need for a 2nd since-line.
if wait_for_log_pattern "$CUR_C1" "$TC_C_FAILED" 20 "TC-c initial failure edit"; then
  pass "TC-c: failure edit succeeded on the real chat (msg_id=$N state=failed)"
else
  fail "TC-c: no real-chat failure edit (state=failed) for msg_id=$N within 20s"
fi

# Bounded-window absence checks: an `if` guard around a plain awk range-match, no pipe into grep -q and
# no bare nonzero-exiting command outside an if/&&/||. The earlier approach (set +eo pipefail;
# tail|head|grep -qE; set -eo pipefail) still let a PASSING (i.e. no-match, grep exit 1) outcome trip
# this script's own ERR trap: set -E's ERR trap fires on any nonzero command regardless of pipefail, and
# set +e does NOT suppress ERR — verified live, the same tail|head|grep -qE pipe fired the ERR trap and
# overwrote _e2e_last_error even though the match correctly came up empty. An `if` condition is exempt
# from both -e and the ERR trap by bash's own rules — verified live to produce no trap firing on either
# outcome (match or no match).
CUR_C2=$(wc -l < "$LOG_FILE")
if PAT="$TC_C_EDIT_ERROR" awk -v s="$CUR_C1" -v e="$CUR_C2" 'NR>s && NR<=e && $0 ~ ENVIRON["PAT"] {f=1; exit} END {exit !f}' "$LOG_FILE"; then
  fail "TC-c: an edit_error was logged for msg_id=$N during the initial send+fail window"
else
  pass "TC-c: no edit_error for msg_id=$N in the initial send+fail window"
fi
# send_error lines (newAudioStatus's own send failure) carry no msg_id, so this check is global to the
# window rather than keyed to N — any send_error in this window indicates the initial send itself failed.
if PAT="$TC_C_SEND_ERROR" awk -v s="$CUR_C1" -v e="$CUR_C2" 'NR>s && NR<=e && $0 ~ ENVIRON["PAT"] {f=1; exit} END {exit !f}' "$LOG_FILE"; then
  fail "TC-c: a send_error was logged during the initial send+fail window"
else
  pass "TC-c: no send_error in the initial send+fail window"
fi

# (e) Retry via the REAL vretry handler (not the /test/callback synthetic echo — see TC-b's own comment
# above). msg_id is N (the status message captured in (d)), NOT $ANCHOR_MSG_ID: using the anchor here
# would make the vretry callback edit the SETTINGS message instead of the status message.
# reply_to.id is a purely local field fed straight into bot.ProcessUpdate's synthetic Callback.Message
# (cmd/api/test_callback.go /test/raw-callback) — never round-tripped through Telegram, so unlike
# msg_id it does not need to reference anything real; "900555004" is just a nonzero sentinel, same
# convention as TC-b's own fake ids.
CUR_C3=$(wc -l < "$LOG_FILE")
pane_log "[group_audio_vretry] TC-c BEFORE /test/raw-callback vretry (real chat)"
curl -sf -X POST "http://127.0.0.1:$TEST_PORT/test/raw-callback" \
  -H 'Content-Type: application/json' \
  -d "{\"unique\":\"vretry\",\"data\":\"whisper|0|0\",\"chat_id\":$DEFAULT_CHAT_ID,\"sender_id\":$DEFAULT_CHAT_ID,\"msg_id\":$N,\"reply_to\":{\"id\":900555004,\"voice\":{\"file_id\":\"BOGUS_P35_REALCHAT\"}}}" > /dev/null 2>&1 || true
pane_log "[group_audio_vretry] TC-c AFTER /test/raw-callback vretry (real chat)"

# Polls tail -n +(CUR_C3+1) every 1s for up to 20s — the real editMessageText round-trip clearing the
# old retry button(s) in the same edit that starts the retry.
if wait_for_log_pattern "$CUR_C3" "$TC_C_RETRYING" 20 "TC-c retry status edit"; then
  pass "TC-c: retry status edit succeeded on the real chat (msg_id=$N state=retrying)"
else
  fail "TC-c: no real-chat retry edit (state=retrying) for msg_id=$N within 20s"
fi

# Same 20s budget, same since-line (CUR_C3): the SECOND failure edit (the forced whisper retry also
# fails, same bogus file_id) follows the retrying edit almost immediately.
if wait_for_log_pattern "$CUR_C3" "$TC_C_FAILED" 20 "TC-c retry failure edit"; then
  pass "TC-c: retry failure edit succeeded on the real chat (msg_id=$N state=failed, button restored)"
else
  fail "TC-c: no real-chat retry failure edit (state=failed) for msg_id=$N within 20s"
fi

# Bounded-window absence check for the retry section, mirroring the one after (d) above.
CUR_C4=$(wc -l < "$LOG_FILE")
if PAT="$TC_C_EDIT_ERROR" awk -v s="$CUR_C3" -v e="$CUR_C4" 'NR>s && NR<=e && $0 ~ ENVIRON["PAT"] {f=1; exit} END {exit !f}' "$LOG_FILE"; then
  fail "TC-c: an edit_error was logged for msg_id=$N during the retry window"
else
  pass "TC-c: no edit_error for msg_id=$N in the retry window"
fi

pane_log "[group_audio_vretry] AFTER test"
pass "phase complete"
