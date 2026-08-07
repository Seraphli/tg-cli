#!/bin/bash
# Phase: Format rendering test (lists, indentation, code blocks) — Codex backend
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/codex_common.sh"

echo ""
echo "--- Format rendering test (lists, indentation, code blocks) ---"

ensure_infrastructure

start_codex "e2e-codex-6"

wait_for_idle $TIMEOUT

LOG_BEFORE_FMT=$(wc -l < "$LOG_FILE")

pane_log "[format_render] BEFORE format prompt"

# Inject prompt with nested lists, ordered+nested lists, and code blocks
inject_prompt 'Without using any tools, output EXACTLY this text with no modifications and no additional commentary:

Here is a nested list:

- Parent item
  - Child item 1
  - Child item 2
- Another parent

Ordered with nested:

1. Step one
   - Sub A
   - Sub B
2. Step two

Code examples: use `inline code` and a block:

```python
def hello():
    print("world")
```'

pane_log "[format_render] AFTER format prompt"

# Wait for Stop notification
ELAPSED=0
FMT_STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_FMT + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
    FMT_STOP_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for format Stop notification... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$FMT_STOP_FOUND" != true ]; then
  fail "Format Stop notification not received within ${TIMEOUT}s"
  wait_for_idle
  pane_log "[format_render] AFTER CC idle (timeout)"
  exit 0
fi

# === Capture CLI pane output (ground truth) ===
PANE_RAW=$(curl -s "http://127.0.0.1:$TEST_PORT/capture?target=$(printf '%s' "$E2E_PANE" | jq -sRr @uri)" | jq -r '.content // "(empty)"')

# === Extract TG full_text from bot log ===
NEW_LOGS=$(tail -n +"$((LOG_BEFORE_FMT + 1))" "$LOG_FILE")
TG_HTML_FILE="/tmp/tg-cli-e2e-tg-html.txt"
echo "$NEW_LOGS" | awk '/TG message sent \[Stop\].*full_text:/{found=1; sub(/.*full_text:/, ""); print; next} found && /^\[[0-9]{4}-/{exit} found{print}' > "$TG_HTML_FILE"

# Strip HTML tags to get plain text
TG_PLAIN_FILE="/tmp/tg-cli-e2e-tg-plain.txt"
sed 's/<[^>]*>//g' "$TG_HTML_FILE" > "$TG_PLAIN_FILE"

# Save debug data to bot log
{
  echo "=== FORMAT_RENDER: PANE RAW ==="
  echo "$PANE_RAW"
  echo "=== FORMAT_RENDER: TG HTML ==="
  cat "$TG_HTML_FILE"
  echo "=== FORMAT_RENDER: TG PLAIN ==="
  cat "$TG_PLAIN_FILE"
} >> "$LOG_FILE"

# === Comparison tests ===

# Test 1: TG rich HTML has <ul> and <li> tags for unordered list items
if grep -q "<ul>" "$TG_HTML_FILE" 2>/dev/null && grep -q "<li>" "$TG_HTML_FILE" 2>/dev/null; then
  pass "TG notification has rich list tags (<ul> and <li>)"
else
  fail "TG notification missing rich list tags (<ul>/<li>) in HTML"
fi

# Test 2: CLI pane shows nested list content (ground truth)
echo "  DEBUG: PANE_RAW (${#PANE_RAW} chars): $PANE_RAW"
set +eo pipefail
echo "$PANE_RAW" | grep -q "Child item" 2>/dev/null
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "CLI pane shows nested list content"
else
  fail "CLI pane missing nested list content"
fi

# Test 3: No excessive blank lines in TG output
TG_MAX_BLANKS=$(awk 'BEGIN{max=0;c=0} /^[[:space:]]*$/{c++;if(c>max)max=c;next} {c=0} END{print max}' "$TG_PLAIN_FILE")
if [ "$TG_MAX_BLANKS" -le 1 ]; then
  pass "TG no excessive blank lines (max_consecutive_blanks=$TG_MAX_BLANKS)"
else
  fail "TG has excessive blank lines (max_consecutive_blanks=$TG_MAX_BLANKS, expected <=1)"
fi

# Test 4: TG blank lines should not exceed CLI pane blank lines
PANE_MAX_BLANKS=$(echo "$PANE_RAW" | awk 'BEGIN{max=0;c=0} /^[[:space:]]*$/{c++;if(c>max)max=c;next} {c=0} END{print max}')
if [ "$TG_MAX_BLANKS" -le "$PANE_MAX_BLANKS" ] || [ "$TG_MAX_BLANKS" -le 1 ]; then
  pass "TG blank lines ($TG_MAX_BLANKS) <= CLI pane ($PANE_MAX_BLANKS)"
else
  fail "TG blank lines ($TG_MAX_BLANKS) > CLI pane ($PANE_MAX_BLANKS)"
fi

# Test 5: TG has code formatting (<code> or <pre> in HTML)
if grep -q "<code>" "$TG_HTML_FILE" 2>/dev/null || grep -q "<pre>" "$TG_HTML_FILE" 2>/dev/null; then
  pass "TG notification has code formatting"
else
  fail "TG notification missing code formatting"
fi

# Test 6: Both CLI and TG contain ordered list content; TG rich HTML has <ol> tag
set +eo pipefail
echo "$PANE_RAW" | grep -q "Step one" 2>/dev/null
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ] && grep -q "Step one" "$TG_PLAIN_FILE" 2>/dev/null && grep -q "<ol>" "$TG_HTML_FILE" 2>/dev/null; then
  pass "Both CLI and TG contain ordered list content, TG HTML has <ol> tag"
else
  fail "Ordered list content mismatch between CLI and TG, or TG HTML missing <ol>"
fi

# Test 7: No text gluing — TG should not have "item•" or "one•" (text immediately before bullet).
# FIX 5 (r9): the extracted plain text includes the notification launch-command header (e.g.
# "... codex --model mimo-v2.5-free ..."); the model name's "v2." matches the letter-digit-dot gluing
# pattern and false-positives. Exclude the header line (grep -v -- "--model", covering both the codex
# and claude header shapes) into .glue, then run BOTH gluing patterns against .glue (never $TG_PLAIN_FILE).
# Ports the cc-sibling phase12:165-167 guard to codex phase6; layer A only, assertion semantics unchanged.
grep -v -- "--model" "$TG_PLAIN_FILE" > "$TG_PLAIN_FILE.glue" 2>/dev/null || true
if grep -q "[a-zA-Z]•" "$TG_PLAIN_FILE.glue" 2>/dev/null || grep -q "[a-zA-Z][0-9]\." "$TG_PLAIN_FILE.glue" 2>/dev/null; then
  fail "TG has text gluing (text immediately before bullet marker)"
else
  pass "TG has no text gluing issues"
fi

# Cleanup temp files
rm -f "$TG_HTML_FILE" "$TG_PLAIN_FILE" "$TG_PLAIN_FILE.glue"

wait_for_idle
pane_log "[format_render] AFTER CLI idle"

# === Tab expansion test (separate prompt to avoid breaking format tests above) ===
LOG_BEFORE_TAB=$(wc -l < "$LOG_FILE")
pane_log "[format_render] BEFORE tab prompt"

inject_prompt 'Without using any tools, output EXACTLY this text with no modifications:

```go
func main() {
'"$(printf '\t')"'fmt.Println("hello")
'"$(printf '\t')"'if true {
'"$(printf '\t\t')"'fmt.Println("nested")
'"$(printf '\t')"'}
}
```'

pane_log "[format_render] AFTER tab prompt"

ELAPSED=0
TAB_STOP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_TAB + 1))" "$LOG_FILE" | grep "Notification sent.*Stop" > /dev/null 2>&1; then
    TAB_STOP_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for tab test Stop notification... ${ELAPSED}s / ${TIMEOUT}s"
done

if [ "$TAB_STOP_FOUND" = true ]; then
  TAB_NEW_LOGS=$(tail -n +"$((LOG_BEFORE_TAB + 1))" "$LOG_FILE")
  TAB_HTML_FILE="/tmp/tg-cli-e2e-tab-html.txt"
  echo "$TAB_NEW_LOGS" | awk '/TG message sent \[Stop\].*full_text:/{found=1; sub(/.*full_text:/, ""); print; next} found && /^\[[0-9]{4}-/{exit} found{print}' > "$TAB_HTML_FILE"
  if grep -q "<pre>" "$TAB_HTML_FILE" 2>/dev/null; then
    PRE_CONTENT=$(awk '/<pre>/{found=1} found{print} /<\/pre>/{found=0}' "$TAB_HTML_FILE")
    echo "  DEBUG: PRE_CONTENT (${#PRE_CONTENT} chars): $PRE_CONTENT"
    set +eo pipefail
    echo "$PRE_CONTENT" | grep -qP '\t'
    _ps=("${PIPESTATUS[@]}")
    set -eo pipefail
    if [ "${_ps[1]}" -eq 0 ]; then
      fail "TG code block still contains literal tab characters"
    else
      pass "TG code block tabs expanded to spaces"
    fi
  else
    fail "TG tab test: no <pre> block found"
  fi
  rm -f "$TAB_HTML_FILE"
else
  fail "Tab test Stop notification not received within ${TIMEOUT}s"
fi

wait_for_idle
pane_log "[format_render] AFTER tab test CLI idle"
