#!/bin/bash
# Phase 9 = SPEC (i): a prompt forcing >=1 tool call. Assert (1) busy stays busy across ALL rounds until
# agent_settled (no mid-run idle: exactly ONE Stop->SetIdle for the whole multi-round run — turn_end maps
# to nothing); (2) an inject queued mid-run is delivered only after the run settles; (3) a stream RENDER to
# Telegram occurs BEFORE Stop (live streaming across the tool rounds). Run A covers (1)+(3); Run B covers (2).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

queue_pending() {
  curl -s "http://127.0.0.1:$TEST_PORT/inject/queue-status" 2>/dev/null \
    | python3 -c "import sys,json; d=json.load(sys.stdin); print(sum(d.get('queues',{}).values()))" 2>/dev/null || echo 0
}

echo ""
echo "--- pi (i) multi-round tool run ---"

ensure_infrastructure
start_pi "e2e-pi-9"

# ---------- Run A: busy across rounds (no mid-run idle) + render before Stop + tool fired ----------
TYPING_A_BEFORE=$(wc -l < "$TYPING_LOG_FILE" 2>/dev/null || echo 0)
LOG_A_BEFORE=$(wc -l < "$LOG_FILE")
pane_log "[pi/tool] Run A before inject"
# The post-tool text must (1) span longer than the 1000ms render throttle (cmd/stream.go:347) AND (2) under
# Round 3 contain a paragraph boundary (\n\n) mid-reply so a live Stream render fires BEFORE Stop — the ticker
# now aligns every send/edit to the last \n\n, so a break-less reply renders ONLY at the Stop flush by design.
# Ask for a long post-tool explanation (~12 sentences) written as several short paragraphs separated by blank
# lines (=> at least one mid-reply \n\n boundary before Stop).
inject_prompt "Run the bash command: echo PI_TOOL_MARKER_I9. After it finishes, write a long, detailed explanation of at least twelve full sentences describing what the command did, what its output means, how the echo builtin works in a shell, and why the output appeared exactly as it did. Write it as several short paragraphs, and separate every paragraph from the next with a blank line. Write in complete sentences. Do not ask for confirmation."

# Observe busy during the run.
BUSY_SEEN=false
for i in $(seq 1 40); do
  st=$(check_session_idle "$E2E_PANE")
  if [ "$st" = "busy" ]; then BUSY_SEEN=true; break; fi
  sleep 1
done
[ "$BUSY_SEEN" = true ] && pass "pi (i): session BUSY during the tool run" || fail "pi (i): never observed busy during the tool run"

wait_for_idle "$TIMEOUT" "$E2E_PANE"
SLICE_A=$(tail -n +"$((LOG_A_BEFORE + 1))" "$LOG_FILE")
TSLICE_A=$(tail -n +"$((TYPING_A_BEFORE + 1))" "$TYPING_LOG_FILE" 2>/dev/null || true)

# (i.1) a tool round actually happened: PreToolUse fired and the tool command output landed in the log.
set +eo pipefail
printf '%s\n' "$SLICE_A" | grep -q "Raw hook payload \[PreToolUse\]:"
_ps_pre=("${PIPESTATUS[@]}")
printf '%s\n' "$SLICE_A" | grep -q "PI_TOOL_MARKER_I9"
_ps_mark=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps_pre[1]}" -eq 0 ] && [ "${_ps_mark[1]}" -eq 0 ]; then
  pass "pi (i): tool round occurred (PreToolUse fired + tool output PI_TOOL_MARKER_I9 present)"
else
  fail "pi (i): tool round not observed (PreToolUse=${_ps_pre[1]} marker=${_ps_mark[1]})"
fi

# (i.2) NO mid-run idle: the multi-round run settles exactly ONCE (a single Stop->state=idle). If turn_end
# were wrongly mapped to Stop, each round would settle -> multiple idle lines.
IDLE_COUNT=$(printf '%s\n' "$TSLICE_A" | grep -c "state: event=Stop .*state=idle" || true)
echo "  DEBUG: Run A Stop->idle transitions = $IDLE_COUNT"
if [ "$IDLE_COUNT" -eq 1 ]; then
  pass "pi (i): exactly ONE Stop->idle for the whole multi-round run (no mid-run idle)"
else
  fail "pi (i): expected 1 Stop->idle for the run, got $IDLE_COUNT (turn_end mis-mapped to Stop?)"
fi

# STIMULUS-VALIDITY GUARD (Round 3 fix, boss-approved) — same rationale as phase3. (i.3) render-before-Stop
# is only testable when the post-tool reply carried a \n\n boundary; Round 3 aligns ticker sends to \n\n, so a
# break-less reply renders ONLY at Stop by design. Count \n\n in the Run A Stop payload last_assistant_message
# (cmd/hooks/register.go:76); zero => stimulus invalid, fail with that explicit cause not "no live render".
PARA_BREAKS_A=$(printf '%s\n' "$SLICE_A" | python3 -c '
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
echo "  DEBUG: Run A delivered paragraph breaks in Stop payload(s) = $PARA_BREAKS_A"
if [ "${PARA_BREAKS_A:-0}" -lt 1 ]; then
  fail "pi (i): stimulus invalid — the model produced no paragraph break, so the render-before-Stop predicate is untestable on this input"
fi
pass "pi (i): stimulus valid — $PARA_BREAKS_A paragraph break(s) in Run A delivered text (render-before-Stop is testable)"

# (i.3) live render before Stop across the tool rounds. Here-strings avoid the awk-exit SIGPIPE (see phase3).
FIRST_STREAM=$(awk '/Stream (send|edit):/{print NR; exit}' <<< "$SLICE_A")
FIRST_STOP=$(awk '/Raw hook payload \[Stop\]:/{print NR; exit}' <<< "$SLICE_A")
echo "  DEBUG: Run A first Stream=$FIRST_STREAM first Stop=$FIRST_STOP"
if [ -n "$FIRST_STREAM" ] && [ -n "$FIRST_STOP" ] && [ "$FIRST_STREAM" -lt "$FIRST_STOP" ]; then
  pass "pi (i): live render before Stop across the tool rounds (stream $FIRST_STREAM < Stop $FIRST_STOP)"
else
  fail "pi (i): no live render before Stop (stream=$FIRST_STREAM stop=$FIRST_STOP)"
fi
set +eo pipefail
printf '%s\n' "$SLICE_A" | grep -q "Stop terminal: outcome=direct_send"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps[1]}" -ne 0 ] && pass "pi (i): no Stop direct_send on the happy path" || fail "pi (i): Stop direct_send present (streaming did not finalize)"

# ---------- Run B: an inject queued mid-run is delivered only after settle ----------
pane_log "[pi/tool] Run B before inject"
inject_prompt "Run the bash command: echo PI_TOOL_MARKER_I9B && sleep 4. Then write two sentences about what it did. Do not ask for confirmation."
# Catch busy, then inject a SECOND prompt mid-run.
BUSY_SEEN_B=false
for i in $(seq 1 40); do
  st=$(check_session_idle "$E2E_PANE")
  if [ "$st" = "busy" ]; then BUSY_SEEN_B=true; break; fi
  sleep 1
done
[ "$BUSY_SEEN_B" = true ] || fail "pi (i): Run B never went busy (cannot test mid-run queueing)"
inject_prompt "Reply with exactly: pi_queued_marker_i9. Do not use any tools."
sleep 1
PENDING_MID=$(queue_pending)
echo "  DEBUG: queue pending mid-run = $PENDING_MID"
if [ "$PENDING_MID" -ge 1 ]; then
  pass "pi (i): mid-run inject was QUEUED (pending=$PENDING_MID), not delivered immediately"
else
  fail "pi (i): mid-run inject not queued (pending=$PENDING_MID) — delivered mid-run?"
fi

# After the run settles, the queue flushes and the queued prompt is delivered/run.
wait_for_idle "$TIMEOUT" "$E2E_PANE"
sleep 3
PENDING_AFTER=$(queue_pending)
echo "  DEBUG: queue pending after settle = $PENDING_AFTER"
if [ "$PENDING_AFTER" -eq 0 ]; then
  pass "pi (i): inject queue drained after settle (queued prompt delivered only after the run settled)"
else
  fail "pi (i): inject queue still has $PENDING_AFTER pending after settle"
fi

pane_log "[pi/tool] end"
echo "  pi (i) multi-round tool test complete."
