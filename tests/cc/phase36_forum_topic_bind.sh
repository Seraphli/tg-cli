#!/bin/bash
# Phase 36: a session gets its own topic in a REAL forum supergroup (real Telegram, test bot), its messages land in
# that topic, and a rebind to the private chat and back leaves the old place empty and reuses the topic.
# Needs E2E_FORUM_CHAT_ID = a forum supergroup (topics enabled) where the test bot is an administrator
# with "Manage Topics" and "Delete messages". The phase FAILS when it is unset.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Forum topic bind test (real Telegram) ---"

if [ -z "${E2E_FORUM_CHAT_ID:-}" ]; then
  fail "E2E_FORUM_CHAT_ID not set"
fi
if ! [[ "$E2E_FORUM_CHAT_ID" =~ ^-[0-9]+$ ]]; then
  fail "E2E_FORUM_CHAT_ID is not a group chat id (-100...): $E2E_FORUM_CHAT_ID"
fi
FORUM="$E2E_FORUM_CHAT_ID"
AGENT_NAME="e2e-forum"
TOPICS=()
ROUTE_TOPIC=0

ensure_infrastructure

# Sets ROUTE_TOPIC to the route's current topic id (0 = none) and remembers every topic seen, for the cleanup.
route_topic() {
  ROUTE_TOPIC=$(curl -s "http://127.0.0.1:$TEST_PORT/route/list" | jq -r --arg n "$AGENT_NAME" '.name_routes[$n].topicId // 0')
  ROUTE_TOPIC=${ROUTE_TOPIC:-0}
  if [ "$ROUTE_TOPIC" != "0" ] && [[ " ${TOPICS[*]:-} " != *" $ROUTE_TOPIC "* ]]; then
    TOPICS+=("$ROUTE_TOPIC")
  fi
}

# Polls every 2 s up to $TIMEOUT until the bot log after line $1 holds a "Stream relabel ✅:" line (cmd/stream.go:83,
# written when the turn's stream is finished, after its last message was sent), the way phase16 waits for the turn
# to end. Returns 1 when it never appears; there is no idle fallback. grep without -q so tail never gets SIGPIPE.
wait_for_turn_end() {
  local elapsed=0
  while [ "$elapsed" -lt "$TIMEOUT" ]; do
    if tail -n +"$(($1 + 1))" "$LOG_FILE" | grep "Stream relabel ✅:" > /dev/null 2>&1; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  return 1
}

# Binds through the real CLI; sets BIND_OUT and BIND_RC.
bind_via_cli() {
  BIND_RC=0
  BIND_OUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" route bind --name "$AGENT_NAME" --chat "$1" --port "$TEST_PORT" 2>&1) || BIND_RC=$?
  echo "  DEBUG: route bind chat=$1 rc=$BIND_RC output: $BIND_OUT"
}

# EXIT trap, runs also when an assertion failed: unbind, delete every topic this phase saw, then the
# harness cleanup (stop_claude or session kill). The bot token is read into a variable and handed to curl
# on stdin (curl config), never in argv and never echoed.
_forum_cleanup() {
  local rc=$?
  set +e +u
  trap - ERR
  route_topic
  curl -s -X POST -H "Content-Type: application/json" \
    -d "$(jq -n --arg n "$AGENT_NAME" '{name: $n}')" \
    "http://127.0.0.1:$TEST_PORT/route/unbind" > /dev/null 2>&1
  local token t
  token=$(jq -r '.botToken // empty' "$CREDENTIALS" 2>/dev/null)
  for t in "${TOPICS[@]}"; do
    printf 'url = "https://api.telegram.org/bot%s/deleteForumTopic"\n' "$token" \
      | curl -s -K - -d chat_id="$FORUM" -d message_thread_id="$t" \
      | jq -r --arg t "$t" '"  [cleanup] deleteForumTopic thread=" + $t + " ok=" + (.ok | tostring) + " " + (.description // "")'
  done
  token=""
  (exit "$rc")
  _cc_phase_cleanup
}

start_claude "e2e-cc-36" "--dangerously-skip-permissions" "false"
trap '_forum_cleanup' EXIT

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
  pass "Extracted session ID: $SESSION_ID"
else
  fail "Could not extract session ID from session list API"
fi
curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=$AGENT_NAME" > /dev/null 2>&1 || true
echo "  Named session $SESSION_ID as $AGENT_NAME"

# 1. Bind into the forum: the session gets its own topic.
echo "  Binding $AGENT_NAME -> forum $FORUM through the CLI"
bind_via_cli "$FORUM"
if [ "$BIND_RC" -eq 0 ]; then
  pass "route bind into the forum exited 0"
else
  fail "route bind into the forum exited $BIND_RC: $BIND_OUT"
fi
route_topic
T1="$ROUTE_TOPIC"
LIST_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/route/list")
echo "  DEBUG: LIST_RESP (${#LIST_RESP} chars): $LIST_RESP"
if echo "$LIST_RESP" | jq -e --arg n "$AGENT_NAME" --argjson c "$FORUM" '.name_routes[$n] | (.chatId == $c) and ((.topicId // 0) > 0) and ((.title // "") != "")' > /dev/null 2>&1; then
  pass "/route/list: chat is the forum, topic $T1 > 0, title stored"
else
  fail "/route/list: the forum route lacks chat, topic or title"
fi
if grep -qF "Route bound: $AGENT_NAME → $FORUM, topic=$T1" <<< "$BIND_OUT"; then
  pass "CLI printed the bound sentence with topic=$T1"
else
  fail "CLI output lacks 'Route bound: ... topic=$T1': $BIND_OUT"
fi

# 2. A message lands in that topic.
LOG_BEFORE_1=$(wc -l < "$LOG_FILE")
pane_log "[forum_topic] BEFORE forum prompt"
inject_prompt "Reply with exactly: forum_topic_ok"
if wait_for_turn_end "$LOG_BEFORE_1"; then
  pass "turn complete after the forum prompt (Stream relabel ✅)"
else
  fail "no Stream relabel ✅ after the forum prompt"
fi
wait_for_idle
pane_log "[forum_topic] AFTER forum prompt idle"
WINDOW1=$(tail -n +"$((LOG_BEFORE_1 + 1))" "$LOG_FILE")
if grep -Eq "TG send: chat=$FORUM msg_id=[0-9]+ .*thread=$T1\$" <<< "$WINDOW1"; then
  pass "a message landed in topic $T1 (TG send ... thread=$T1)"
else
  fail "no 'TG send: chat=$FORUM ... thread=$T1' line after the forum prompt"
fi
GENERAL_SENDS=$(grep -Ec "TG send: chat=$FORUM msg_id=[0-9]+ .*thread=0\$" <<< "$WINDOW1" || true)
if [ "$GENERAL_SENDS" = "0" ]; then
  pass "nothing landed in General (thread=0) of the forum"
else
  fail "$GENERAL_SENDS message(s) landed in General of the forum"
fi

# 5. Rebind to the paired private chat: the old place receives nothing.
echo "  Rebinding $AGENT_NAME -> default chat $DEFAULT_CHAT_ID"
bind_via_cli "$DEFAULT_CHAT_ID"
if [ "$BIND_RC" -eq 0 ]; then
  pass "route bind to the default chat exited 0"
else
  fail "route bind to the default chat exited $BIND_RC: $BIND_OUT"
fi
route_topic
LIST_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/route/list")
if [ "$ROUTE_TOPIC" = "0" ] && echo "$LIST_RESP" | jq -e --arg n "$AGENT_NAME" --argjson c "$DEFAULT_CHAT_ID" '.name_routes[$n].chatId == $c' > /dev/null 2>&1; then
  pass "after the rebind the route is the default chat without a topic"
else
  fail "after the rebind the route is not the default chat without a topic: $LIST_RESP"
fi
LOG_BEFORE_2=$(wc -l < "$LOG_FILE")
pane_log "[forum_topic] BEFORE rebind prompt"
inject_prompt "Reply with exactly: forum_rebind_ok"
if wait_for_turn_end "$LOG_BEFORE_2"; then
  pass "turn complete after the rebind prompt (Stream relabel ✅)"
else
  fail "no Stream relabel ✅ after the rebind prompt"
fi
wait_for_idle
pane_log "[forum_topic] AFTER rebind prompt idle"
WINDOW2=$(tail -n +"$((LOG_BEFORE_2 + 1))" "$LOG_FILE")
FORUM_SENDS=$(grep -Ec "TG send: chat=$FORUM " <<< "$WINDOW2" || true)
if [ "$FORUM_SENDS" = "0" ]; then
  pass "the old place (forum) received nothing after the rebind"
else
  fail "$FORUM_SENDS send(s) still went to the forum after the rebind"
fi
if grep -Eq "TG send: chat=$DEFAULT_CHAT_ID msg_id=[0-9]+ .*thread=0\$" <<< "$WINDOW2"; then
  pass "after the rebind the message went to the default chat (thread=0)"
else
  fail "no 'TG send: chat=$DEFAULT_CHAT_ID ... thread=0' line after the rebind prompt"
fi

# 6. Rebind to the forum: the recorded topic is reused, no second topic.
bind_via_cli "$FORUM"
if [ "$BIND_RC" -eq 0 ]; then
  pass "route bind into the forum again exited 0"
else
  fail "route bind into the forum again exited $BIND_RC: $BIND_OUT"
fi
route_topic
if [ "$ROUTE_TOPIC" = "$T1" ] && [ "${#TOPICS[@]}" -eq 1 ]; then
  pass "rebinding into the forum reused topic $T1 (no second topic)"
else
  fail "rebinding into the forum gave topic $ROUTE_TOPIC (expected $T1), topics seen: ${TOPICS[*]}"
fi
