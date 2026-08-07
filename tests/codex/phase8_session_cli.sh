#!/bin/bash
# Phase 8: Session CLI commands + session log transcript tests (self-contained per-phase session)
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

ensure_infrastructure
start_codex "e2e-codex-8"

echo ""
echo "--- Session CLI commands test ---"
pane_log "[session_cli] BEFORE test"

# Get session ID from API (always use current session, not stale file)
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
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=e2e-cli" > /dev/null 2>&1 || true
  echo "  Named session $SESSION_ID as e2e-cli"
fi

# Test session list — verify it contains the active session
LIST_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session list --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: LIST_OUTPUT (${#LIST_OUTPUT} chars): $LIST_OUTPUT"
set +eo pipefail
echo "$LIST_OUTPUT" | grep -q "e2e-cli"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session list: contains agent name 'e2e-cli'"
else
  fail "session list: agent name not found: $LIST_OUTPUT"
fi
set +eo pipefail
echo "$LIST_OUTPUT" | grep -q "target=\|%"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session list: shows tmux target"
else
  fail "session list: tmux target not found: $LIST_OUTPUT"
fi

# Test session send — inject and verify in bot log (API-based, works for both CC and Codex)
LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
pane_log "[session_cli] BEFORE session send"
./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-cli --port "$TEST_PORT" --from e2e-test --text "Reply with exactly: e2e_session_send_test_marker. Do not run any tools or commands." > /dev/null 2>&1 || true
sleep 2
pane_log "[session_cli] AFTER session send"
set +eo pipefail
tail -n +$((LOG_BEFORE+1)) "$LOG_FILE" | grep -q "e2e_session_send_test_marker"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session send: message injected and logged"
else
  fail "session send: injection not found in bot log"
fi
# Check TG notification for session send
set +eo pipefail
tail -n +$((LOG_BEFORE+1)) "$LOG_FILE" | grep -q "Session send notification\|CLI Message"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session send: TG notification sent"
else
  fail "session send: TG notification not found in log"
fi

pane_log "[session_cli] AFTER CLI tests"
echo "  Session CLI tests complete."

echo ""
echo "--- Session log transcript tests ---"
pane_log "[session_log] BEFORE transcript tests"

# Test session log — verify actual content from this phase's own session
wait_for_idle $TIMEOUT
LOG_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 20 2>&1) || true

# Should have header with tmux target (📟)
echo "  DEBUG: LOG_OUTPUT (${#LOG_OUTPUT} chars): $LOG_OUTPUT"
set +eo pipefail
echo "$LOG_OUTPUT" | grep -q "📟"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session log: header contains tmux target (📟)"
else
  fail "session log: missing tmux target header: ${LOG_OUTPUT%%$'\n'*}"
fi

# Should have separator lines
set +eo pipefail
echo "$LOG_OUTPUT" | grep -q "────────────────────────"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session log: has separator lines between messages"
else
  fail "session log: missing separator lines"
fi

# Should have timestamps
set +eo pipefail
echo "$LOG_OUTPUT" | grep -qE "[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session log: has timestamp format"
else
  fail "session log: missing timestamps"
fi

# Test --no-tools filter (Codex tool entries may use different labels; basic filter check)
NOTOOLS_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 20 --no-tools 2>&1) || true
echo "  DEBUG: NOTOOLS_OUTPUT (${#NOTOOLS_OUTPUT} chars): $NOTOOLS_OUTPUT"
set +eo pipefail
echo "$NOTOOLS_OUTPUT" | grep -q "\[Bash\]"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  fail "session log --no-tools: still contains [Bash] entries"
else
  pass "session log --no-tools: Bash entries filtered out"
fi

# Test --format json
JSON_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 3 --format json 2>&1) || true
echo "  DEBUG: JSON_OUTPUT (${#JSON_OUTPUT} chars): $JSON_OUTPUT"
if echo "$JSON_OUTPUT" | python3 -c "import sys,json; d=json.load(sys.stdin); assert 'target' in d and 'messages' in d" 2>/dev/null; then
  pass "session log --format json: valid JSON with target and messages"
else
  fail "session log --format json: invalid JSON structure: ${JSON_OUTPUT%%$'\n'*}"
fi

# Verify session log contains known content (after injecting marker)
LOG_FULL=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 9999 2>&1) || true
echo "  DEBUG: LOG_FULL (${#LOG_FULL} chars): $LOG_FULL"
set +eo pipefail
echo "$LOG_FULL" | grep -q "e2e_session_send_test_marker"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session log: contains known content from transcript"
else
  fail "session log: no known content found in transcript"
fi

# Test session log content accuracy — --lines 3 returns exactly 3 messages
JSON_3=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-cli --port "$TEST_PORT" --lines 3 --format json 2>&1)

MSG_COUNT=$(echo "$JSON_3" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('messages',[])))" 2>/dev/null || echo "0")
if [ "$MSG_COUNT" = "3" ]; then
  pass "session log accuracy: --lines 3 returned exactly 3 messages"
else
  fail "session log accuracy: --lines 3 returned $MSG_COUNT messages (expected 3)"
fi

# Verify messages have valid types (Codex may use response_item or assistant/user)
MSG_TYPES=$(echo "$JSON_3" | python3 -c "
import sys,json
d=json.load(sys.stdin)
msgs=d.get('messages',[])
for m in msgs:
    print(m.get('type','unknown'))
" 2>/dev/null || echo "")
echo "  DEBUG: MSG_TYPES (${#MSG_TYPES} chars): $MSG_TYPES"
set +eo pipefail
echo "$MSG_TYPES" | grep -q "user\|assistant\|response_item"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "session log accuracy: messages have valid types"
else
  fail "session log accuracy: messages missing valid types: $MSG_TYPES"
fi

pane_log "[session_log] AFTER transcript tests"
echo "  Session log tests complete."

echo ""
echo "--- Codex slash-command inject confirmation (/compact) test [f29 C] ---"
# f29 C: a codex LOCAL slash-command (e.g. /compact) emits NO UserPromptSubmit and the composer is already
# cleared by the time a post-hoc CapturePane runs, so the OLD confirmation path yielded a FALSE
# "inject not confirmed" even though the command executed. The C transaction OWNS the Enter (clear+paste
# WITHOUT Enter -> compose-confirm under the inject lock -> submit) and then confirms via the codex Working
# indicator (busy pane-title spinner). This case sends a REAL /compact via `session send --no-header` and
# asserts the bot log records the inject as CONFIRMED (never "not confirmed"), and that the send carries NO
# delivery-status annotation (delivery_status="" -> DeliveryStatusTag renders nothing).
#
# Placed LAST in the phase on purpose: after Enter, /compact drives the codex session busy for a while
# (that busy IS the Working confirm), so nothing must run after it — later cases would race the compact.
#
# --no-header is REQUIRED: the injected text must be the raw "/compact" (leading "/") for isCodexSlash to
# fire the C transaction; the default header ("--- 💬 Message from agent [..] ---\n/compact") would put
# "---" first and defeat the leading-"/" scope check, routing the send through the normal UPS path instead.

# Ensure the session is IDLE before we send — the Working-confirm poll would otherwise latch onto a
# pre-existing busy state and confirm falsely. The prior session-log tests are read-only, so this settles fast.
wait_for_idle "$TIMEOUT" "$E2E_PANE"
SLASH_LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
pane_log "[codex_slash] BEFORE /compact send"
# session send blocks server-side through phase1+phase2 (SafeInjectText is synchronous in the handler), so by
# the time this returns the bot log already holds the confirm line + the "Session send via API:" line.
SLASH_SEND_OUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session send --name e2e-cli --port "$TEST_PORT" --from e2e-test --no-header --text "/compact" 2>&1) || true
echo "  DEBUG: SLASH_SEND_OUT (${#SLASH_SEND_OUT} chars): $SLASH_SEND_OUT"
sleep 2
pane_log "[codex_slash] AFTER /compact send"

SLASH_SLICE=$(tail -n +$((SLASH_LOG_BEFORE+1)) "$LOG_FILE" 2>/dev/null || true)

# (a) The inject is CONFIRMED: the C phase2 logged "codex slash inject confirmed (Working)".
set +eo pipefail
echo "$SLASH_SLICE" | grep -q "codex slash inject confirmed"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "codex slash: /compact inject confirmed (Working) in bot log"
else
  fail "codex slash: /compact inject NOT confirmed — expected 'codex slash inject confirmed' in log slice"
fi

# (a') And NOT a false-negative: neither the compose-veto nor the Working-timeout 'not confirmed' line appears.
set +eo pipefail
echo "$SLASH_SLICE" | grep -qE "codex slash inject not confirmed|codex slash compose not confirmed"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -ne 0 ]; then
  pass "codex slash: no 'not confirmed' inject log for /compact"
else
  fail "codex slash: found a 'not confirmed' inject log for /compact (false negative)"
fi

# (b) The TG notification for this send carries NO delivery-status annotation: the API log line for THIS
# send (matched by injectText="/compact") records delivery_status="" — the exact value fed to
# DeliveryStatusTag, which renders nothing for "".
SLASH_API_LINE=$(echo "$SLASH_SLICE" | grep "Session send via API:" | grep 'injectText="/compact"' || true)
echo "  DEBUG: SLASH_API_LINE: $SLASH_API_LINE"
set +eo pipefail
echo "$SLASH_API_LINE" | grep -q 'delivery_status=""'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "codex slash: /compact send carries NO delivery-status annotation (delivery_status=\"\")"
else
  fail "codex slash: /compact send has a delivery-status annotation (expected delivery_status=\"\"): $SLASH_API_LINE"
fi

# (b') CLI side of the same guarantee: a confirmed send prints the plain "Message sent" with no delivery Warning.
set +eo pipefail
echo "$SLASH_SEND_OUT" | grep -q "Message sent to e2e-cli"
_ps_msg=("${PIPESTATUS[@]}")
echo "$SLASH_SEND_OUT" | grep -qiE "delivery unconfirmed|submit failed|Warning:"
_ps_warn=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps_msg[1]}" -eq 0 ] && [ "${_ps_warn[1]}" -ne 0 ]; then
  pass "codex slash: CLI reports plain 'Message sent' with no delivery warning"
else
  fail "codex slash: CLI output missing plain send or shows a delivery warning: $SLASH_SEND_OUT"
fi

pane_log "[codex_slash] AFTER assertions"
echo "  Codex slash-command inject confirmation test complete."
