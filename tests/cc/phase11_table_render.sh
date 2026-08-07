#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Table rendering test (rich HTML) ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-11"

wait_for_idle $TIMEOUT

LOG_BEFORE_TABLE=$(wc -l < "$LOG_FILE")

pane_log "[table_render] BEFORE table prompt"
inject_prompt "Without using any tools, output a markdown table with 3 columns (状态, 名称, 城市) and 2 rows: (✅, 小明, 北京) and (⚠️, Alice, Shanghai). Output ONLY the table, nothing else."
pane_log "[table_render] AFTER table prompt"

# Wait for the finalized table body: EITHER Stream relabel ✅ (incremental streaming — the usual path
# for a long table) OR a Stop direct-send (f22: a fast burst-at-Stop reply is delivered via ": Stop [" /
# "Stop terminal: outcome=direct_send" with NO Stream send that turn — its finalized rich body lands in
# the Stop notification). Accept either; the content asserts below read the rich HTML from the stream
# reconstruction on the relabel path, or from the "TG message [Stop] full_body:" block on the Stop path.
ELAPSED=0
TABLE_STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_NOW=$(wc -l < "$LOG_FILE")
  if [ "$LOG_NOW" -gt "$LOG_BEFORE_TABLE" ]; then
    if tail -n +"$((LOG_BEFORE_TABLE + 1))" "$LOG_FILE" | grep -E "Stream relabel ✅:|: Stop \[|Stop terminal: outcome=direct_send" > /dev/null 2>&1; then
      TABLE_STOP_FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for streaming finalize (Stream relabel ✅) or Stop direct-send... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$TABLE_STOP_FOUND" = true ]; then
  NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TABLE + 1))" "$LOG_FILE")

  # Extract the finalized body into a file for content assertions (region: all lines after LOG_BEFORE_TABLE)
  TABLE_HTML_FILE="/tmp/tg-cli-e2e-table-html.txt"
  reconstruct_tg_full_text "$NEW_LOGS" > "$TABLE_HTML_FILE"
  # f22: on a burst-at-Stop turn there are no Stream send/edit lines, so the reconstruction above is
  # empty; the finalized rich body is delivered only via the Stop direct-send. Append the multi-line
  # "TG message [Stop] full_body:" block (bot_helpers.go:127 logs markdown.RenderRichHTML(rawBody) — the
  # SAME <table bordered striped>/<td> rich HTML the stream path produces) so the asserts below still
  # verify content. That block is logged ONLY on the Stop direct-send path, so on the relabel path this
  # appends nothing and the stream reconstruction is used unchanged.
  printf '%s\n' "$NEW_LOGS" | awk '
    /TG message \[Stop\] full_body:/ { cap=1; next }
    cap && /^\[[0-9]{4}-/ { cap=0 }
    cap { print }
  ' >> "$TABLE_HTML_FILE"

  # Rich path (Fix 5): table renders as native <table bordered striped>/<td> HTML — assert presence in stream body region
  if grep -q "<table bordered striped>" "$TABLE_HTML_FILE" 2>/dev/null; then
    pass "Stream body contains native <table bordered striped> HTML element (rich table rendering)"
  else
    fail "Stream body missing native <table bordered striped> element (expected rich HTML table)"
  fi

  if grep -q "<td>" "$TABLE_HTML_FILE" 2>/dev/null; then
    pass "Stream body contains <td> elements (table cells present)"
  else
    fail "Stream body missing <td> elements"
  fi

  # Verify CJK table content is present in the rendered body
  if grep -q "小明" "$TABLE_HTML_FILE" 2>/dev/null; then
    pass "CJK content (小明) present in stream body"
  else
    fail "CJK content (小明) missing from stream body"
  fi

  # Assert NO table image was sent on the stream path (rich path does not send PNG photos)
  if echo "$NEW_LOGS" | grep "Stream table image sent:" > /dev/null 2>&1; then
    fail "Stream table image sent: appeared — rich path must NOT send table PNG photos"
  else
    pass "No table PNG photo sent (rich path renders inline <table>, as expected)"
  fi

  rm -f "$TABLE_HTML_FILE"
else
  fail "Neither Stream relabel ✅ nor Stop direct-send received within ${TIMEOUT}s (table output not finalized)"
fi

wait_for_idle $TIMEOUT
pane_log "[table_render] AFTER CC idle"
