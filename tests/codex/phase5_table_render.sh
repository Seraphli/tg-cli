#!/bin/bash
# Phase: Table rendering test (native rich inline table) — Codex backend
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Table rendering test (native rich inline table) ---"

ensure_infrastructure

start_codex "e2e-codex-5"

wait_for_idle $TIMEOUT

LOG_BEFORE_TABLE=$(wc -l < "$LOG_FILE")

pane_log "[table_render] BEFORE table prompt"
inject_prompt "Without using any tools, output a markdown table with 3 columns (状态, 名称, 城市) and 2 rows: (✅, 小明, 北京) and (⚠️, Alice, Shanghai). Output ONLY the table, nothing else."
pane_log "[table_render] AFTER table prompt"

# Wait for Stop notification — rich path logs "Notification sent ... body=" with the rich HTML body
ELAPSED=0
TABLE_STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_TABLE" ]; then
    if tail -n +"$((LOG_BEFORE_TABLE + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
      TABLE_STOP_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for table Stop notification... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$TABLE_STOP_FOUND" = true ]; then
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TABLE + 1))" "$LOG_FILE")

  # Assert native <table bordered striped> tag (Fix 5) in the rich body logged by "Notification sent ... body="
  NOTIF_BODY_LINE=$(echo "$NEW_LOGS" | grep "Notification sent.*Stop.*body=" | head -1 || true)
  echo "  DEBUG: NOTIF_BODY_LINE: ${NOTIF_BODY_LINE:0:200}"
  if echo "$NOTIF_BODY_LINE" | grep -q "<table bordered striped>" 2>/dev/null; then
    pass "Notification body contains native <table bordered striped> tag (rich inline table)"
  else
    fail "Notification body missing <table bordered striped> tag: $NOTIF_BODY_LINE"
  fi

  # Assert <td> cells present (verifies row content is rendered inline)
  if echo "$NOTIF_BODY_LINE" | grep -q "<td>" 2>/dev/null; then
    pass "Notification body contains <td> cells (rich inline table cells)"
  else
    fail "Notification body missing <td> cells: $NOTIF_BODY_LINE"
  fi

  # Assert CJK content (小明, 北京) appears in the body
  if echo "$NOTIF_BODY_LINE" | grep -q "小明" 2>/dev/null; then
    pass "Notification body contains CJK text from table cell (小明)"
  else
    fail "Notification body missing CJK text: $NOTIF_BODY_LINE"
  fi

  # Assert NO "Stream table image sent:" — rich path does NOT produce table images
  if echo "$NEW_LOGS" | grep "Stream table image sent:" > /dev/null 2>&1; then
    fail "Table image was sent via Stream path — should NOT happen on rich sendEventNotification path"
  else
    pass "No table image PNG sent (native inline table replaces image path)"
  fi

  # Assert NO "Table render:" log — that log line belongs to the legacy/streaming image path
  if echo "$NEW_LOGS" | grep "Table render:" > /dev/null 2>&1; then
    fail "Legacy 'Table render:' log appeared — should NOT appear on rich sendEventNotification path"
  else
    pass "No legacy 'Table render:' log (expected: rich path skips image render)"
  fi
else
  fail "Table Stop notification not received within ${TIMEOUT}s"
fi

wait_for_idle $TIMEOUT
pane_log "[table_render] AFTER CC idle"
