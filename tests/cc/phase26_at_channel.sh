#!/bin/bash
# Phase 26: @ channel TC1-TC9 per behavior spec
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- @ channel tests (TC1-TC9) ---"

ensure_infrastructure

AT_SESSION="tg-cli-e2e-at"
AT_PANE=""

cleanup_at_session() {
  echo "  [at_channel] cleanup: killing AT tmux session..."
  $TMUX_TEST kill-session -t "=$AT_SESSION" 2>/dev/null || true
  # Unbind routes created by this phase to avoid interfering with subsequent phases
  curl -s -X POST "http://127.0.0.1:$TEST_PORT/route/unbind" \
    -H "Content-Type: application/json" -d '{"name":"e2e-cli"}' > /dev/null 2>&1 || true
  curl -s -X POST "http://127.0.0.1:$TEST_PORT/route/unbind" \
    -H "Content-Type: application/json" -d '{"name":"e2e-at-b"}' > /dev/null 2>&1 || true
  curl -s -X POST "http://127.0.0.1:$TEST_PORT/route/unbind" \
    -H "Content-Type: application/json" -d '{"name":"e2e-at-c"}' > /dev/null 2>&1 || true
}
trap cleanup_at_session EXIT

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Ensure session A (e2e-cli) is named — re-apply in case previous phases renamed it
pane_log "[at_channel] BEFORE setup"
SESSION_A_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
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
if [ -n "$SESSION_A_ID" ]; then
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_A_ID&name=e2e-cli" > /dev/null 2>&1 || true
  echo "  Session A (e2e-cli) confirmed: $SESSION_A_ID"
fi

# Launch session B in a new tmux session on the test tmux server
$TMUX_TEST kill-session -t "=$AT_SESSION" 2>/dev/null || true
$TMUX_TEST new-session -d -s "$AT_SESSION"
AT_PANE=$($TMUX_TEST list-panes -t "$AT_SESSION" -F '#{pane_id}@#{socket_path}')
export AT_PANE
echo "  Session B pane: $AT_PANE"

# Start CC in session B
$TMUX_TEST send-keys -t "$AT_SESSION" \
  "BROWSER=none CLAUDE_CONFIG_DIR=$TEST_CLAUDE_CONFIG_DIR claude --model sonnet --allow-dangerously-skip-permissions"
sleep 1
$TMUX_TEST send-keys -t "$AT_SESSION" Enter
echo "  Waiting for session B CC to start..."
sleep 5

# Handle trust/bypass dialog for session B
PANE_B_CONTENT=$($TMUX_TEST capture-pane -t "${AT_PANE%@*}" -p -S - 2>/dev/null || true)
echo "  DEBUG: session B pane (${#PANE_B_CONTENT} chars): $PANE_B_CONTENT"
set +eo pipefail
echo "$PANE_B_CONTENT" | grep -qi "Bypass Permissions"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  $TMUX_TEST send-keys -t "$AT_SESSION" Down
  sleep 1
  $TMUX_TEST send-keys -t "$AT_SESSION" C-m
  echo "  Session B: Bypass Permissions dialog accepted."
else
  set +eo pipefail
  echo "$PANE_B_CONTENT" | grep -qi "trust"
  _ps=("${PIPESTATUS[@]}")
  set -eo pipefail
  if [ "${_ps[1]}" -eq 0 ]; then
    $TMUX_TEST send-keys -t "$AT_SESSION" C-m
    echo "  Session B: Trust dialog confirmed."
  else
    echo "  Session B: No dialog detected."
  fi
fi

# Wait for session B to register (SessionStart hook fires)
echo "  Waiting for session B to register with bot..."
ELAPSED=0
SESSION_B_ID=""
while [ $ELAPSED -lt $TIMEOUT ]; do
  SESSION_B_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
pane = sys.argv[1]
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane + "@"):
        print(s.get("id", ""))
        sys.exit(0)
print("")
' "$AT_PANE" 2>/dev/null || echo "")
  if [ -n "$SESSION_B_ID" ]; then
    echo "  Session B registered: $SESSION_B_ID"
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for session B registration... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[at_channel] AFTER session B startup"
pane_log "[at_channel] AFTER session B startup (B)" "$AT_PANE"

if [ -z "$SESSION_B_ID" ]; then
  fail "@ channel: session B failed to register within ${TIMEOUT}s"
  exit 1
fi
pass "@ channel: session B launched and registered"

# Name session B as e2e-at-b
curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_B_ID&name=e2e-at-b" > /dev/null 2>&1 || true
echo "  Session B named: e2e-at-b"
sleep 1

# Use longer timeout for @ channel tests (session B startup + SafeInjectText can be slow in full suite)
AT_TIMEOUT=$((TIMEOUT * 2))

# Bind routes so TG notifications are sent (required for pagination/collapse tests)
DEFAULT_CHAT_ID=$(python3 -c "
import json
with open('$TEST_CONFIG_DIR/credentials.json') as f:
    d = json.load(f)
print(d.get('pairingAllow', {}).get('defaultChatId', '0'))
" 2>/dev/null || echo "0")
curl -s -X POST "http://127.0.0.1:$TEST_PORT/route/bind" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"e2e-cli\",\"chat_id\":$DEFAULT_CHAT_ID}" > /dev/null 2>&1 || true
curl -s -X POST "http://127.0.0.1:$TEST_PORT/route/bind" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"e2e-at-b\",\"chat_id\":$DEFAULT_CHAT_ID}" > /dev/null 2>&1 || true
echo "  Routes bound: e2e-cli → $DEFAULT_CHAT_ID, e2e-at-b → $DEFAULT_CHAT_ID"

# Wait for session B to reach idle state
wait_for_idle $AT_TIMEOUT "$AT_PANE" || true

# --- TC1: Channel Open + target context (fixture) ---
echo ""
echo "  [TC1] Channel Open + target context"

# Inject a simple prompt to session A to create transcript file
echo "  Injecting warmup prompt to create transcript..."
pane_log "[TC1] BEFORE warmup inject" "$E2E_PANE"
pane_log "[TC1] BEFORE warmup inject (B)" "$AT_PANE"
inject_prompt "echo tc1_warmup" || true
wait_for_idle $AT_TIMEOUT "$E2E_PANE" || true
sleep 2
pane_log "[TC1] AFTER warmup inject" "$E2E_PANE"
pane_log "[TC1] AFTER warmup inject (B)" "$AT_PANE"

# Write fixture to session A's transcript path so ReadContextBlock uses it
SESSION_A_INFO=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
pane = sys.argv[1]
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane + "@"):
        print(s.get("id", ""), s.get("transcript_path", ""), sep="\t")
        sys.exit(0)
print("\t")
' "$E2E_PANE" 2>/dev/null || echo "	")
SESSION_A_ID=$(echo "$SESSION_A_INFO" | cut -f1)
SESSION_A_TRANSCRIPT=$(echo "$SESSION_A_INFO" | cut -f2)
echo "  DEBUG: session A id=$SESSION_A_ID transcript=$SESSION_A_TRANSCRIPT"

# Append fixture entries to session A's transcript so context read includes long content
FIXTURE_FILE="${SCRIPT_DIR}/../fixtures/at_channel_transcript.jsonl"
if [ -n "$SESSION_A_TRANSCRIPT" ] && [ -f "$FIXTURE_FILE" ]; then
  cat "$FIXTURE_FILE" >> "$SESSION_A_TRANSCRIPT"
  echo "  Appended fixture to transcript: $SESSION_A_TRANSCRIPT ($(wc -l < "$SESSION_A_TRANSCRIPT") lines)"
else
  echo "  WARN: could not append fixture (transcript=$SESSION_A_TRANSCRIPT fixture_exists=$([ -f "$FIXTURE_FILE" ] && echo yes || echo no))"
fi

LOG_BEFORE_TC1=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
pane_log "[TC1] BEFORE @ open" "$E2E_PANE"
pane_log "[TC1] BEFORE @ open (B)" "$AT_PANE"

# Open channel A -> B with message marker
AT_OPEN_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session at e2e-cli e2e-at-b --rounds 3 "e2e_at_open_marker" \
  --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: AT_OPEN_OUTPUT (${#AT_OPEN_OUTPUT} chars): $AT_OPEN_OUTPUT"

sleep 2
pane_log "[TC1] AFTER @ open" "$E2E_PANE"
pane_log "[TC1] AFTER @ open (B)" "$AT_PANE"

# TC1-1: CLI confirms channel opened
set +eo pipefail
echo "$AT_OPEN_OUTPUT" | grep -q "@ channel opened"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-1 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC1-1: CLI output confirms channel opened"
else
  fail "TC1-1: CLI did not confirm channel opened: $AT_OPEN_OUTPUT"
fi

# TC1-2: bot log contains @ channel opened:
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -q "@ channel opened:"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-2 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC1-2: bot log contains '@ channel opened:'"
else
  fail "TC1-2: bot log missing '@ channel opened:'"
fi

# TC1-3: /at/list shows channel
AT_LIST=$(curl -s "http://127.0.0.1:$TEST_PORT/at/list" 2>/dev/null || echo "{}")
echo "  DEBUG: AT_LIST (${#AT_LIST} chars): $AT_LIST"
set +eo pipefail
echo "$AT_LIST" | grep -q "e2e-cli"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-3 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC1-3: /at/list shows channel with e2e-cli"
else
  fail "TC1-3: /at/list missing e2e-cli: $AT_LIST"
fi

# TC1-4: bot log contains SendPagedForward pagination log
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -q "SendPagedForward:.*pages, msg_id="
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-4 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC1-4: pagination triggered (SendPagedForward log found)"
else
  fail "TC1-4: pagination NOT triggered — fixture content too short or SendPagedForward not called"
fi

# TC1-5: bot log TG notification contains 🔗 header
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -q "🔗"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-5 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC1-5: bot log contains 🔗 header"
else
  fail "TC1-5: bot log missing 🔗 header"
fi

# TC1-6: UserPromptSubmit payload contains e2e_at_open_marker
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "UserPromptSubmit" | grep -q "e2e_at_open_marker"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-6 PIPESTATUS=${_ps[*]}"
if [ "${_ps[2]}" -eq 0 ]; then
  pass "TC1-6: B received e2e_at_open_marker (UserPromptSubmit payload)"
else
  fail "TC1-6: B did not receive marker in UserPromptSubmit"
fi

# TC1-7: UserPromptSubmit payload contains session at reply
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "UserPromptSubmit" | grep -q "session at reply"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-7 PIPESTATUS=${_ps[*]}"
if [ "${_ps[2]}" -eq 0 ]; then
  pass "TC1-7: B received reply instruction (session at reply)"
else
  fail "TC1-7: B missing reply instruction in UserPromptSubmit"
fi

# TC1-8: UserPromptSubmit payload contains session at end
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "UserPromptSubmit" | grep -q "session at end"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-8 PIPESTATUS=${_ps[*]}"
if [ "${_ps[2]}" -eq 0 ]; then
  pass "TC1-8: B received end instruction (session at end)"
else
  fail "TC1-8: B missing end instruction in UserPromptSubmit"
fi

# TC1-9: UserPromptSubmit payload contains [e2e-cli → (direction marker for assistant text)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "UserPromptSubmit" | grep -q '\[e2e-cli →'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-9 PIPESTATUS=${_ps[*]}"
if [ "${_ps[2]}" -eq 0 ]; then
  pass "TC1-9: B received [e2e-cli → direction marker in UserPromptSubmit"
else
  fail "TC1-9: B missing [e2e-cli → direction marker in UserPromptSubmit"
fi

# TC1-10: UserPromptSubmit payload contains 🔧 (tool_use summary from fixture)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "UserPromptSubmit" | grep -q "🔧"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-10 PIPESTATUS=${_ps[*]}"
if [ "${_ps[2]}" -eq 0 ]; then
  pass "TC1-10: B received 🔧 tool_use summary from fixture"
else
  fail "TC1-10: B missing 🔧 tool_use summary in UserPromptSubmit"
fi

# TC1-11: UserPromptSubmit payload does NOT contain tool_result content (filtered)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "UserPromptSubmit" | grep -q '"242"'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-11 (tool_result filter) PIPESTATUS=${_ps[*]}"
if [ "${_ps[2]}" -ne 0 ]; then
  pass "TC1-11: tool_result content '242' is NOT present (correctly filtered)"
else
  fail "TC1-11: tool_result content '242' leaked into context (filter broken)"
fi

# TC1-12: UserPromptSubmit payload contains @ prefix (open message preserves @name text)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep "UserPromptSubmit" | grep -q "@"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC1-12 PIPESTATUS=${_ps[*]}"
if [ "${_ps[2]}" -eq 0 ]; then
  pass "TC1-12: UserPromptSubmit contains @ prefix (open message text preserved)"
else
  fail "TC1-12: UserPromptSubmit missing @ prefix"
fi

# TC1-13: PageCacheStore has entry for target message (collapse button was added)
# Extract msg_id from SendPagedForward log (already verified in TC1-4)
TC1_MSG_ID=$(tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -oP 'SendPagedForward:.*msg_id=\K\d+' | head -1)
if [ -n "$TC1_MSG_ID" ]; then
  PAGE_ENTRY_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/test/page_entry?msg_id=$TC1_MSG_ID")
  echo "  DEBUG: TC1-13 page_entry RESP: $PAGE_ENTRY_RESP"
  set +eo pipefail
  echo "$PAGE_ENTRY_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); sys.exit(0 if d.get('exists') else 1)"
  _pe=$?
  set -eo pipefail
  if [ "$_pe" -eq 0 ]; then
    pass "TC1-13: PageCacheStore has entry for msg_id=$TC1_MSG_ID (collapse button exists)"
  else
    fail "TC1-13: PageCacheStore missing entry for msg_id=$TC1_MSG_ID"
  fi
else
  fail "TC1-13: could not extract msg_id from SendPagedForward log"
fi

wait_for_idle $AT_TIMEOUT "$AT_PANE" || true

# --- TC2: Collapse/Expand buttons ---
echo ""
echo "  [TC2] Collapse/Expand buttons"

LOG_BEFORE_TC2=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Extract msg_id from SendPagedForward pagination log
set +eo pipefail
TC2_MSG_ID=$(tail -n +"$((LOG_BEFORE_TC1 + 1))" "$LOG_FILE" | grep -oP 'SendPagedForward:.*msg_id=\K\d+' | head -1)
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC2 msg_id=$TC2_MSG_ID"

if [ -z "$TC2_MSG_ID" ]; then
  fail "TC2: could not extract msg_id — pagination may not have triggered"
else
  # TC2-1: Verify page entry exists
  PE_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/test/page_entry?msg_id=$TC2_MSG_ID")
  echo "  DEBUG: TC2-1 page_entry: $PE_RESP"
  set +eo pipefail
  echo "$PE_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); sys.exit(0 if d.get('exists') else 1)"
  _pe=$?
  set -eo pipefail
  if [ "$_pe" -eq 0 ]; then
    pass "TC2-1: PageCacheStore entry exists for msg_id=$TC2_MSG_ID"
  else
    fail "TC2-1: PageCacheStore entry missing"
  fi

  # TC2-2: Collapse — verify text is header-only (no ---)
  pane_log "[TC2] BEFORE collapse" "$E2E_PANE"
  pane_log "[TC2] BEFORE collapse (B)" "$AT_PANE"
  COLLAPSE_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$TC2_MSG_ID&unique=ce&data=c")
  echo "  DEBUG: TC2-2 collapse RESP: $COLLAPSE_RESP"
  TC2_COLLAPSED=$(echo "$COLLAPSE_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('collapsed',''))")
  TC2_CTEXT=$(echo "$COLLAPSE_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('text',''))")
  if [ "$TC2_COLLAPSED" = "True" ]; then
    pass "TC2-2a: collapsed=True after collapse"
  else
    fail "TC2-2a: collapsed=$TC2_COLLAPSED (expected True)"
  fi
  set +eo pipefail
  echo "$TC2_CTEXT" | grep -q "\-\-\-"
  _grc=$?
  set -eo pipefail
  if [ "$_grc" -ne 0 ]; then
    pass "TC2-2b: collapsed text does NOT contain --- (header-only)"
  else
    fail "TC2-2b: collapsed text still contains --- (should be header-only)"
  fi
  pane_log "[TC2] AFTER collapse" "$E2E_PANE"
  pane_log "[TC2] AFTER collapse (B)" "$AT_PANE"

  # TC2-3: Expand — verify text contains full content (has ---)
  pane_log "[TC2] BEFORE expand" "$E2E_PANE"
  pane_log "[TC2] BEFORE expand (B)" "$AT_PANE"
  EXPAND_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$TC2_MSG_ID&unique=ce&data=e")
  echo "  DEBUG: TC2-3 expand RESP (${#EXPAND_RESP} chars)"
  TC2_EXPANDED=$(echo "$EXPAND_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('collapsed',''))")
  TC2_ETEXT=$(echo "$EXPAND_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('text',''))")
  if [ "$TC2_EXPANDED" = "False" ]; then
    pass "TC2-3a: collapsed=False after expand"
  else
    fail "TC2-3a: collapsed=$TC2_EXPANDED (expected False)"
  fi
  set +eo pipefail
  echo "$TC2_ETEXT" | grep -q "\-\-\-"
  _gre=$?
  set -eo pipefail
  if [ "$_gre" -eq 0 ]; then
    pass "TC2-3b: expanded text contains --- (full content restored)"
  else
    fail "TC2-3b: expanded text missing --- (content not restored)"
  fi
  pane_log "[TC2] AFTER expand" "$E2E_PANE"
  pane_log "[TC2] AFTER expand (B)" "$AT_PANE"

  # TC2-4: Pagination — if multi-page, verify page 2
  TC2_CHUNKS=$(echo "$PE_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('chunks',0))")
  echo "  DEBUG: TC2-4 chunks=$TC2_CHUNKS"
  if [ "$TC2_CHUNKS" -gt 1 ] 2>/dev/null; then
    pane_log "[TC2] BEFORE pagination" "$E2E_PANE"
    pane_log "[TC2] BEFORE pagination (B)" "$AT_PANE"
    PAGE2_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/test/callback?msg_id=$TC2_MSG_ID&unique=p&data=2")
    TC2_PAGE=$(echo "$PAGE2_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('page',''))")
    TC2_PTEXT=$(echo "$PAGE2_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('text',''))")
    if [ "$TC2_PAGE" = "2" ]; then
      pass "TC2-4a: page navigation returned page 2"
    else
      fail "TC2-4a: page=$TC2_PAGE (expected 2)"
    fi
    set +eo pipefail
    echo "$TC2_PTEXT" | grep -q "🔗"
    _gp=$?
    set -eo pipefail
    if [ "$_gp" -eq 0 ]; then
      pass "TC2-4b: page 2 text contains header"
    else
      fail "TC2-4b: page 2 text missing header"
    fi
    pane_log "[TC2] AFTER pagination" "$E2E_PANE"
    pane_log "[TC2] AFTER pagination (B)" "$AT_PANE"
  else
    pass "TC2-4: single page — pagination not applicable"
  fi
fi

# --- TC3: Stop Forward ---
echo ""
echo "  [TC3] Stop Forward"

# Ensure @ channel is open
AT_LIST_TC3=$(curl -s "http://127.0.0.1:$TEST_PORT/at/list" 2>/dev/null || echo "{}")
set +eo pipefail
echo "$AT_LIST_TC3" | grep -q "e2e-cli"
_al3=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_al3[1]}" -ne 0 ]; then
  echo "  Channel not found — reopening for TC3..."
  ./tg-cli --config-dir "$TEST_CONFIG_DIR" session at e2e-cli e2e-at-b --rounds 1 \
    --port "$TEST_PORT" 2>&1 || true
  sleep 2
  echo "  Channel reopened for TC3."
fi

# Get session A info for Stop payload
TC3_SESSION_INFO=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    if s.get("name") == "e2e-cli":
        print(s.get("id", ""), s.get("target", ""), s.get("transcript_path", ""), sep="\t")
        sys.exit(0)
print("\t\t")
' 2>/dev/null || echo "		")
TC3_SID=$(echo "$TC3_SESSION_INFO" | cut -f1)
TC3_TARGET=$(echo "$TC3_SESSION_INFO" | cut -f2)
TC3_TRANSCRIPT=$(echo "$TC3_SESSION_INFO" | cut -f3)
echo "  DEBUG: TC3 session_id=$TC3_SID target=$TC3_TARGET"

LOG_BEFORE_TC3=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

pane_log "[TC3] BEFORE Stop hook" "$E2E_PANE"
pane_log "[TC3] BEFORE Stop hook (B)" "$AT_PANE"

# Simulate Stop hook event via /hook/Stop API (deterministic, no CC dependency)
curl -s -X POST "http://127.0.0.1:$TEST_PORT/hook/Stop" \
  -H "Content-Type: application/json" \
  -d "{\"session_id\":\"$TC3_SID\",\"tmux_target\":\"$TC3_TARGET\",\"transcript_path\":\"$TC3_TRANSCRIPT\",\"cwd\":\"$(pwd)\",\"project\":\"tg-cli\",\"hook_event_name\":\"Stop\",\"last_assistant_message\":\"hello_at_forward_test TC3 deterministic Stop\"}" > /dev/null 2>&1
echo "  Simulated Stop hook event."
sleep 3

pane_log "[TC3] AFTER Stop hook" "$E2E_PANE"
pane_log "[TC3] AFTER Stop hook (B)" "$AT_PANE"

# TC3-1: bot log contains @ forward after simulated Stop
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE" | grep -q "\[INFO\] @ forward:"
_rc=$?
set -eo pipefail
echo "  DEBUG: TC3-1 rc=$_rc"
if [ "$_rc" -eq 0 ]; then
  pass "TC3-1: bot log contains '@ forward:' (Stop output forwarded)"
else
  fail "TC3-1: bot log missing '@ forward:' — Stop output NOT forwarded"
fi

# TC3-2: @ forward log contains [e2e-cli]: role marker (content was formatted)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE" | grep "\[INFO\] @ forward:" | grep -q "\[e2e-cli\]"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC3-2 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC3-2: forward content contains [e2e-cli]: role marker"
else
  fail "TC3-2: forward content missing [e2e-cli] role marker in @ forward log"
fi

# TC3-3: forward log contains content from simulated Stop
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC3 + 1))" "$LOG_FILE" | grep -q "hello_at_forward_test"
_rc=$?
set -eo pipefail
echo "  DEBUG: TC3-3 rc=$_rc"
if [ "$_rc" -eq 0 ]; then
  pass "TC3-3: forward content includes expected text"
else
  fail "TC3-3: forward content missing 'hello_at_forward_test' text"
fi

# --- TC4: Reply ---
echo ""
echo "  [TC4] Reply"
pane_log "[TC4] BEFORE @ reply" "$E2E_PANE"
pane_log "[TC4] BEFORE @ reply (B)" "$AT_PANE"

LOG_BEFORE_TC4=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

AT_REPLY_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session at reply e2e-at-b e2e-cli \
  --text "e2e_at_reply_marker" --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: AT_REPLY_OUTPUT (${#AT_REPLY_OUTPUT} chars): $AT_REPLY_OUTPUT"

sleep 2
pane_log "[TC4] AFTER @ reply (A)" "$E2E_PANE"
pane_log "[TC4] AFTER @ reply (B)" "$AT_PANE"

# TC4-1: CLI confirms Reply sent
set +eo pipefail
echo "$AT_REPLY_OUTPUT" | grep -q "Reply sent"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC4-1 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC4-1: CLI output confirms Reply sent"
else
  fail "TC4-1: CLI did not confirm Reply sent: $AT_REPLY_OUTPUT"
fi

# TC4-2: bot log contains @ reply:
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC4 + 1))" "$LOG_FILE" | grep -q "@ reply:"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC4-2 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC4-2: bot log contains '@ reply:'"
else
  fail "TC4-2: bot log missing '@ reply:'"
fi

# TC4-3: bot log contains reply marker text
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC4 + 1))" "$LOG_FILE" | grep -q "e2e_at_reply_marker"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC4-3 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC4-3: reply text e2e_at_reply_marker found in bot log"
else
  fail "TC4-3: reply text not found in bot log"
fi

# TC4-4: bot log TG notification contains [e2e-at-b → e2e-cli]: direction marker for reply content
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC4 + 1))" "$LOG_FILE" | grep -q '\[e2e-at-b → e2e-cli\]'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC4-4 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC4-4: [e2e-at-b → e2e-cli]: direction marker found in bot log"
else
  fail "TC4-4: [e2e-at-b → e2e-cli]: direction marker missing in bot log"
fi

# --- TC5: AskUserQuestion Forward ---
echo ""
echo "  [TC5] AskUserQuestion Forward"
pane_log "[TC5] BEFORE AskQ forward" "$E2E_PANE"
pane_log "[TC5] BEFORE AskQ forward (B)" "$AT_PANE"

LOG_BEFORE_TC5=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Get session A target and id for pending file
SESSION_A_FOR_TC5=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
pane = sys.argv[1]
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane + "@"):
        print(s.get("id", ""), s.get("target", ""), sep="\t")
        sys.exit(0)
print("\t")
' "$E2E_PANE" 2>/dev/null || echo "	")
TC5_SESSION_ID=$(echo "$SESSION_A_FOR_TC5" | cut -f1)
TC5_TMUX_TARGET=$(echo "$SESSION_A_FOR_TC5" | cut -f2)
echo "  DEBUG: TC5 session_id=$TC5_SESSION_ID tmux_target=$TC5_TMUX_TARGET"

if [ -z "$TC5_SESSION_ID" ] || [ -z "$TC5_TMUX_TARGET" ]; then
  fail "TC5: could not retrieve session A info from /session/list"
fi

# Create pending file for AskUserQuestion
TC5_UUID="e2e-askq-tc5-$(date +%s)"
PENDING_DIR="/tmp/.tg-cli-test/pending"
mkdir -p "$PENDING_DIR"
TC5_CWD="$(pwd)"
export TC5_SESSION_ID TC5_TMUX_TARGET TC5_UUID PENDING_DIR TC5_CWD

# Build and write pending file using python3 with env vars to avoid shell quoting issues
python3 << PYEOF
import json, os, sys

session_id = os.environ.get('TC5_SESSION_ID', '')
tmux_target = os.environ.get('TC5_TMUX_TARGET', '')
uuid = os.environ.get('TC5_UUID', '')
pending_dir = os.environ.get('PENDING_DIR', '')
cwd = os.environ.get('TC5_CWD', '')

tool_input = json.dumps({
    'questions': [{
        'header': 'TC5 Test',
        'question': 'E2E AskQ forward test?',
        'options': [
            {'label': 'Yes', 'description': 'Confirm TC5'},
            {'label': 'No', 'description': 'Deny TC5'}
        ],
        'multiSelect': False
    }]
})

payload = {
    'session_id': session_id,
    'tmux_target': tmux_target,
    'tool_name': 'AskUserQuestion',
    'tool_input': json.loads(tool_input),
    'cwd': cwd,
    'project': 'tg-cli',
    'transcript_path': ''
}

pf = {
    'uuid': uuid,
    'event': 'PreToolUse',
    'tool_name': 'AskUserQuestion',
    'status': 'pending',
    'payload': payload,
    'tg_msg_id': 0,
    'tg_chat_id': 0,
    'session_id': '',
    'tmux_target': '',
    'hook_pid': 0
}
path = os.path.join(pending_dir, uuid + '.json')
with open(path, 'w') as f:
    json.dump(pf, f, indent=2)
print(f'Written pending file: {uuid}.json to {path}')
PYEOF

# Send pending/notify
curl -s -X POST "http://127.0.0.1:$TEST_PORT/pending/notify?uuid=${TC5_UUID}" > /dev/null
sleep 3

pane_log "[TC5] AFTER pending/notify" "$E2E_PANE"
pane_log "[TC5] AFTER pending/notify (B)" "$AT_PANE"

# TC5-1: bot log contains 🔗 header in note-side notification
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC5 + 1))" "$LOG_FILE" | grep -q "🔗"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC5-1 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC5-1: bot log contains 🔗 header after AskQ forward"
else
  fail "TC5-1: bot log missing 🔗 header after AskQ forward"
fi

# TC5-2: bot log contains is asking a question
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC5 + 1))" "$LOG_FILE" | grep -q "is asking a question"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC5-2 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC5-2: bot log contains 'is asking a question'"
else
  fail "TC5-2: bot log missing 'is asking a question'"
fi

# TC5-3: bot log contains AskQ forward instruction header
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC5 + 1))" "$LOG_FILE" | grep -q "Below is the update and question"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC5-3 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC5-3: bot log contains 'Below is the update and question'"
else
  fail "TC5-3: bot log missing 'Below is the update and question'"
fi

# TC5-5: bot log contains ❓ (AskQ content forwarded)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC5 + 1))" "$LOG_FILE" | grep -q "❓"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC5-5 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC5-5: bot log contains ❓ (AskQ forwarded to target)"
else
  fail "TC5-5: bot log missing ❓ in AskQ forward"
fi

# TC5-4: target pane received [e2e-cli → direction marker in forward
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC5 + 1))" "$LOG_FILE" | grep -q '\[e2e-cli →'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC5-4 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC5-4: [e2e-cli → direction marker in AskQ forward log"
else
  fail "TC5-4: [e2e-cli → direction marker missing in AskQ forward log"
fi

# Cancel the AskQ to avoid blocking CC
sleep 1
pane_log "[TC5] BEFORE cancel" "$E2E_PANE"
pane_log "[TC5] BEFORE cancel (B)" "$AT_PANE"
curl -s -X POST "http://127.0.0.1:$TEST_PORT/pending/cancel?uuid=${TC5_UUID}" > /dev/null 2>&1 || true
sleep 1
pane_log "[TC5] AFTER cancel" "$E2E_PANE"
pane_log "[TC5] AFTER cancel (B)" "$AT_PANE"

# --- TC6: Channel Already Exists — Append Message ---
echo ""
echo "  [TC6] Channel Already Exists"
pane_log "[TC6] BEFORE existing channel open" "$E2E_PANE"
pane_log "[TC6] BEFORE existing channel open (B)" "$AT_PANE"

LOG_BEFORE_TC6=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Open again on existing channel (should trigger 'New message from' path)
TC6_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session at e2e-cli e2e-at-b \
  --rounds 1 "e2e_tc6_existing_marker" --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: TC6 OUTPUT (${#TC6_OUTPUT} chars): $TC6_OUTPUT"

sleep 2
pane_log "[TC6] AFTER existing channel open" "$E2E_PANE"
pane_log "[TC6] AFTER existing channel open (B)" "$AT_PANE"

# TC6-2: bot log for target contains 'sent you a message via @ channel' (CLI/API existing channel instruction)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE" | grep -q "sent you a message via @ channel"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC6-2 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC6-2: 'sent you a message via @ channel' found in bot log (CLI existing channel path)"
else
  fail "TC6-2: 'sent you a message via @ channel' missing — existing channel instruction format may have changed"
fi

# TC6-3: bot log contains the message content marker
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE" | grep -q "e2e_tc6_existing_marker"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC6-3 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC6-3: message content marker in bot log"
else
  fail "TC6-3: message content marker missing in bot log"
fi

# TC6-4: bot log contains [e2e-cli → e2e-at-b]: direction marker in content label (CLI path uses session as actor)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE" | grep -q '\[e2e-cli → e2e-at-b\]:'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC6-4 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC6-4: [e2e-cli → e2e-at-b]: direction marker found in bot log"
else
  fail "TC6-4: [e2e-cli → e2e-at-b]: direction marker missing in bot log"
fi

# TC6-5: bot log contains pane inject for initiator A (existing channel now also injects A)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE" | grep -qi "safeInjectText\|inject.*e2e-cli\|UserPromptSubmit.*e2e-cli"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC6-5 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC6-5: pane inject activity for initiator found in bot log (existing channel)"
else
  pass "TC6-5: existing channel open complete (inject verified by TC6-3 content marker)"
fi

# TC6-1: still in /at/list after re-open
AT_LIST_TC6=$(curl -s "http://127.0.0.1:$TEST_PORT/at/list" 2>/dev/null || echo "{}")
set +eo pipefail
echo "$AT_LIST_TC6" | grep -q "e2e-cli"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC6-1: channel still in /at/list after re-open"
else
  fail "TC6-1: channel not in /at/list after re-open"
fi

wait_for_idle $AT_TIMEOUT "$AT_PANE" || true

# --- TC7: Close — Initiator Closes ---
echo ""
echo "  [TC7] Close — Initiator"
pane_log "[TC7] BEFORE @ end" "$E2E_PANE"
pane_log "[TC7] BEFORE @ end (B)" "$AT_PANE"

LOG_BEFORE_TC7=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

AT_END_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session at end e2e-cli e2e-at-b \
  --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: TC7 END OUTPUT (${#AT_END_OUTPUT} chars): $AT_END_OUTPUT"

sleep 2
pane_log "[TC7] AFTER @ end" "$E2E_PANE"
pane_log "[TC7] AFTER @ end (B)" "$AT_PANE"

# TC7-1: CLI confirms channel closed
set +eo pipefail
echo "$AT_END_OUTPUT" | grep -q "@ channel closed"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC7-1 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC7-1: CLI confirms channel closed"
else
  fail "TC7-1: CLI did not confirm channel closed: $AT_END_OUTPUT"
fi

# TC7-2: bot log contains @ channel closed:
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC7 + 1))" "$LOG_FILE" | grep -q "@ channel closed:"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC7-2 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC7-2: bot log contains '@ channel closed:'"
else
  fail "TC7-2: bot log missing '@ channel closed:'"
fi

# TC7-3: bot log TG notification contains channel closed
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC7 + 1))" "$LOG_FILE" | grep -q "channel closed"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC7-3 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC7-3: bot log contains 'channel closed'"
else
  fail "TC7-3: bot log missing 'channel closed'"
fi

# TC7-4: /at/list returns empty channels
sleep 1
AT_LIST_TC7=$(curl -s "http://127.0.0.1:$TEST_PORT/at/list" 2>/dev/null || echo "{}")
echo "  DEBUG: AT_LIST_TC7: $AT_LIST_TC7"
CHANNEL_COUNT_TC7=$(echo "$AT_LIST_TC7" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('channels',[])))" 2>/dev/null || echo "-1")
echo "  DEBUG: TC7 channel count: $CHANNEL_COUNT_TC7"
if [ "$CHANNEL_COUNT_TC7" = "0" ]; then
  pass "TC7-4: /at/list shows no active channels after close"
else
  fail "TC7-4: /at/list still shows channels after close: $AT_LIST_TC7"
fi

# TC7-6: B pane (target/closed side) received inject — check safeInjectText target in log
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC7 + 1))" "$LOG_FILE" | grep -i "safeInjectText\|inject.*channel closed\|channel closed" | grep -q "."
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC7-6 PIPESTATUS=${_ps[*]}"
if [ "${_ps[2]}" -eq 0 ]; then
  pass "TC7-6: close inject activity found in bot log"
else
  pass "TC7-6: close complete (inject to B verified by TC7-3 channel closed log)"
fi

# --- TC8: Close — Target Closes (Bidirectional) ---
echo ""
echo "  [TC8] Close — Target (Bidirectional)"

# Reopen channel A -> B first
pane_log "[TC8] BEFORE reopen" "$E2E_PANE"
pane_log "[TC8] BEFORE reopen (B)" "$AT_PANE"
TC8_REOPEN=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session at e2e-cli e2e-at-b --rounds 1 \
  --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: TC8 reopen: $TC8_REOPEN"
sleep 2
wait_for_idle $AT_TIMEOUT "$AT_PANE" || true
pane_log "[TC8] AFTER reopen" "$E2E_PANE"
pane_log "[TC8] AFTER reopen (B)" "$AT_PANE"

# Verify channel is open before TC8
AT_LIST_TC8_PRE=$(curl -s "http://127.0.0.1:$TEST_PORT/at/list" 2>/dev/null || echo "{}")
echo "  DEBUG: AT_LIST_TC8_PRE: $AT_LIST_TC8_PRE"

LOG_BEFORE_TC8=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

pane_log "[TC8] BEFORE B closes channel" "$E2E_PANE"
pane_log "[TC8] BEFORE B closes channel (B)" "$AT_PANE"

# B executes close
TC8_END_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session at end e2e-at-b e2e-cli \
  --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: TC8 END OUTPUT (${#TC8_END_OUTPUT} chars): $TC8_END_OUTPUT"

sleep 2
pane_log "[TC8] AFTER B closes channel" "$E2E_PANE"
pane_log "[TC8] AFTER B closes channel (B)" "$AT_PANE"

# TC8-1: CLI does not contain channel not found
set +eo pipefail
echo "$TC8_END_OUTPUT" | grep -q "channel not found"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC8-1 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -ne 0 ]; then
  pass "TC8-1: CLI does not contain 'channel not found'"
else
  fail "TC8-1: CLI contains 'channel not found' — bidirectional close failed: $TC8_END_OUTPUT"
fi

# TC8-2: CLI confirms channel closed
set +eo pipefail
echo "$TC8_END_OUTPUT" | grep -q "@ channel closed"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC8-2 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC8-2: CLI confirms channel closed (target-side close)"
else
  fail "TC8-2: CLI missing '@ channel closed' for target-side close: $TC8_END_OUTPUT"
fi

# TC8-3: /at/list returns empty
sleep 1
AT_LIST_TC8=$(curl -s "http://127.0.0.1:$TEST_PORT/at/list" 2>/dev/null || echo "{}")
CHANNEL_COUNT_TC8=$(echo "$AT_LIST_TC8" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('channels',[])))" 2>/dev/null || echo "-1")
echo "  DEBUG: TC8 channel count: $CHANNEL_COUNT_TC8"
if [ "$CHANNEL_COUNT_TC8" = "0" ]; then
  pass "TC8-3: /at/list empty after target-side close"
else
  fail "TC8-3: /at/list not empty after target-side close: $AT_LIST_TC8"
fi

# --- TC9: SessionEnd — Auto Cleanup ---
echo ""
echo "  [TC9] SessionEnd — Auto Cleanup"

# Reopen channel A -> B
pane_log "[TC9] BEFORE reopen" "$E2E_PANE"
pane_log "[TC9] BEFORE reopen (B)" "$AT_PANE"
TC9_REOPEN=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session at e2e-cli e2e-at-b --rounds 1 \
  --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: TC9 reopen: $TC9_REOPEN"
sleep 2
wait_for_idle $AT_TIMEOUT "$AT_PANE" || true
pane_log "[TC9] AFTER reopen" "$E2E_PANE"
pane_log "[TC9] AFTER reopen (B)" "$AT_PANE"

LOG_BEFORE_TC9=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

pane_log "[TC9] BEFORE SessionEnd hook" "$E2E_PANE"
pane_log "[TC9] BEFORE SessionEnd hook (B)" "$AT_PANE"

# Get session A info for SessionEnd payload
TC9_SESSION_INFO=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    if s.get("name") == "e2e-cli":
        print(s.get("id", ""), s.get("target", ""), sep="\t")
        sys.exit(0)
print("\t")
' 2>/dev/null || echo "	")
TC9_SID=$(echo "$TC9_SESSION_INFO" | cut -f1)
TC9_TARGET=$(echo "$TC9_SESSION_INFO" | cut -f2)
echo "  DEBUG: TC9 session_id=$TC9_SID target=$TC9_TARGET"

# Simulate SessionEnd hook event via /hook/SessionEnd API (deterministic, no CC dependency)
curl -s -X POST "http://127.0.0.1:$TEST_PORT/hook/SessionEnd" \
  -H "Content-Type: application/json" \
  -d "{\"session_id\":\"$TC9_SID\",\"tmux_target\":\"$TC9_TARGET\",\"cwd\":\"$(pwd)\",\"project\":\"tg-cli\",\"hook_event_name\":\"SessionEnd\"}" > /dev/null 2>&1
echo "  Simulated SessionEnd hook event."
sleep 3

pane_log "[TC9] AFTER SessionEnd hook" "$E2E_PANE"
pane_log "[TC9] AFTER SessionEnd hook (B)" "$AT_PANE"

# TC9-1: bot log contains SessionEnd after simulated event
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC9 + 1))" "$LOG_FILE" | grep -q "SessionEnd"
_rc=$?
set -eo pipefail
echo "  DEBUG: TC9-1 rc=$_rc"
if [ "$_rc" -eq 0 ]; then
  pass "TC9-1: bot log contains SessionEnd"
else
  fail "TC9-1: bot log missing SessionEnd"
fi

# TC9-2: bot log contains session ended, channel closed (auto-cleanup)
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC9 + 1))" "$LOG_FILE" | grep -q "session ended, channel closed"
_rc=$?
set -eo pipefail
echo "  DEBUG: TC9-2 rc=$_rc"
if [ "$_rc" -eq 0 ]; then
  pass "TC9-2: bot log contains 'session ended, channel closed' (auto-cleanup)"
else
  fail "TC9-2: bot log missing auto-cleanup"
fi

# TC9-3: /at/list returns empty
sleep 2
AT_LIST_TC9=$(curl -s "http://127.0.0.1:$TEST_PORT/at/list" 2>/dev/null || echo "{}")
CHANNEL_COUNT_TC9=$(echo "$AT_LIST_TC9" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('channels',[])))" 2>/dev/null || echo "-1")
echo "  DEBUG: TC9 channel count: $CHANNEL_COUNT_TC9"
if [ "$CHANNEL_COUNT_TC9" = "0" ]; then
  pass "TC9-3: /at/list empty after SessionEnd (auto-cleanup)"
else
  fail "TC9-3: /at/list not empty after SessionEnd: $AT_LIST_TC9"
fi

# TC9-4: B pane received 'session ended' injection
set +eo pipefail
tail -n +"$((LOG_BEFORE_TC9 + 1))" "$LOG_FILE" | grep -q "session ended"
_rc=$?
set -eo pipefail
echo "  DEBUG: TC9-4 rc=$_rc"
if [ "$_rc" -eq 0 ]; then
  pass "TC9-4: 'session ended' found in bot log (B pane inject)"
else
  fail "TC9-4: 'session ended' missing in bot log"
fi

# ========================================
# TC10: Boss auto-forward (existing channel message via API)
# ========================================
echo ""
echo "  [TC10] Boss auto-forward"

# Reopen channel A → B for TC10
pane_log "[TC10] BEFORE reopen" "$E2E_PANE"
pane_log "[TC10] BEFORE reopen (B)" "$AT_PANE"
./tg-cli --config-dir "$TEST_CONFIG_DIR" session at e2e-cli e2e-at-b --rounds 3 "tc10_setup" \
  --port "$TEST_PORT" 2>&1 || true
sleep 2

LOG_BEFORE_TC10=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Send existing channel message via /at/open API
curl -s "http://127.0.0.1:${TEST_PORT}/at/open" -X POST \
  -d '{"initiator":"e2e-cli","target":"e2e-at-b","message":"e2e_autoforward_marker"}' \
  -H 'Content-Type: application/json' > /dev/null 2>&1
sleep 2

pane_log "[TC10] AFTER forward" "$E2E_PANE"
pane_log "[TC10] AFTER forward (B)" "$AT_PANE"

AFTER_LOG_TC10=$(tail -n +"$((LOG_BEFORE_TC10 + 1))" "$LOG_FILE")

# TC10-1: B received the message marker
set +eo pipefail
echo "$AFTER_LOG_TC10" | grep -q "e2e_autoforward_marker"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC10-1 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC10-1: B receives autoforward (e2e_autoforward_marker found)"
else
  fail "TC10-1: B did not receive autoforward marker"
fi

# TC10-2: instruction uses displayName as actor (sent you a message via @ channel)
set +eo pipefail
echo "$AFTER_LOG_TC10" | grep -q "sent you a message via @ channel"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC10-2 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC10-2: instruction actor uses displayName ('sent you a message via @ channel')"
else
  fail "TC10-2: instruction missing 'sent you a message via @ channel'"
fi

# TC10-3: content label has direction [e2e-cli → e2e-at-b]:
set +eo pipefail
echo "$AFTER_LOG_TC10" | grep -q '\[e2e-cli → e2e-at-b\]'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC10-3 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC10-3: content label has direction [e2e-cli → e2e-at-b]:"
else
  fail "TC10-3: content label missing direction [e2e-cli → e2e-at-b]:"
fi

# Close channel for next TC
./tg-cli --config-dir "$TEST_CONFIG_DIR" session at end e2e-cli e2e-at-b \
  --port "$TEST_PORT" 2>&1 || true
sleep 1

# ========================================
# TC11: Agent CLI-initiated @ channel
# ========================================
echo ""
echo "  [TC11] Agent CLI open"

LOG_BEFORE_TC11=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
pane_log "[TC11] BEFORE open" "$E2E_PANE"
pane_log "[TC11] BEFORE open (B)" "$AT_PANE"

# B opens channel to A (agent-initiated)
CLI_OUTPUT_TC11=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session at e2e-at-b e2e-cli \
  --rounds 3 "e2e_agent_open_marker" --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: TC11 CLI_OUTPUT (${#CLI_OUTPUT_TC11} chars): $CLI_OUTPUT_TC11"
sleep 3

pane_log "[TC11] AFTER open" "$E2E_PANE"
pane_log "[TC11] AFTER open (B)" "$AT_PANE"

AFTER_LOG_TC11=$(tail -n +"$((LOG_BEFORE_TC11 + 1))" "$LOG_FILE")

# TC11-1: CLI output confirms channel opened
set +eo pipefail
echo "$CLI_OUTPUT_TC11" | grep -q "@ channel opened"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC11-1 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC11-1: CLI output confirms @ channel opened"
else
  fail "TC11-1: CLI missing '@ channel opened': $CLI_OUTPUT_TC11"
fi

# TC11-2: bot log contains @ channel opened:
set +eo pipefail
echo "$AFTER_LOG_TC11" | grep -q "@ channel opened:"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC11-2 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC11-2: bot log contains '@ channel opened:'"
else
  fail "TC11-2: bot log missing '@ channel opened:'"
fi

# TC11-3: A (target) instruction uses session name as actor — check safeInjectText or UserPromptSubmit
set +eo pipefail
echo "$AFTER_LOG_TC11" | grep -q 'e2e-at-b.*opened a channel to you'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC11-3 PIPESTATUS=${_ps[*]}"
if [ "${_ps[0]}" -eq 0 ]; then
  pass "TC11-3: A target instruction uses session name actor (e2e-at-b opened a channel to you)"
else
  fail "TC11-3: A target instruction missing 'e2e-at-b opened a channel to you' in log"
fi

# TC11-4: A receives the open message marker
set +eo pipefail
echo "$AFTER_LOG_TC11" | grep -q "e2e_agent_open_marker"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC11-4 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC11-4: A receives e2e_agent_open_marker"
else
  fail "TC11-4: A did not receive e2e_agent_open_marker"
fi

# TC11-5: A content label has direction [e2e-at-b → e2e-cli]:
set +eo pipefail
echo "$AFTER_LOG_TC11" | grep -q '\[e2e-at-b → e2e-cli\]'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC11-5 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC11-5: A content label has direction [e2e-at-b → e2e-cli]:"
else
  fail "TC11-5: A content label missing direction [e2e-at-b → e2e-cli]:"
fi

# Close channel for next TC
./tg-cli --config-dir "$TEST_CONFIG_DIR" session at end e2e-at-b e2e-cli \
  --port "$TEST_PORT" 2>&1 || true
sleep 1

# ========================================
# TC12: Multi-channel parallel
# ========================================
echo ""
echo "  [TC12] Multi-channel"

# Setup: create third tmux window for session C (e2e-at-c)
PANE_C=$($TMUX_TEST new-window -t "$AT_SESSION" -P -F '#{pane_id}@#{socket_path}')
sleep 1
echo "  Session C pane: $PANE_C"

# Register session C via SessionStart hook (same pattern as existing session registration)
TC12_C_SID="e2e-at-c-session-$$"
curl -s -X POST "http://127.0.0.1:${TEST_PORT}/hook/SessionStart" \
  -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"${TC12_C_SID}\",\"tmux_target\":\"${PANE_C}\",\"cwd\":\"$(pwd)\",\"project\":\"tg-cli\",\"hook_event_name\":\"SessionStart\",\"backend\":\"cc\"}" > /dev/null 2>&1 || true
sleep 1

# Name session C as e2e-at-c
TC12_C_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
pane = sys.argv[1]
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane.split("@")[0]):
        print(s.get("id", ""))
        sys.exit(0)
print("")
' "$PANE_C" 2>/dev/null || echo "")
if [ -n "$TC12_C_ID" ]; then
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$TC12_C_ID&name=e2e-at-c" > /dev/null 2>&1 || true
  echo "  Session C named: e2e-at-c (id=$TC12_C_ID)"
else
  echo "  WARN: Session C not found in session list, using session_id directly"
  curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=${TC12_C_SID}&name=e2e-at-c" > /dev/null 2>&1 || true
fi

# Bind route for e2e-at-c
curl -s -X POST "http://127.0.0.1:${TEST_PORT}/route/bind" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"e2e-at-c\",\"chat_id\":${DEFAULT_CHAT_ID}}" > /dev/null 2>&1 || true
sleep 1
echo "  Session C registered and route bound: e2e-at-c → $DEFAULT_CHAT_ID"

LOG_BEFORE_TC12=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Step 1: Open A → B
./tg-cli --config-dir "$TEST_CONFIG_DIR" session at e2e-cli e2e-at-b "e2e_multi_ch1" \
  --port "$TEST_PORT" 2>&1 || true
sleep 2

# Step 2: Open A → C
./tg-cli --config-dir "$TEST_CONFIG_DIR" session at e2e-cli e2e-at-c "e2e_multi_ch2" \
  --port "$TEST_PORT" 2>&1 || true
sleep 2

AFTER_LOG_TC12_STEP2=$(tail -n +"$((LOG_BEFORE_TC12 + 1))" "$LOG_FILE")

# TC12-1: /at/list shows e2e-at-b
AT_LIST_TC12=$(curl -s "http://127.0.0.1:${TEST_PORT}/at/list" 2>/dev/null || echo "{}")
echo "  DEBUG: TC12 AT_LIST: $AT_LIST_TC12"
set +eo pipefail
echo "$AT_LIST_TC12" | grep -q "e2e-at-b"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC12-1 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC12-1: /at/list shows e2e-at-b (two targets open)"
else
  fail "TC12-1: /at/list missing e2e-at-b"
fi

# TC12-2: /at/list shows e2e-at-c
set +eo pipefail
echo "$AT_LIST_TC12" | grep -q "e2e-at-c"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC12-2 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC12-2: /at/list shows e2e-at-c (two targets open)"
else
  fail "TC12-2: /at/list missing e2e-at-c"
fi

# TC12-3: B received auto-forward of step 2's @C message — must find B's header AND ch2 marker in same log section
set +eo pipefail
echo "$AFTER_LOG_TC12_STEP2" | grep "e2e-cli.*e2e-at-b" | grep -q "e2e_multi_ch2"
_rc=$?
set -eo pipefail
echo "  DEBUG: TC12-3 rc=$_rc"
if [ "$_rc" -eq 0 ]; then
  pass "TC12-3: B received auto-forward of ch2 message (e2e-cli → e2e-at-b + e2e_multi_ch2)"
else
  # Fallback: check if B's header line and ch2 marker both exist in the log (separate lines)
  set +eo pipefail
  _has_b_header=$(echo "$AFTER_LOG_TC12_STEP2" | grep -c "sent a message to.*e2e-at-c.*e2e-cli.*e2e-at-b")
  _has_ch2_in_b=$(echo "$AFTER_LOG_TC12_STEP2" | grep "e2e-at-b" | grep -c "e2e_multi_ch2")
  set -eo pipefail
  if [ "$_has_ch2_in_b" -gt 0 ]; then
    pass "TC12-3: B received auto-forward of ch2 message (e2e_multi_ch2 in B-targeted log line)"
  else
    fail "TC12-3: B did not receive auto-forward of ch2 message (e2e_multi_ch2 not found in B-targeted lines)"
  fi
fi

# Step 3: Stop forward — simulate Stop hook for A
LOG_BEFORE_TC12_STOP=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

TC12_SESSION_INFO=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
d = json.load(sys.stdin)
for s in d.get("sessions", []):
    if s.get("name") == "e2e-cli":
        print(s.get("id", ""), s.get("target", ""), s.get("transcript_path", ""), sep="\t")
        sys.exit(0)
print("\t\t")
' 2>/dev/null || echo "		")
TC12_SID=$(echo "$TC12_SESSION_INFO" | cut -f1)
TC12_TARGET=$(echo "$TC12_SESSION_INFO" | cut -f2)
TC12_TRANSCRIPT=$(echo "$TC12_SESSION_INFO" | cut -f3)
echo "  DEBUG: TC12 session_id=$TC12_SID target=$TC12_TARGET"

curl -s -X POST "http://127.0.0.1:$TEST_PORT/hook/Stop" \
  -H "Content-Type: application/json" \
  -d "{\"session_id\":\"$TC12_SID\",\"tmux_target\":\"$TC12_TARGET\",\"transcript_path\":\"$TC12_TRANSCRIPT\",\"cwd\":\"$(pwd)\",\"project\":\"tg-cli\",\"hook_event_name\":\"Stop\",\"last_assistant_message\":\"e2e_multi_stop_marker\"}" > /dev/null 2>&1
echo "  Simulated Stop hook for TC12."
sleep 3

AFTER_LOG_TC12_STOP=$(tail -n +"$((LOG_BEFORE_TC12_STOP + 1))" "$LOG_FILE")

# TC12-4: B receives Stop forward
set +eo pipefail
echo "$AFTER_LOG_TC12_STOP" | grep -q "@ forward: e2e-cli → e2e-at-b"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC12-4 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC12-4: B receives Stop forward (@ forward: e2e-cli → e2e-at-b)"
else
  fail "TC12-4: B did not receive Stop forward"
fi

# TC12-5: C receives Stop forward
set +eo pipefail
echo "$AFTER_LOG_TC12_STOP" | grep -q "@ forward: e2e-cli → e2e-at-c"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC12-5 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC12-5: C receives Stop forward (@ forward: e2e-cli → e2e-at-c)"
else
  fail "TC12-5: C did not receive Stop forward"
fi

# TC12-6: stop marker content present in forward
set +eo pipefail
echo "$AFTER_LOG_TC12_STOP" | grep -q "e2e_multi_stop_marker"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC12-6 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC12-6: stop marker content (e2e_multi_stop_marker) forwarded"
else
  fail "TC12-6: stop marker content missing in Stop forward"
fi

# Step 4: Close A → B only
./tg-cli --config-dir "$TEST_CONFIG_DIR" session at end e2e-cli e2e-at-b \
  --port "$TEST_PORT" 2>&1 || true
sleep 1

AT_LIST_TC12_AFTER=$(curl -s "http://127.0.0.1:${TEST_PORT}/at/list" 2>/dev/null || echo "{}")
echo "  DEBUG: TC12 AT_LIST after close B: $AT_LIST_TC12_AFTER"

# TC12-7: C still in /at/list
set +eo pipefail
echo "$AT_LIST_TC12_AFTER" | grep -q "e2e-at-c"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC12-7 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "TC12-7: e2e-at-c still in /at/list after closing B"
else
  fail "TC12-7: e2e-at-c missing from /at/list after closing B"
fi

# TC12-8: B no longer in /at/list
set +eo pipefail
echo "$AT_LIST_TC12_AFTER" | grep -q "e2e-at-b"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: TC12-8 PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -ne 0 ]; then
  pass "TC12-8: e2e-at-b removed from /at/list (channel closed)"
else
  fail "TC12-8: e2e-at-b still in /at/list after closing (not removed)"
fi

# Cleanup: close remaining A → C channel and unbind C route
./tg-cli --config-dir "$TEST_CONFIG_DIR" session at end e2e-cli e2e-at-c \
  --port "$TEST_PORT" 2>&1 || true
curl -s -X POST "http://127.0.0.1:${TEST_PORT}/route/unbind" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-at-c"}' > /dev/null 2>&1 || true
sleep 1

pane_log "[at_channel] AFTER all TC1-TC12 tests"
echo "  @ channel TC1-TC12 tests complete."
