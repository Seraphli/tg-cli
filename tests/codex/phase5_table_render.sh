#!/bin/bash
# Phase: Table rendering test (image) — Codex backend
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Table rendering test (image) ---"

ensure_infrastructure

wait_for_idle $TIMEOUT

LOG_BEFORE_TABLE=$(wc -l < "$LOG_FILE")

pane_log "[table_render] BEFORE table prompt"
inject_prompt "Without using any tools, output a markdown table with 3 columns (状态, 名称, 城市) and 2 rows: (✅, 小明, 北京) and (⚠️, Alice, Shanghai). Output ONLY the table, nothing else."
pane_log "[table_render] AFTER table prompt"

# Wait for Table render log (sendEventNotification now calls shared table helper)
# Codex uses Stop-based sendEventNotification path (no MessageDisplay); table image is sent from there.
ELAPSED=0
TABLE_STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_TABLE" ]; then
    if tail -n +"$((LOG_BEFORE_TABLE + 1))" "$LOG_FILE" | grep "Table render:" > /dev/null 2>&1; then
      TABLE_STOP_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for table render notification... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$TABLE_STOP_FOUND" = true ]; then
  # Table text is kept in the notification body (not removed) — check via Notification sent body log
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TABLE + 1))" "$LOG_FILE")
  TABLE_RENDER_LOG=$(echo "$NEW_LOGS" | grep "Table render:" | head -1 || true)
  echo "  DEBUG: TABLE_RENDER_LOG: $TABLE_RENDER_LOG"
  if echo "$TABLE_RENDER_LOG" | grep "tables=[1-9]" > /dev/null 2>&1; then
    pass "Table render logged with non-zero table count (text+image)"
  else
    fail "Table render log missing expected table count: $TABLE_RENDER_LOG"
  fi

  # Wait for table image upload to complete (sendTableImages logs "Stream table image sent:")
  IMG_ELAPSED=0
  IMG_FOUND=false
  while [ $IMG_ELAPSED -lt 30 ]; do
    if tail -n +"$((LOG_BEFORE_TABLE + 1))" "$LOG_FILE" | grep "Stream table image sent:" > /dev/null 2>&1; then
      IMG_FOUND=true
      break
    fi
    sleep 1
    IMG_ELAPSED=$((IMG_ELAPSED + 1))
  done

  if [ "$IMG_FOUND" = true ]; then
    pass "Table image sent via shared helper (Stream table image sent:)"
  else
    fail "Table image not sent (Stream table image sent: not found)"
  fi

  # Verify no table image render failure
  if echo "$NEW_LOGS" | grep "Table image render failed" > /dev/null 2>&1; then
    fail "Table image rendering had errors"
  else
    pass "Table image rendered without errors"
  fi
else
  fail "Table render log not received within ${TIMEOUT}s"
fi

wait_for_idle $TIMEOUT
pane_log "[table_render] AFTER CC idle"
