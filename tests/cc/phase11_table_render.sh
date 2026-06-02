#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Table rendering test (image) ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-11"

wait_for_idle $TIMEOUT

LOG_BEFORE_TABLE=$(wc -l < "$LOG_FILE")

pane_log "[table_render] BEFORE table prompt"
inject_prompt "Without using any tools, output a markdown table with 3 columns (状态, 名称, 城市) and 2 rows: (✅, 小明, 北京) and (⚠️, Alice, Shanghai). Output ONLY the table, nothing else."
pane_log "[table_render] AFTER table prompt"

# Wait for Stop notification
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
  # Wait for table image upload to complete (logged after Stop notification)
  IMG_ELAPSED=0
  IMG_FOUND=false
  while [ $IMG_ELAPSED -lt 30 ]; do
    if tail -n +"$((LOG_BEFORE_TABLE + 1))" "$LOG_FILE" | grep "Table image.*sent to chat" > /dev/null 2>&1; then
      IMG_FOUND=true
      break
    fi
    sleep 1
    IMG_ELAPSED=$((IMG_ELAPSED + 1))
  done

  if [ "$IMG_FOUND" = true ]; then
    pass "Table rendered as image and sent to chat"
  else
    fail "Table image not sent to chat"
  fi

  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TABLE + 1))" "$LOG_FILE")
  # Verify no table image render failure
  if echo "$NEW_LOGS" | grep "Table image render failed" > /dev/null 2>&1; then
    fail "Table image rendering had errors"
  else
    pass "Table image rendered without errors"
  fi
else
  fail "Table Stop notification not received within ${TIMEOUT}s"
fi

wait_for_idle $TIMEOUT
pane_log "[table_render] AFTER CC idle"
