#!/bin/bash
# Phase 3 = SPEC (c): assert the assistant text is delivered to Telegram AND rendered LIVE during the run,
# not lumped at Stop. Assert a RENDER observable (Stream send/edit, cmd/stream.go:57/63) occurs BEFORE the
# Stop hook is processed, and NO "Stop terminal: outcome=direct_send" on the happy path. Append-only bot log
# => line order == time order. The render-before-Stop check is the SOLE cover for the index-mapping bug
# (a mis-mapped index=contentIndex would render only at Stop). Bot runs with --debug (start_bot) so the
# Debug Stream lines are emitted.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi (c) live streaming render before Stop ---"

ensure_infrastructure
start_pi "e2e-pi-3"

LOG_BEFORE=$(wc -l < "$LOG_FILE")
pane_log "[pi/stream] before inject"
# The render loop throttle is 1000ms (cmd/stream.go:347): the first Stream render fires ~1s after the first
# delta. For render-before-Stop to be observable the reply must (1) span WELL BEYOND that throttle AND (2)
# under Round 3 contain a paragraph boundary (\n\n) mid-reply — the ticker flush now aligns every send/edit
# to the last \n\n boundary, so a break-less reply renders ONLY at the Stop flush by design. So ask for a
# long answer (~15 sentences => several seconds of streaming) written as several short paragraphs separated
# by blank lines (=> at least one mid-reply \n\n boundary before Stop).
inject_prompt "Write a long, detailed essay of at least fifteen full sentences about the color blue: its physical wavelength, its cultural symbolism across different societies, its use in art and design, and its psychological effects on people. Write it as several short paragraphs, one per aspect, and separate every paragraph from the next with a blank line. Write in complete sentences. Do not use any tools."
wait_for_idle "$TIMEOUT" "$E2E_PANE"
pane_log "[pi/stream] after settle"

SLICE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE")

# (c.1) assistant text was delivered via a stream render (Stream send/edit present).
set +eo pipefail
printf '%s\n' "$SLICE" | grep -qE "Stream (send|edit):"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (c): assistant text rendered to Telegram (Stream send/edit present)"
else
  fail "pi (c): no Stream send/edit render line — text not streamed to Telegram"
fi

# STIMULUS-VALIDITY GUARD (Round 3 fix, boss-approved). The (c.2) render-before-Stop predicate is only
# meaningful when the model actually emitted a paragraph boundary (\n\n) in the reply: Round 3 aligns every
# ticker send/edit to the last \n\n, so a break-less reply renders ONLY at Stop BY DESIGN and would fail
# (c.2) for a reason unrelated to index mapping. Count \n\n in the delivered assistant text (Stop payload
# last_assistant_message, cmd/hooks/register.go:76). Zero breaks => the stimulus, not the product, is at
# fault: fail with that explicit cause instead of the misleading "index-mapping regression?" message below.
PARA_BREAKS=$(printf '%s\n' "$SLICE" | python3 -c '
import sys, json
total = 0
marker = "Raw hook payload [Stop]: "
for line in sys.stdin:
    i = line.find(marker)
    if i < 0:
        continue
    try:
        payload = json.loads(line[i+len(marker):])
    except Exception:
        continue
    total += (payload.get("last_assistant_message") or "").count("\n\n")
print(total)
')
echo "  DEBUG: delivered paragraph breaks in Stop payload(s) = $PARA_BREAKS"
if [ "${PARA_BREAKS:-0}" -lt 1 ]; then
  fail "pi (c): stimulus invalid — the model produced no paragraph break, so the render-before-Stop predicate is untestable on this input"
fi
pass "pi (c): stimulus valid — $PARA_BREAKS paragraph break(s) in delivered text (render-before-Stop is testable)"

# (c.2) RENDER-BEFORE-STOP: first Stream send/edit at an earlier log line than the first Stop payload.
# Here-strings (not printf|awk): awk's early `exit` would SIGPIPE a printf writer under set -o pipefail.
FIRST_STREAM=$(awk '/Stream (send|edit):/{print NR; exit}' <<< "$SLICE")
FIRST_STOP=$(awk '/Raw hook payload \[Stop\]:/{print NR; exit}' <<< "$SLICE")
echo "  DEBUG: first Stream line=$FIRST_STREAM  first Stop line=$FIRST_STOP"
if [ -n "$FIRST_STREAM" ] && [ -n "$FIRST_STOP" ] && [ "$FIRST_STREAM" -lt "$FIRST_STOP" ]; then
  pass "pi (c): live render occurred BEFORE Stop (stream line $FIRST_STREAM < Stop line $FIRST_STOP)"
else
  fail "pi (c): no live render before Stop (stream=$FIRST_STREAM stop=$FIRST_STOP) — text lumped at Stop (index-mapping regression?)"
fi

# (c.3) happy path: NO direct_send terminal outcome (the streamed entry finalizes in place).
set +eo pipefail
printf '%s\n' "$SLICE" | grep -q "Stop terminal: outcome=direct_send"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -ne 0 ]; then
  pass "pi (c): no Stop direct_send on happy path (streamed entry finalized in place)"
else
  fail "pi (c): Stop terminal outcome=direct_send present (streaming did not finalize the entry)"
fi

pane_log "[pi/stream] end"
echo "  pi (c) streaming test complete."
