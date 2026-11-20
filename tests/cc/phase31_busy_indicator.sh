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
    if grep -qE "$pattern" <<< "$(tail -n +"$((since + 1))" "$LOG_FILE")" 2>/dev/null; then
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

# ============================================================
# TC-d: restart sweep — seed a busy_status.json, restart bot → deleted at startup
# ============================================================
echo ""
echo "  TC-d: startup sweep deletes persisted status"

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
# Force a REAL restart: kill the running phase bot (cleanup_sessions pattern, e2e_common.sh:590) and
# wait for the port to stop responding, else start_bot no-ops against the still-listening old bot.
$TMUX_TEST kill-session -t "=$BOT_SESSION" 2>/dev/null || true
_tcd_down=0
while curl -sf -o /dev/null "http://127.0.0.1:$TEST_PORT/session/idle" 2>/dev/null; do
  sleep 1; _tcd_down=$((_tcd_down + 1))
  [ "$_tcd_down" -ge 15 ] && fail "TC-d: bot port $TEST_PORT still responding 15s after kill-session (old bot not down)"
done
start_bot
# start_bot truncates the shared bot.log (e2e_common.sh:521), so the whole fresh log is new-bot output.
# The startup sweep logs BEFORE the HTTP port opens (sweep line precedes "Hook HTTP server listening"),
# hence before start_bot's wait_for_bot_ready returns — so search from offset 0 (truncation guarantees no
# stale match) rather than the post-start_bot line count, which would land past the already-written sweep.
LOG_BEFORE_TCD=0

# The startup sweep should log a delete attempt for the seeded entry.
if wait_for_log_pattern "$LOG_BEFORE_TCD" "busy startup sweep" 15 "TC-d startup sweep"; then
  pass "TC-d: startup sweep log line found"
else
  fail "TC-d: startup sweep not logged after bot restart"
fi
