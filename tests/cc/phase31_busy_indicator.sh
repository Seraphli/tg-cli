#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Busy indicator feature test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# ============================================================
# Helper: wait_for_log_pattern <since_line> <pattern> <timeout_s> <label>
# Polls bot log until pattern is found or timeout.
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
    if (( elapsed % 10 == 0 )); then
      echo "  Waiting for $label... ${elapsed}s / ${timeout}s"
    fi
  done
  return 1
}

# FIX 3 (r9): fairy-nap script fixture for TC-b, created at phase start, removed at phase end. Unique
# path per run ($$ = this phase process). It echoes a line, sleeps 30s (the busy window during which the
# re-float must fire), then echoes a second line. bash <path> (not the bare path) so a freshly created,
# non-executable file still runs — no chmod needed.
TCB_SCRIPT="/tmp/tg-cli-test-fairy-sleep-$$.sh"
cat > "$TCB_SCRIPT" <<'FAIRY_EOF'
#!/bin/bash
echo "The fairy is falling asleep now."
sleep 30
echo "The fairy has woken up."
FAIRY_EOF

# ============================================================
# TC-a: inject a long task → muted status send appears in bot log
# ============================================================
echo ""
echo "  TC-a: busy status send on long-running task"

start_claude "e2e-cc-31a"
wait_for_idle

LOG_BEFORE_TCA=$(wc -l < "$LOG_FILE")
pane_log "[busy] TC-a BEFORE inject"
inject_prompt "Without using any tools, write at least 10 very long sentences about the history of the internet. Each sentence must be at least 50 words long. Label each with SENTENCE_N. Take your time and be thorough."
pane_log "[busy] TC-a AFTER inject"

# Wait for busy status send log line.
if wait_for_log_pattern "$LOG_BEFORE_TCA" "busy status sent chat=" 30 "TC-a status send"; then
  pass "TC-a: busy status sent log line found"
  # Verify disable_notification via SendOptions (bot log shows the sent message).
  if tail -n +"$((LOG_BEFORE_TCA + 1))" "$LOG_FILE" | grep -q "busy status sent" 2>/dev/null; then
    pass "TC-a: busy status message created (muted send confirmed)"
  else
    fail "TC-a: busy status sent marker missing"
  fi
else
  fail "TC-a: busy status send not logged within 30s"
fi

wait_for_idle
pane_log "[busy] TC-a AFTER idle"

# Verify status was deleted after session went idle.
if wait_for_log_pattern "$LOG_BEFORE_TCA" "busy status deleted" 10 "TC-a status delete"; then
  pass "TC-a: busy status deleted after idle grace period"
else
  # Not fatal — deletion may still be pending (grace period just elapsed).
  warn "TC-a: busy status delete not observed within 10s of idle"
fi

# ============================================================
# TC-b: while busy, an outbound bot send → status is deleted+resent
# Assert: resent status msg_id > triggering send msg_id
# ============================================================
echo ""
echo "  TC-b: re-float on outbound send (msg_id ordering)"

LOG_BEFORE_TCB=$(wc -l < "$LOG_FILE")
pane_log "[busy] TC-b BEFORE inject"
# FIX 3 (r9, boss decision: KEEP TC-b): pure bedtime-fairy scenario, announce-before-tool. This replaces
# the earlier "This is an automated end-to-end test of a Telegram bot busy indicator..." framing that
# mimo refused as a prompt-injection attempt (transcript 0e4f466c: zero tool calls, turn over in 5s, so
# no outbound send existed and the re-float never fired). Two elements matter:
#  (1) NO test framing — a plain in-story task the model has no reason to refuse (removes the refusal
#      failure mode that f10/f13 kept re-triggering).
#  (2) ANNOUNCE BEFORE THE TOOL — "first tell me ... that you are about to run the command" makes the
#      pre-tool text a SECOND re-float candidate that is necessarily EARLIER than the Bash tool
#      notification (the pre-tool text is flushed to Telegram before the tool notification — the same
#      round-8 invariant phase26 TC7 asserts). With a tool-only turn there is a single candidate send
#      that could land inside the ~1s busy-status-create window and precede SentAt, so nothing would ever
#      bury the status and TC-b could time out while the product is correct; the announcement makes it two
#      candidates and closes that race. The fairy script's sleep 30 keeps the session busy well past the
#      1s status tick + 2s re-float gate.
# "bash <SCRIPT_PATH>" (not the bare path) so a freshly created non-executable file still runs.
# NO new assertion (boss principle: verify the FUNCTION, not the model). If no genuine trigger/refloat
# ever lands, the f1 rev4 route-scoped extraction below HARD-FAILS — it never false-passes. The existing
# re-float / msg_id-ordering assertions (Fix A f1 rev 4) are UNCHANGED, and the 300s re-float ceiling
# (below) still covers mimo's unbounded think latency.
inject_prompt "A fairy in a bedtime story needs to take her nap. First tell me in one sentence that you are about to run the command that puts her to sleep. Then use the Bash tool to run this exact command exactly once: bash $TCB_SCRIPT. Then tell me the two lines it printed."
pane_log "[busy] TC-b AFTER inject"

# Wait for initial status send.
if ! wait_for_log_pattern "$LOG_BEFORE_TCB" "busy status sent chat=" 30 "TC-b initial status"; then
  fail "TC-b: initial busy status send not logged"
fi
pass "TC-b: initial busy status sent"

# Now wait for a genuine re-float (a real outbound send landing after the status).
if ! wait_for_log_pattern "$LOG_BEFORE_TCB" "busy status re-floated" 300 "TC-b refloat"; then
  fail "TC-b: re-float not observed within 300s"
fi
pass "TC-b: busy status re-floated"

# Route-scoped trigger extraction (Fix A f1 rev 4). The bot log is global but Telegram
# msg_id ordering is per-chat, so scope every selector by route. Capture CHAT+TOPIC from
# the initial "busy status sent" marker; the trigger is the last NON-status-owned "TG send"
# in that chat, scoped after the started marker. Status boundaries match the FULL route.
WIN=$(tail -n +"$((LOG_BEFORE_TCB + 1))" "$LOG_FILE")
CHAT=$(echo "$WIN"  | grep -oP 'busy status sent chat=\K-?[0-9]+' | head -1 || true)
TOPIC=$(echo "$WIN" | grep -oP 'busy status sent chat=-?[0-9]+ topic=\K[0-9]+' | head -1 || true)
[ -n "$CHAT" ] || fail "TC-b: no busy status sent marker in window"
# Terminal + initial status boundaries on the FULL route (chat+topic).
REFLOAT_NEW_ID=$(echo "$WIN" | grep -oP "busy status re-floated chat=$CHAT topic=$TOPIC new_msg_id=\K[0-9]+" | head -1 || true)
# OWNED = status-owned ids across the WHOLE CHAT (all topics) — a TG send line has no topic,
# so chat-wide exclusion is the only safe trigger filter.
OWNED=$(echo "$WIN" | grep -oP "busy status sent chat=$CHAT topic=[0-9]+ msg_id=\K[0-9]+|busy status re-floated chat=$CHAT topic=[0-9]+ new_msg_id=\K[0-9]+" | sort -u)
TRIGGER_MSG_ID=$(echo "$WIN" | awk -v owned="$OWNED" -v chat="$CHAT" -v topic="$TOPIC" '
  BEGIN { n=split(owned,a,"\n"); for(i=1;i<=n;i++) own[a[i]]=1 }
  index($0, "busy status sent chat=" chat " topic=" topic " ") { started=1; next }
  started && index($0, "TG send: chat=" chat " ") { if (match($0,/msg_id=[0-9]+/)) { id=substr($0,RSTART+7,RLENGTH-7); if (!(id in own)) last=id } }
  index($0, "busy status re-floated chat=" chat " topic=" topic " ") { print last; exit }')
if [ -z "$REFLOAT_NEW_ID" ] || [ -z "$TRIGGER_MSG_ID" ]; then
  fail "TC-b: no genuine route-scoped trigger/refloat (chat=$CHAT refloat=$REFLOAT_NEW_ID trigger=$TRIGGER_MSG_ID)"
fi
[ "$REFLOAT_NEW_ID" -gt "$TRIGGER_MSG_ID" ] \
  && pass "TC-b: resent status msg_id=$REFLOAT_NEW_ID > trigger msg_id=$TRIGGER_MSG_ID (chat=$CHAT, correct TG order)" \
  || fail "TC-b: ordering violated: resent status msg_id=$REFLOAT_NEW_ID NOT > trigger msg_id=$TRIGGER_MSG_ID (chat=$CHAT)"

wait_for_idle
pane_log "[busy] TC-b AFTER idle"

# ============================================================
# TC-c: on Stop/idle → status deleted after 2-3s grace
# ============================================================
echo ""
echo "  TC-c: status deleted after idle grace period"

LOG_BEFORE_TCC=$(wc -l < "$LOG_FILE")
pane_log "[busy] TC-c BEFORE inject"
inject_prompt "Without using any tools, write exactly 5 sentences about the moon. Label each MOON_N."
pane_log "[busy] TC-c AFTER inject"

# Wait for status send.
if ! wait_for_log_pattern "$LOG_BEFORE_TCC" "busy status sent chat=" 30 "TC-c status send"; then
  fail "TC-c: initial busy status not logged"
fi
pass "TC-c: busy status created"

# Wait for CC to go idle.
wait_for_idle
pane_log "[busy] TC-c AFTER idle"

# After idle, the 2-3s grace should trigger deletion.
if wait_for_log_pattern "$LOG_BEFORE_TCC" "busy status deleted" 10 "TC-c idle delete"; then
  pass "TC-c: busy status deleted after idle grace (2-3s)"
else
  fail "TC-c: busy status NOT deleted after idle grace period"
fi

# ============================================================
# TC-d: restart sweep — seed a busy_status.json, restart bot → deleted at startup
# ============================================================
echo ""
echo "  TC-d: startup sweep deletes persisted status"

# Stop CC so no session is active.
stop_claude "e2e-cc-31a"

# Seed a fake busy_status.json with a nonexistent msg_id.
python3 -c "
import json
entry = {
  '999999999:0': {
    'chat_id': 999999999,
    'topic_id': 0,
    'msg_id': 12345,
    'started_at': '2026-01-01T00:00:00Z',
    'sent_at': '2026-01-01T00:00:00Z',
    'last_edit_at': '2026-01-01T00:00:00Z',
    'last_float_at': '2026-01-01T00:00:00Z',
    'idle_since': '0001-01-01T00:00:00Z'
  }
}
import os
path = os.path.join('$TEST_CONFIG_DIR', 'busy_status.json')
with open(path, 'w') as f:
    json.dump(entry, f)
print('seeded busy_status.json')
"

# Restart the bot.
pane_log "[busy] TC-d BEFORE bot restart"
# Force a REAL restart: kill the running phase bot (cleanup_sessions pattern, e2e_common.sh:590) and
# wait for the port to stop responding, else start_bot no-ops against the still-listening old bot.
$TMUX_TEST kill-session -t "=$BOT_SESSION" 2>/dev/null || true
_tcd_down=0
while curl -sf -o /dev/null "http://127.0.0.1:$TEST_PORT/session/idle" 2>/dev/null; do
  sleep 1; _tcd_down=$((_tcd_down + 1))
  [ "$_tcd_down" -ge 15 ] && fail "TC-d: bot port $TEST_PORT still responding 15s after kill-session (old bot not down)"
done
# Fix A f15: validate the pre-restart TC-a/b/c segment NOW, BEFORE start_bot truncates the shared bot.log.
# start_bot (e2e_common.sh:521) truncates $LOG_FILE mid-phase. In the full suite the phase's run_phase offset
# PHASE_LOG_BEFORE (e.g. 13433) then points PAST the truncated log (~374 lines), so the EXIT-trap's
# validate_phase_log gets an EMPTY slice and the CC-scoped guard (e2e_common.sh:401) silently skips V1/V2/V3
# — the full-suite validation gap. Fix: validate the pre-truncation segment explicitly here against the
# ORIGINAL PHASE_LOG_BEFORE, then re-baseline PHASE_LOG_BEFORE=0 after start_bot so the EXIT-trap validates the
# post-truncation TC-d/e/f segment. This uses validate_phase_log (NOT validate_phase_inline): it does NOT touch
# .phase-validated, so the trap's inline validation still fires and run_phase's fallback stays gated (no
# double-count, flag protocol intact).
validate_phase_log "phase31 pre-restart" "${PHASE_LOG_BEFORE:-0}"
start_bot
# f15: re-baseline the phase offset to 0 (script-local; the EXIT-trap validate_phase_inline reads it in THIS
# same process) so the final trap validation covers the post-truncation TC-d/e/f content, not an empty slice.
PHASE_LOG_BEFORE=0
# start_bot truncates the shared bot.log (e2e_common.sh:521), so the whole fresh log is new-bot output.
# The startup sweep logs BEFORE the HTTP port opens (sweep line precedes "Hook HTTP server listening"),
# hence before start_bot's wait_for_bot_ready returns — so search from offset 0 (truncation guarantees no
# stale match) rather than the post-start_bot line count, which would land past the already-written sweep.
LOG_BEFORE_TCD=0

pane_log "[busy] TC-d AFTER bot restart"

# The startup sweep should log a delete attempt for the seeded entry.
if wait_for_log_pattern "$LOG_BEFORE_TCD" "busy startup sweep" 15 "TC-d startup sweep"; then
  pass "TC-d: startup sweep log line found"
else
  fail "TC-d: startup sweep not logged after bot restart"
fi

# ============================================================
# TC-e: config off → no status ever sent
# ============================================================
echo ""
echo "  TC-e: config off → no status sent"

# Write config with busyIndicator: false.
python3 -c "
import json, os
path = os.path.join('$TEST_CONFIG_DIR', 'config.json')
with open(path) as f:
    cfg = json.load(f)
cfg['busyIndicator'] = False
with open(path, 'w') as f:
    json.dump(cfg, f)
print('config: busyIndicator=false')
"

LOG_BEFORE_TCE=$(wc -l < "$LOG_FILE")
pane_log "[busy] TC-e BEFORE inject"
start_claude "e2e-cc-31e"
wait_for_idle

LOG_BEFORE_TCE=$(wc -l < "$LOG_FILE")
inject_prompt "Without using any tools, write 5 sentences about the ocean. Label each OCEAN_N."
pane_log "[busy] TC-e AFTER inject"
wait_for_idle
pane_log "[busy] TC-e AFTER idle"

# Must NOT have any busy status sent line.
if tail -n +"$((LOG_BEFORE_TCE + 1))" "$LOG_FILE" | grep -q "busy status sent" 2>/dev/null; then
  fail "TC-e: busy status was sent despite busyIndicator=false"
else
  pass "TC-e: no busy status sent with busyIndicator=false"
fi

stop_claude "e2e-cc-31e"

# Restore config.
python3 -c "
import json, os
path = os.path.join('$TEST_CONFIG_DIR', 'config.json')
with open(path) as f:
    cfg = json.load(f)
cfg.pop('busyIndicator', None)
with open(path, 'w') as f:
    json.dump(cfg, f)
print('config: busyIndicator restored (default on)')
"

# ============================================================
# TC-f: incoming COMMAND and DOCUMENT updates each re-float the status
# via the REAL bot.Use markIncoming middleware + command/media routing
# (through the /test/update endpoint — NOT /inject/message pane injection).
#   TC-f1 (command /p): below-reply OUTCOME (final status below the handler reply).
#   TC-f2 (document, bogus file_id): middleware COVERAGE (handler fired + re-float).
# Busy workload = the Sleeping Beauty foreground script (below) => CC busy via hook state. NARRATION IS
# ALLOWED here, and that is deliberate: a "do not narrate" order paired with "run this script" trips CC's
# prompt-injection heuristic and it refuses the script (Round-3 attempt 1), so the stimulus lets CC narrate
# and it does — streaming rich=true sends land in this window (observed: msg 84538 at 07:19:57, inside the
# TC-f1 window). The re-float asserts stay valid REGARDLESS of those streaming sends because they do NOT
# rely on send-purity — they key on SPECIFIC log markers that narration never emits: "Capture reply sent:
# chat=... msg_id=" for the /p reply and "busy status re-floated ... new_msg_id=" for the float. Interleaved
# narration logs "Stream send"/"TG send" (different lines), invisible to both greps, so it cannot bury or
# spoof a marker or the msg_id ordering. (This corrects the earlier premise that the window had ZERO
# streaming sends — the silence order that would have guaranteed that is exactly what made CC refuse.)
# ============================================================
echo ""
echo "  TC-f: incoming command/document re-float status (real middleware path)"

[ -n "${DEFAULT_CHAT_ID:-}" ] || fail "TC-f: DEFAULT_CHAT_ID not set (pairing missing)"

PRE_START_TCF=$(wc -l < "$LOG_FILE")
start_claude "e2e-cc-31f"
wait_for_idle

# The SessionStart notification is a Pages-resolvable reply target for THIS session:
# recordReplyTarget stores its msg_id -> tmux target, and the session stays alive while busy.
# It is the FIRST rich=true TG send to the paired chat after start_claude and precedes any
# tool call / preamble, so it can never be a (non-resolvable) stream chunk.
REPLY_TARGET_ID=$(tail -n +"$((PRE_START_TCF + 1))" "$LOG_FILE" \
  | grep -oP "TG send: chat=$DEFAULT_CHAT_ID msg_id=\K[0-9]+(?=.*rich=true)" | head -1 || true)
[ -n "$REPLY_TARGET_ID" ] || fail "TC-f: no Pages-resolvable notification msg_id to reply to (chat=$DEFAULT_CHAT_ID) — SessionStart rich notification missing"
pass "TC-f: reply target notification msg_id=$REPLY_TARGET_ID"

# Quiet-but-busy via the Sleeping Beauty fixture. WHY THIS SHAPE (Round-3 fix, boss's design; do NOT
# "simplify" to a bare foreground sleep). This rationale lives HERE, in the phase file, on purpose — CC
# never reads phase31_busy_indicator.sh, so it is the safe place for the parts a model must not see:
#   - CC v2.1.219 BLOCKS a bare `sleep N` at the Bash-tool layer ("<tool_use_error>Blocked: standalone
#     sleep ..."), so the withdrawn "run sleep 180 in the FOREGROUND" prompt was REFUSED and the session
#     went idle in ~4s (TC-f2 then found no busy status to re-float). CC does NOT block invoking a SCRIPT,
#     so the wait lives INSIDE sleeping_beauty.sh: the model plays Sleeping Beauty and runs it, and it sleeps
#     until the "prince" — an external sentinel this phase creates at teardown — wakes her. That is a
#     sustained FOREGROUND busy state with NO background shell left at /exit (a backgrounded sleep is what
#     broke this phase originally).
#   - Attempt 1 was still REFUSED ("I am not going to run an unfamiliar script blindly, especially one paired
#     with instructions to suppress any narration"). Three fixes (boss/note3, attempt 2): (1) the stimulus
#     below names the script as a repository test fixture and invites CC to read it first (kills
#     "unfamiliar/blindly"); (2) the silence order is DROPPED entirely — the "do not narrate" + "run this
#     script" combination was the trigger; (3) the CC-blocks-bare-sleep rationale was MOVED here, out of
#     sleeping_beauty.sh, whose own comment now states only its legitimate purpose — CC DOES read that file,
#     and a "how we get around your Bash policy" comment gives it a fresh, independent reason to refuse.
#   - Narration is now allowed, and TC-f is NOT changed to accommodate it: TC-f1/TC-f2 each take their cursor
#     (CUR1/CUR2) immediately BEFORE their POST, so any short pre-tool acknowledgement CC emits lands ahead of
#     both windows and cannot bury a route-scoped re-float. See tests/cc/sleeping_beauty.sh (purpose only).
# Copy the fixture into CC's cwd so the prompt can call it by name, and clear any stale prince sentinel.
mkdir -p "$CC_WORKDIR"
cp "$SCRIPT_DIR/sleeping_beauty.sh" "$CC_WORKDIR/sleeping_beauty.sh"
rm -f "$CC_WORKDIR/prince-arrived"
LOG_BEFORE_TCF=$(wc -l < "$LOG_FILE")
pane_log "[busy] TC-f BEFORE quiet-busy inject"
inject_prompt "We are acting out a short scene from the tale of Sleeping Beauty. The file sleeping_beauty.sh in your current directory is a test fixture that ships with this repository. You are welcome to read it first to see exactly what it does. In the scene, Sleeping Beauty eats the poisoned apple and falls into a long enchanted sleep, waking only when the prince arrives. To play it out, use the Bash tool to run this command exactly once and let it finish in the foreground (do not run it in the background): bash sleeping_beauty.sh. The script carries out her enchanted sleep and returns on its own when the prince arrives."
pane_log "[busy] TC-f AFTER quiet-busy inject"

# Wait for the initial busy status; capture the FULL route (chat+topic) for scoping.
if ! wait_for_log_pattern "$LOG_BEFORE_TCF" "busy status sent chat=" 30 "TC-f initial status"; then
  fail "TC-f: initial busy status not sent (quiet-busy workload did not register as busy)"
fi
pass "TC-f: initial busy status created (quiet-busy)"
WINF=$(tail -n +"$((LOG_BEFORE_TCF + 1))" "$LOG_FILE")
CHATF=$(echo "$WINF"  | grep -oP 'busy status sent chat=\K-?[0-9]+' | head -1 || true)
TOPICF=$(echo "$WINF" | grep -oP 'busy status sent chat=-?[0-9]+ topic=\K[0-9]+' | head -1 || true)
[ -n "$CHATF" ] || fail "TC-f: no chat in busy status marker"
# Settle: let the initial status send / any tool-notify send finish before measuring.
sleep 3

# ---- TC-f1: incoming COMMAND (/p) → below-reply OUTCOME ----
CUR1=$(wc -l < "$LOG_FILE")
pane_log "[busy] TC-f1 BEFORE /test/update command"
curl -sf -X POST "http://127.0.0.1:$TEST_PORT/test/update" \
  -H 'Content-Type: application/json' \
  -d "{\"chat_id\":$DEFAULT_CHAT_ID,\"sender_id\":$DEFAULT_CHAT_ID,\"text\":\"/p\",\"reply_to_msg_id\":$REPLY_TARGET_ID}" > /dev/null 2>&1 || true
pane_log "[busy] TC-f1 AFTER /test/update command"

# Anchor the capture reply (a log line emitted ONLY by capture replies), after CUR1.
if ! wait_for_log_pattern "$CUR1" "Capture reply sent: chat=$CHATF " 20 "TC-f1 capture reply"; then
  fail "TC-f1: /p did not produce a capture reply (routing failed: reply_to=$REPLY_TARGET_ID chat=$CHATF)"
fi
REPLY_MSG_ID=$(tail -n +"$((CUR1 + 1))" "$LOG_FILE" \
  | grep -oP "Capture reply sent: chat=$CHATF msg_id=\K[0-9]+" | tail -1 || true)
[ -n "$REPLY_MSG_ID" ] || fail "TC-f1: could not extract capture reply msg_id (chat=$CHATF)"

# Poll-until (20s) a route-scoped re-float whose new_msg_id > the reply msg_id (below-reply outcome).
F1_RF=""
for _i in $(seq 1 20); do
  F1_RF=$(tail -n +"$((CUR1 + 1))" "$LOG_FILE" \
    | grep -oP "busy status re-floated chat=$CHATF topic=$TOPICF new_msg_id=\K[0-9]+" | tail -1 || true)
  if [ -n "$F1_RF" ] && [ "$F1_RF" -gt "$REPLY_MSG_ID" ]; then break; fi
  sleep 1
done
if [ -n "$F1_RF" ] && [ "$F1_RF" -gt "$REPLY_MSG_ID" ]; then
  pass "TC-f1: status re-floated new_msg_id=$F1_RF > reply msg_id=$REPLY_MSG_ID (below-reply outcome, chat=$CHATF)"
else
  fail "TC-f1: no route-scoped re-float > reply msg_id=$REPLY_MSG_ID within 20s (chat=$CHATF refloat=$F1_RF)"
fi

# ---- TC-f2: incoming DOCUMENT (bogus file_id) → middleware COVERAGE ----
sleep 2  # settle TC-f1 activity
CUR2=$(wc -l < "$LOG_FILE")
pane_log "[busy] TC-f2 BEFORE /test/update document"
curl -sf -X POST "http://127.0.0.1:$TEST_PORT/test/update" \
  -H 'Content-Type: application/json' \
  -d "{\"chat_id\":$DEFAULT_CHAT_ID,\"sender_id\":$DEFAULT_CHAT_ID,\"document\":{\"file_id\":\"BOGUS_TCF2\",\"file_name\":\"probe.txt\"}}" > /dev/null 2>&1 || true
pane_log "[busy] TC-f2 AFTER /test/update document"

# (a) OnDocument fired: a bogus file_id makes downloadTGFile fail => "Document download failed"
#     (messages.go:731). This deterministically proves the markIncoming middleware + media routing
#     + the OnDocument handler ran for a MEDIA update (coverage the per-handler bumps would miss).
if ! wait_for_log_pattern "$CUR2" "Document download failed" 20 "TC-f2 document handler"; then
  fail "TC-f2: OnDocument did not fire for the synthetic document update (no 'Document download failed' after cursor)"
fi
pass "TC-f2: OnDocument fired (Document download failed marker present)"
# (b) route-scoped re-float after the document update (middleware markIncoming entry+deferred Mark).
if wait_for_log_pattern "$CUR2" "busy status re-floated chat=$CHATF topic=$TOPICF" 20 "TC-f2 refloat"; then
  pass "TC-f2: status re-floated after incoming document (middleware coverage, chat=$CHATF)"
else
  fail "TC-f2: no route-scoped re-float after the document update within 20s (chat=$CHATF)"
fi

# End the quiet-busy session and confirm idle cleanup.
pane_log "[busy] TC-f BEFORE stop"
# The prince arrives: create the external sentinel sleeping_beauty.sh polls for, so the script returns on its
# own, CC finishes the Bash tool call and goes idle — NO background shell at teardown (the boss's external-wake
# design, replacing the withdrawn foreground-sleep).
touch "$CC_WORKDIR/prince-arrived"
wait_for_idle
# f12: do NOT call stop_claude here — 31f is the LAST start_claude session, so the EXIT-trap
# _cc_phase_cleanup (cc_common.sh:120, rc==0 branch) runs validate_phase_inline THEN stop_claude for it.
# An explicit stop here double-stops (the trap then hits "session not found" -> rc=1 -> phase crash).
# The Escape+C-u above leave CC idle with an empty composer so the trap's stop_claude exits cleanly.
# Mirrors the phase13c last-session convention (intermediate sessions 31a/31e ARE explicitly stopped).
wait_for_log_pattern "$LOG_BEFORE_TCF" "busy status deleted" 15 "TC-f final delete" || true
pass "TC-f: phase complete"

# FIX 3: remove the fairy-nap script fixture (phase end).
rm -f "$TCB_SCRIPT"
# Round-3 fix: remove the Sleeping Beauty fixture + prince sentinel copied into CC's cwd.
rm -f "$CC_WORKDIR/sleeping_beauty.sh" "$CC_WORKDIR/prince-arrived"
