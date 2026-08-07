#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Phase 30: SQLite message archive + header message ID ---"

ensure_infrastructure

# m1: the archive is verified via the sqlite3 CLI — fail loudly if it is missing (no silent skip).
command -v sqlite3 >/dev/null || { echo "ERROR: phase30 requires the sqlite3 CLI to verify the archive"; exit 1; }

DB="$TEST_CONFIG_DIR/messages.db"
# sq: a scalar query that must succeed (db + tables exist by this point).
sq() { sqlite3 -batch -cmd ".timeout 5000" "$DB" "$1"; }
# sq0: a scalar query that returns 0 when the db/table is not yet present.
sq0() { sqlite3 -batch -cmd ".timeout 5000" "$DB" "$1" 2>/dev/null || echo 0; }

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# TC3 baseline (round-3 fix #4): capture MAX(op_id) BEFORE start_claude so the SessionStart send row
# (created DURING start_claude) is not excluded by the later "op_id > baseline" filter.
BASE_OP_TC3=$(sq0 "SELECT COALESCE(MAX(op_id),0) FROM operations")
# Change 2 baseline: capture MAX(event_id) BEFORE start_claude so the SessionStart hook_events row counts.
BASE_HOOK=$(sq0 "SELECT COALESCE(MAX(event_id),0) FROM hook_events")
echo "  BASE_OP_TC3 (before start_claude) = $BASE_OP_TC3, BASE_HOOK = $BASE_HOOK"

start_claude "e2e-cc-30"
pane_log "[phase30] after start_claude"
# Let the SessionStart rich notification be sent + archived, and the session→chat mapping settle.
sleep 3

# ---------------------------------------------------------------------------------------------------
# TC30-1 — archive-send (rich): /test/capture_message routes through RetrySendRich; the archive records
# one messages row + one send operation (rich=1, text_len>0, content carrying a unique marker).
# ---------------------------------------------------------------------------------------------------
echo ""
echo "  [TC30-1] archive-send (rich)"
BASE_ID1=$(sq0 "SELECT COALESCE(MAX(id),0) FROM messages")
BASE_OP1=$(sq0 "SELECT COALESCE(MAX(op_id),0) FROM operations")
MARKER="ARCHIVEMARKONE${E2E_RUN_ID}"
CAP_JSON=$(python3 -c "import json; print(json.dumps({'target': '$E2E_PANE', 'content': '$MARKER'}))")
CAP_RESP=$(curl -sf -X POST "http://127.0.0.1:$TEST_PORT/test/capture_message" -H "Content-Type: application/json" -d "$CAP_JSON")
echo "  DEBUG: TC30-1 capture_message resp: $CAP_RESP"
TC1_MSGID=$(echo "$CAP_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('msg_id',''))")
TC1_CHATID=$(echo "$CAP_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('chat_id',''))")
[ -z "$TC1_MSGID" ] && fail "TC30-1: capture_message returned no msg_id (resp=$CAP_RESP)"
[ -z "$TC1_CHATID" ] && fail "TC30-1: capture_message returned no chat_id (resp=$CAP_RESP)"
sleep 1
TC1_ARCH_ID=$(sq "SELECT id FROM messages WHERE tg_msg_id=$TC1_MSGID AND chat_id=$TC1_CHATID AND id>$BASE_ID1")
[ -z "$TC1_ARCH_ID" ] && fail "TC30-1: no new messages row for tg_msg_id=$TC1_MSGID chat=$TC1_CHATID (id>$BASE_ID1)"
pass "TC30-1-1: send created a messages row (tg-cli id=$TC1_ARCH_ID)"
TC1_SEND=$(sq "SELECT rich||'|'||text_len FROM operations WHERE msg_id=$TC1_ARCH_ID AND op='send' AND op_id>$BASE_OP1 LIMIT 1")
echo "  DEBUG: TC30-1 send row rich|text_len = '$TC1_SEND'"
[ -z "$TC1_SEND" ] && fail "TC30-1-2: no send operation row under id=$TC1_ARCH_ID"
TC1_RICH="${TC1_SEND%%|*}"
TC1_LEN="${TC1_SEND##*|}"
[ "$TC1_RICH" = "1" ] || fail "TC30-1-2: send op rich flag = '$TC1_RICH', want 1"
[ -n "$TC1_LEN" ] && [ "$TC1_LEN" -gt 0 ] || fail "TC30-1-3: send op text_len = '$TC1_LEN', want >0"
TC1_MARK=$(sq "SELECT COUNT(*) FROM operations WHERE msg_id=$TC1_ARCH_ID AND op='send' AND content LIKE '%$MARKER%'")
[ "$TC1_MARK" -ge 1 ] || fail "TC30-1-4: send op content does not contain marker $MARKER"
pass "TC30-1: rich send recorded (rich=1, text_len=$TC1_LEN, marker present)"

# ---------------------------------------------------------------------------------------------------
# TC30-2 — edit-history: a settings SEND (RetrySend) then a DIFFERENT-content edit (RetryEdit via
# data=cwd) share ONE tg-cli id. data=cwd (not data=main, which is byte-identical → "message not
# modified" → no recorded edit) guarantees a real edit operation.
# ---------------------------------------------------------------------------------------------------
echo ""
echo "  [TC30-2] edit-history (send + edit share the tg-cli id)"
BASE_ID2=$(sq0 "SELECT COALESCE(MAX(id),0) FROM messages")
SET_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/settings_message?chat_id=$DEFAULT_CHAT_ID")
echo "  DEBUG: TC30-2 settings_message resp: $SET_RESP"
SMID=$(echo "$SET_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('msg_id',''))")
[ -z "$SMID" ] && fail "TC30-2: settings_message returned no msg_id (resp=$SET_RESP)"
sleep 1
EDIT_RESP=$(curl -sf "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$SMID&unique=settings&data=cwd&chat_id=$DEFAULT_CHAT_ID")
echo "  DEBUG: TC30-2 edit(cwd) resp: $EDIT_RESP"
sleep 1
TC2_ARCH_ID=$(sq "SELECT id FROM messages WHERE tg_msg_id=$SMID AND chat_id=$DEFAULT_CHAT_ID AND id>$BASE_ID2")
[ -z "$TC2_ARCH_ID" ] && fail "TC30-2: no new messages row for settings tg_msg_id=$SMID chat=$DEFAULT_CHAT_ID (id>$BASE_ID2)"
TC2_SEND=$(sq "SELECT COUNT(*) FROM operations WHERE msg_id=$TC2_ARCH_ID AND op='send'")
TC2_EDIT=$(sq "SELECT COUNT(*) FROM operations WHERE msg_id=$TC2_ARCH_ID AND op='edit'")
echo "  DEBUG: TC30-2 send_count=$TC2_SEND edit_count=$TC2_EDIT under tg-cli id=$TC2_ARCH_ID"
[ "$TC2_SEND" -ge 1 ] || fail "TC30-2-1: expected >=1 send under id=$TC2_ARCH_ID, got $TC2_SEND"
[ "$TC2_EDIT" -ge 1 ] || fail "TC30-2-2: expected >=1 edit under id=$TC2_ARCH_ID, got $TC2_EDIT (data=cwd must produce a real edit)"
pass "TC30-2: send + edit share tg-cli id=$TC2_ARCH_ID (send=$TC2_SEND edit=$TC2_EDIT, tg_msg_id=$SMID)"

# ---------------------------------------------------------------------------------------------------
# TC30-3 — header message ID: among SEND ops after the pre-start baseline, at least one content has a
# <details> block AND a "🆔 #<id>" line matching that row's OWN tg-cli id. Verified from the archive DB
# (operations.content), NOT bot.log — the loggers finalize via FinalizeRichHTML (id 0) and carry no 🆔
# line (D4). The SessionStart (🟢) rich notification is the deterministic, model-independent source.
# ---------------------------------------------------------------------------------------------------
echo ""
echo "  [TC30-3] header message ID inside the collapsed <details>"
TC3_MATCH=$(sq "SELECT COUNT(*) FROM operations WHERE op='send' AND op_id>$BASE_OP_TC3 AND content LIKE '%<details>%' AND content LIKE ('%🆔 #' || msg_id || '%')")
echo "  DEBUG: TC30-3 matching send rows = $TC3_MATCH (base_op=$BASE_OP_TC3)"
if [ -z "$TC3_MATCH" ] || [ "$TC3_MATCH" -lt 1 ]; then
  echo "  DEBUG: recent send contents (msg_id, first 200 chars):"
  sq "SELECT msg_id, substr(content,1,200) FROM operations WHERE op='send' AND op_id>$BASE_OP_TC3 ORDER BY op_id DESC LIMIT 5" || true
  pane_log "[TC30-3] FAIL pane"
  fail "TC30-3: no SEND with a <details> header carrying its own 🆔 #<id> (base_op=$BASE_OP_TC3)"
fi
pass "TC30-3: collapsed header carries the tg-cli message ID (🆔 #<id> inside <details>)"

# ---------------------------------------------------------------------------------------------------
# TC30-4 — Change 1: no <br> around details boundaries in the actually-sent HTML. Verified from
# operations.content (the exact sent bytes). A details-bearing SEND (the SessionStart header) must have
# NO "<br><details", NO "</details><br>", NO "</summary><br>" — but KEEP a "<br></details>".
# ---------------------------------------------------------------------------------------------------
echo ""
echo "  [TC30-4] no <br> around details in sent HTML"
TC4_DETAILS=$(sq "SELECT COUNT(*) FROM operations WHERE op='send' AND op_id>$BASE_OP_TC3 AND content LIKE '%<details>%'")
[ "$TC4_DETAILS" -ge 1 ] || fail "TC30-4: no details-bearing SEND found after baseline (check would be vacuous)"
TC4_BAD=$(sq "SELECT COUNT(*) FROM operations WHERE op='send' AND op_id>$BASE_OP_TC3 AND content LIKE '%<details>%' AND (content LIKE '%<br><details%' OR content LIKE '%</details><br>%' OR content LIKE '%</summary><br>%')")
echo "  DEBUG: TC30-4 details_sends=$TC4_DETAILS bad_adjacency_sends=$TC4_BAD"
if [ -z "$TC4_BAD" ] || [ "$TC4_BAD" -ne 0 ]; then
  echo "  DEBUG: offending send contents (first 300 chars):"
  sq "SELECT substr(content,1,300) FROM operations WHERE op='send' AND op_id>$BASE_OP_TC3 AND content LIKE '%<details>%' AND (content LIKE '%<br><details%' OR content LIKE '%</details><br>%' OR content LIKE '%</summary><br>%') LIMIT 3" || true
  fail "TC30-4: $TC4_BAD sent details-message(s) still have a <br> adjacent to a details/summary boundary"
fi
# The kept case: at least one send retains "<br></details>" (br before a closing details).
TC4_KEPT=$(sq "SELECT COUNT(*) FROM operations WHERE op='send' AND op_id>$BASE_OP_TC3 AND content LIKE '%<br></details>%'")
[ "$TC4_KEPT" -ge 1 ] || fail "TC30-4: expected a kept '<br></details>' in a sent details message, found none"
pass "TC30-4: sent HTML has no <br> before/after details or after summary (kept <br></details>)"

# ---------------------------------------------------------------------------------------------------
# TC30-5 — Change 2: hook events are archived. After start_claude the SessionStart hook is recorded in
# hook_events with a valid JSON payload (model-independent).
# ---------------------------------------------------------------------------------------------------
echo ""
echo "  [TC30-5] hook events archived"
TC5_COUNT=$(sq "SELECT COUNT(*) FROM hook_events WHERE event_id>$BASE_HOOK AND hook_event_name='SessionStart'")
echo "  DEBUG: TC30-5 new SessionStart hook_events = $TC5_COUNT (base_hook=$BASE_HOOK)"
[ "$TC5_COUNT" -ge 1 ] || fail "TC30-5: no SessionStart row in hook_events after start_claude (base_hook=$BASE_HOOK)"
TC5_PAYLOAD=$(sq "SELECT payload FROM hook_events WHERE event_id>$BASE_HOOK AND hook_event_name='SessionStart' ORDER BY event_id LIMIT 1")
echo "$TC5_PAYLOAD" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null || fail "TC30-5: SessionStart payload is not valid JSON: ${TC5_PAYLOAD:0:200}"
pass "TC30-5: SessionStart hook recorded in hook_events with valid JSON payload"

# ---------------------------------------------------------------------------------------------------
# TC30-6 — Change 2 join: an assistant message (streamed) links hook_events to its messages row via the
# CC message_id. cc_message_id is populated ONLY by the incremental Stream-send path; a reply short enough
# to burst coincident with Stop is delivered via Stop direct-send and records NO cc_message_id. f21: elicit
# a substantially LONG multi-line reply so generation spans multiple stream throttle windows and streams
# incrementally, then assert a messages row with a non-empty cc_message_id joins to a hook_events row on
# cc_message_id = message_id.
# ---------------------------------------------------------------------------------------------------
echo ""
echo "  [TC30-6] hook<->message join via cc_message_id"
BASE_ID_JOIN=$(sq "SELECT COALESCE(MAX(id),0) FROM messages")
inject_prompt "List ten programming languages, one per line, each followed by a colon and a short one-sentence description. No other text."
wait_for_idle
sleep 3
TC6_CC=$(sq "SELECT COUNT(*) FROM messages WHERE id>$BASE_ID_JOIN AND cc_message_id != ''")
echo "  DEBUG: TC30-6 new messages with cc_message_id = $TC6_CC"
[ "$TC6_CC" -ge 1 ] || fail "TC30-6: no streamed messages row carried a cc_message_id after the prompt (stream send wiring)"
TC6_JOIN=$(sq "SELECT COUNT(*) FROM messages m JOIN hook_events h ON m.cc_message_id = h.message_id WHERE m.id>$BASE_ID_JOIN AND m.cc_message_id != ''")
echo "  DEBUG: TC30-6 join rows (messages.cc_message_id = hook_events.message_id) = $TC6_JOIN"
[ "$TC6_JOIN" -ge 1 ] || fail "TC30-6: a streamed message's cc_message_id did not join to any hook_events.message_id"
pass "TC30-6: streamed assistant message links to its hook events via cc_message_id (join rows=$TC6_JOIN)"

echo ""
echo "  Phase 30 complete."
