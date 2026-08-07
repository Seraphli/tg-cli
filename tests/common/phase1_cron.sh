#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Cron job management test ---"

ensure_infrastructure

pane_log "[cron] BEFORE cron tests"

# Cleanup any leftover jobs from previous runs
EXISTING_JOBS=$(curl -s "http://127.0.0.1:$TEST_PORT/cron/list" | jq -r '.jobs[].id // empty' 2>/dev/null)
for jid in $EXISTING_JOBS; do
  curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/remove" \
    -H "Content-Type: application/json" \
    -d "{\"id\":\"$jid\"}" > /dev/null 2>&1
done

# Test 1: Add a print mode cron job via API
LOG_BEFORE=$(wc -l < "$LOG_FILE")
ADD_RESP=$(curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/add" \
  -H "Content-Type: application/json" \
  -d '{"mode":"print","schedule":"1h","prompt":"echo hello cron","cwd":"/tmp"}')

JOB_ID=$(echo "$ADD_RESP" | jq -r '.id // ""')
echo "  DEBUG: ADD_RESP (${#ADD_RESP} chars): $ADD_RESP"
set +eo pipefail
echo "$ADD_RESP" | grep -q '"ok":true'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ -n "$JOB_ID" ] && [ "${_ps[1]}" -eq 0 ]; then
  pass "Cron add print job via API"
else
  fail "Cron add print job via API - response: $ADD_RESP"
fi

# Verify log entry for add
if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "Cron job added via API" > /dev/null 2>&1; then
  pass "Cron add logged"
else
  fail "Cron add not logged in bot log"
fi

# Test 2: List jobs via API
LIST_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/cron/list")
JOB_COUNT=$(echo "$LIST_RESP" | jq '.jobs | length')
if [ "$JOB_COUNT" -ge 1 ]; then
  pass "Cron list returns jobs"
else
  fail "Cron list empty after add - response: $LIST_RESP"
fi

# Verify job ID is in list
echo "  DEBUG: LIST_RESP (${#LIST_RESP} chars): $LIST_RESP"
set +eo pipefail
echo "$LIST_RESP" | grep -q "$JOB_ID"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Cron list contains added job ID"
else
  fail "Cron list does not contain job ID $JOB_ID"
fi

# Test 3: Remove job via API
LOG_BEFORE_REMOVE=$(wc -l < "$LOG_FILE")
REMOVE_RESP=$(curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/remove" \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"$JOB_ID\"}")

echo "  DEBUG: REMOVE_RESP (${#REMOVE_RESP} chars): $REMOVE_RESP"
set +eo pipefail
echo "$REMOVE_RESP" | grep -q '"ok":true'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Cron remove job via API"
else
  fail "Cron remove job via API - response: $REMOVE_RESP"
fi

# Verify log entry for remove
if tail -n +"$((LOG_BEFORE_REMOVE + 1))" "$LOG_FILE" | grep "Cron job removed via API" > /dev/null 2>&1; then
  pass "Cron remove logged"
else
  fail "Cron remove not logged in bot log"
fi

# Test 4: Verify list is now empty (poll for up to 5s)
ELAPSED=0
CRON_EMPTY=false
while [ $ELAPSED -lt 5 ]; do
  LIST_AFTER=$(curl -s "http://127.0.0.1:$TEST_PORT/cron/list")
  JOB_COUNT_AFTER=$(echo "$LIST_AFTER" | jq '.jobs | length')
  if [ "$JOB_COUNT_AFTER" -eq 0 ]; then
    CRON_EMPTY=true
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done
if [ "$CRON_EMPTY" = true ]; then
  pass "Cron list empty after remove"
else
  echo "  DEBUG: cron list after remove: $LIST_AFTER"
  fail "Cron list still has $JOB_COUNT_AFTER jobs after remove"
fi

# Test 5: Add an inject mode job with non-existent agent
INJECT_ADD_RESP=$(curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/add" \
  -H "Content-Type: application/json" \
  -d '{"mode":"inject","schedule":"1h","prompt":"test inject prompt","agent_name":"nonexistent-agent-e2e"}')

INJECT_JOB_ID=$(echo "$INJECT_ADD_RESP" | jq -r '.id // ""')
echo "  DEBUG: INJECT_ADD_RESP (${#INJECT_ADD_RESP} chars): $INJECT_ADD_RESP"
set +eo pipefail
echo "$INJECT_ADD_RESP" | grep -q '"ok":true'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ -n "$INJECT_JOB_ID" ] && [ "${_ps[1]}" -eq 0 ]; then
  pass "Cron add inject job via API"
else
  fail "Cron add inject job via API - response: $INJECT_ADD_RESP"
fi

# Cleanup test 5 job
curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/remove" \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"$INJECT_JOB_ID\"}" > /dev/null

# Test 6: Actual cron execution — inject mode with short interval
LOG_BEFORE_EXEC=$(wc -l < "$LOG_FILE")
EXEC_TOKEN="cron-exec-test-$RANDOM"
EXEC_ADD_RESP=$(curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/add" \
  -H "Content-Type: application/json" \
  -d "{\"mode\":\"inject\",\"schedule\":\"1s\",\"prompt\":\"$EXEC_TOKEN\",\"agent_name\":\"cron-test-nonexistent\"}")
EXEC_JOB_ID=$(echo "$EXEC_ADD_RESP" | jq -r '.id // ""')
EXEC_JOB_SHORT="${EXEC_JOB_ID:0:8}"

# Wait for cron loop to fire (30s tick + margin)
echo "  Waiting 35s for cron loop to trigger..."
sleep 35

# Verify execution in bot log
if tail -n +"$((LOG_BEFORE_EXEC + 1))" "$LOG_FILE" | grep "Cron job executing.*${EXEC_JOB_SHORT}" > /dev/null 2>&1; then
  pass "Cron job execution triggered"
else
  fail "Cron job execution not triggered after 35s"
fi

# Verify agent offline notification
if tail -n +"$((LOG_BEFORE_EXEC + 1))" "$LOG_FILE" | grep "Cron inject job: agent 'cron-test-nonexistent' not online" > /dev/null 2>&1; then
  pass "Cron inject agent offline detected"
else
  fail "Cron inject agent offline not detected"
fi

# Cleanup execution test job
curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/remove" \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"$EXEC_JOB_ID\"}" > /dev/null

pane_log "[cron] AFTER cron tests"

# --- Cron CLI layer tests ---
echo ""
echo "  --- Cron CLI layer tests ---"

# Test cron list via CLI
CLI_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" cron list --port "$TEST_PORT" 2>&1) || true
echo "  DEBUG: CLI_OUTPUT (${#CLI_OUTPUT} chars): $CLI_OUTPUT"
set +eo pipefail
echo "$CLI_OUTPUT" | grep -qi "job\|No cron\|id\|mode"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Cron CLI list: command executed"
else
  fail "Cron CLI list: unexpected output: $CLI_OUTPUT"
fi

# Test: cron add --fresh flag
FRESH_ADD_RESP=$(curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/add" \
  -H "Content-Type: application/json" \
  -d '{"mode":"print","schedule":"1h","prompt":"fresh test","cwd":"/tmp","fresh":true}')
FRESH_JOB_ID=$(echo "$FRESH_ADD_RESP" | jq -r '.id // ""')
FRESH_LIST=$(curl -s "http://127.0.0.1:$TEST_PORT/cron/list")
FRESH_VALUE=$(echo "$FRESH_LIST" | jq -r ".jobs[] | select(.id==\"$FRESH_JOB_ID\") | .fresh")
if [ "$FRESH_VALUE" = "true" ]; then
  pass "Cron fresh flag: job stored with fresh=true"
else
  fail "Cron fresh flag: expected fresh=true, got $FRESH_VALUE"
fi
curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/remove" \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"$FRESH_JOB_ID\"}" > /dev/null

# Test: Rich escaping for cron — HTML-special chars in command output must be escaped
# Criterion v11-cron: EscapeRich prevents <patt> and & from causing TG 400 errors.
# Use an inject job whose notification text contains raw HTML-special chars so we can
# verify the "Cron notification sent: ... body=" log shows the escaped forms.
echo ""
echo "  --- Cron rich-escaping test ---"
LOG_BEFORE_ESC=$(wc -l < "$LOG_FILE")
# Add a short-interval inject job targeting a non-existent agent — this triggers
# sendCronNotification with the agent-not-online message which embeds EscapeRich(agentLabel).
# We use an agent name containing HTML-special chars to verify they are escaped.
ESC_AGENT="esc-test-<patt>-&-agent"
ESC_ADD_RESP=$(curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/add" \
  -H "Content-Type: application/json" \
  -d "{\"mode\":\"inject\",\"schedule\":\"1s\",\"prompt\":\"esc-marker-prompt\",\"agent_name\":\"$ESC_AGENT\"}")
ESC_JOB_ID=$(echo "$ESC_ADD_RESP" | jq -r '.id // ""')
echo "  DEBUG: ESC_ADD_RESP: $ESC_ADD_RESP"

# Wait for cron loop to fire
echo "  Waiting 35s for escaping-test cron loop to trigger..."
sleep 35

# Verify the cron executed
if tail -n +"$((LOG_BEFORE_ESC + 1))" "$LOG_FILE" | grep "Cron job executing.*${ESC_JOB_ID:0:8}" > /dev/null 2>&1; then
  pass "Cron rich-escape: job executed"
else
  fail "Cron rich-escape: job did not execute after 35s"
fi

# Verify "Cron notification sent:" log region shows escaped HTML entities.
# The body is logged verbatim and may span multiple physical lines, so grep the
# entire region after LOG_BEFORE_ESC rather than a single matched line.
ESC_REGION=$(tail -n +"$((LOG_BEFORE_ESC + 1))" "$LOG_FILE")
echo "  DEBUG: ESC_REGION lines: $(echo "$ESC_REGION" | wc -l)"

if echo "$ESC_REGION" | grep -q "&lt;patt&gt;"; then
  pass "Cron rich-escape: < and > escaped to &lt;/&gt; in notification body"
else
  fail "Cron rich-escape: &lt;patt&gt; not found in notification body region"
fi

if echo "$ESC_REGION" | grep -q "&amp;"; then
  pass "Cron rich-escape: & escaped to &amp; in notification body"
else
  fail "Cron rich-escape: &amp; not found in notification body region"
fi

# Verify no RICH_FALLBACK (400 error) occurred — escaped content was accepted by TG
if tail -n +"$((LOG_BEFORE_ESC + 1))" "$LOG_FILE" | grep "RICH_FALLBACK" > /dev/null 2>&1; then
  fail "Cron rich-escape: RICH_FALLBACK triggered — TG 400 error on rich send"
else
  pass "Cron rich-escape: no RICH_FALLBACK (rich send accepted by TG)"
fi

# Anchor on the [ERROR]/[WARN] level so a real TG 400 in the message is caught, but the bot PID
# in the [timestamp][PID=...] prefix (which can contain the digits "400") does NOT false-match.
if tail -n +"$((LOG_BEFORE_ESC + 1))" "$LOG_FILE" | grep -E "\[(ERROR|WARN)\].*(400|Bad Request)" > /dev/null 2>&1; then
  fail "Cron rich-escape: 400/Bad Request error found in logs"
else
  pass "Cron rich-escape: no 400/Bad Request errors"
fi

# Cleanup escape test job
curl -s -X POST "http://127.0.0.1:$TEST_PORT/cron/remove" \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"$ESC_JOB_ID\"}" > /dev/null

pane_log "[cron] AFTER rich-escaping test"
