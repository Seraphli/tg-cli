#!/bin/bash
# Phase 34 = Round-4 Item 1: the S6 pre-tool boundary-wait must NOT drain a POST-tool MessageDisplay into the
# pre-tool flush (which would render the post-tool bubble BEFORE the tool notification). Root cause (rounds/3
# note3-r3-phase27-v3-rootcause.md): the S6 lookahead `boundary` closure (register.go) did not treat PostToolUse
# as a boundary, so `extractEligibleBeforeBoundaryLocked` front-scanned PAST this tool's own PostToolUse and
# drained the following (post-tool) eligible MD, rendering it before the `🔧 Bash` tool notify. Fix: add
# PostToolUse to the boundary predicate so the front-scan stops there and a post-tool MD stays in the kept tail
# (renders AFTER the notify).
#
# Fully SYNTHETIC + DETERMINISTIC (no live model) — this reproduces phase27-V3's timing-gated mis-order as a
# hard, repeatable ordering assertion. Drive, on ONE session's Hook FIFO, all sharing prompt_id p_b34:
#   PreToolUse(Bash, tu34)  -> starts the S6 wait (budget 1500ms)
#   MD(mPRE, PREMD34, final) -> a LAGGING pre-tool MD (correctly drained + rendered BEFORE the notify: control)
#   PostToolUse(Bash, tu34)  -> the boundary the front-scan must stop at
#   MD(mPOST, POSTMD34, final) -> the POST-tool MD (must NOT be drained into the pre-tool flush)
# Observed via bot-log SEND ORDER (Message-FIFO worker logs at the moment of the actual Telegram send):
#   "Notification sent to chat ...: ToolUse ..."  (the tool notify)  vs
#   "Stream send/edit: ... message_id=mPOST ..."  (the post-tool MD render).
# ASSERT: the ToolUse notify line PRECEDES the mPOST render line.
#   RED (pre-fix, PostToolUse absent from boundary): the wait drains mPOST -> the pre-tool flush renders POSTMD34
#     BEFORE the ToolUse notify -> mPOST render line comes first -> FAIL.
#   GREEN (post-fix): the wait stops at PostToolUse -> notify first, mPOST renders later (ticker) -> PASS.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Round 4 Item 1: S6 pre-tool boundary — post-tool MD must render AFTER the tool notify (synthetic) ---"

ensure_infrastructure

CFG="$TEST_CONFIG_DIR/config.json"
CWD_NOW="$(pwd)"

# Snapshot config.json so our throttle/compact changes do not leak into later phases in a full suite run.
CFG_BACKUP="$TEST_CONFIG_DIR/config.json.phase34bak"
[ -f "$CFG" ] && command cp -f "$CFG" "$CFG_BACKUP" || true
restore_cfg() { [ -f "$CFG_BACKUP" ] && command cp -f "$CFG_BACKUP" "$CFG" && rm -f "$CFG_BACKUP" || rm -f "$CFG"; }
trap restore_cfg EXIT

post_hook() {
  curl -s -X POST "http://127.0.0.1:${TEST_PORT}/hook/$1" -H "Content-Type: application/json" -d "$2" >/dev/null 2>&1 || true
}

SID="ptbound-b34-$RANDOM"
TGT="%98@/tmp/tmux-1000/tg-cli-test-ptbound34"
PID="p_b34"; TID="t_b34"; TU="tu34"

# md: message_id delta final  (shared session/target/prompt so the S6 eligible/boundary prompt match applies)
md() {
  post_hook MessageDisplay "$(printf '{"session_id":"%s","tmux_target":"%s","cwd":"%s","project":"tg-cli","backend":"cc","hook_event_name":"MessageDisplay","message_id":"%s","turn_id":"%s","prompt_id":"%s","index":0,"delta":"%s","final":%s}' \
    "$SID" "$TGT" "$CWD_NOW" "$1" "$TID" "$PID" "$2" "$3")"
}

# Standard detailed tool-notify mode (compact=false -> a single clean "ToolUse" send) + a low stream throttle so
# the ticker renders the post-tool MD promptly. Atomic rewrite; the bot re-reads config.json per tick/hook.
CFG="$CFG" python3 - <<'PY'
import json, os
p = os.environ["CFG"]
try:
    c = json.load(open(p))
except Exception:
    c = {}
c["toolNotifyCompact"] = False
c["streamThrottleMs"] = 300
tmp = p + ".tmp"
json.dump(c, open(tmp, "w"))
os.replace(tmp, p)
PY

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# Every /hook/* POST is SYNCHRONOUS: register.go's handler runs DispatchWithMeta, which blocks until the queued
# job's handler completes (session_queue.go:164 `enqueueBlocking(...); return <-done`). The PreToolUse POST thus
# blocks for the whole S6 wait (~1.5s budget). The follow-up jobs MUST each be posted in SEPARATE CONCURRENT
# background curls so their ENQUEUE (enqueueBlocking appends on receipt, register.go:63) lands on the Hook FIFO
# WHILE the PreToolUse job is inside the wait — even though each follow-up curl then blocks on its own job. A
# SINGLE sequential background subshell does NOT work: the PostToolUse curl blocks until its job runs (after the
# wait), so a subsequent MD would post AFTER the deadline and never be drained.
# FIFO order behind PreToolUse: PostToolUse (enqueued ~0.3s) -> MD(post) (~0.6s), both inside the 1.5s window.
# NO pre-tool MD on purpose: a pre-tool MD drained by DrainAndRunMatching (step-b) makes n>0 and SKIPS the wait
# (gate register.go:567), masking the bug. With only PostToolUse->MD(post):
#   pre-fix (PostToolUse NOT a boundary): the wait's front-scan runs PAST PostToolUse and drains MD(post) -> the
#     pre-tool flush renders POSTMD34 BEFORE the ToolUse notify.
#   fixed (PostToolUse IS a boundary): the wait stops (boundary_floored) at PostToolUse, MD(post) is never drained
#     and renders (ticker / Stop-finalize) AFTER the notify.
( sleep 0.3; post_hook PostToolUse "{\"session_id\":\"$SID\",\"tmux_target\":\"$TGT\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"PostToolUse\",\"tool_name\":\"Bash\",\"tool_use_id\":\"$TU\",\"prompt_id\":\"$PID\",\"turn_id\":\"$TID\"}" ) &
( sleep 0.6; md "mPOST" "POSTMD34 post-tool text" false ) &
post_hook PreToolUse "{\"session_id\":\"$SID\",\"tmux_target\":\"$TGT\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"echo TOOLMARK34\"},\"tool_use_id\":\"$TU\",\"prompt_id\":\"$PID\",\"turn_id\":\"$TID\"}"
wait 2>/dev/null || true

# Let the pre-tool flush + tool notify send and PostToolUse processing settle, then Stop-finalize so mPOST
# renders deterministically (on the fixed binary it was never drained -> its bubble is finalized here).
sleep 3
post_hook Stop "{\"session_id\":\"$SID\",\"tmux_target\":\"$TGT\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"Stop\",\"last_assistant_message\":\"POSTMD34 post-tool text\"}"
sleep 3

SLICE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE")
echo "  DEBUG raw hook arrivals (b34, with timestamps):"
printf '%s\n' "$SLICE" | grep -E "Raw hook payload \[(PreToolUse|PostToolUse|MessageDisplay)\]" | grep -E "b34|mPOST|$SID" | sed -E 's/(\{.{40}).*/\1.../; s/^/    /' || true
echo "  DEBUG tool-boundary wait resolution:"
printf '%s\n' "$SLICE" | grep -E "tool-boundary wait done.*$PID" | sed 's/^/    /' || true
echo "  DEBUG send-order (ToolUse notify + mPOST Stream renders):"
printf '%s\n' "$SLICE" | grep -nE "Notification sent to chat.*: ToolUse |Stream (send|edit):.*message_id=mPOST" | sed 's/^/    /' || true

# First-occurrence line numbers (awk, not `grep|head` — avoids the SIGPIPE-under-pipefail footgun).
NOTIFY_LN=$(printf '%s\n' "$SLICE" | awk '/Notification sent to chat.*: ToolUse /{print NR; exit}')
POSTMD_LN=$(printf '%s\n' "$SLICE" | awk '/Stream (send|edit):.*message_id=mPOST/{print NR; exit}')
echo "  DEBUG line numbers: NOTIFY=$NOTIFY_LN POSTMD=$POSTMD_LN"

[ -n "$NOTIFY_LN" ] || fail "phase34: the ToolUse tool notification was never emitted (cannot test ordering)"
[ -n "$POSTMD_LN" ] || fail "phase34: the post-tool MD (message_id=mPOST) was never rendered (cannot test ordering)"

# DISCRIMINATING assert (RED pre-fix / GREEN post-fix): the post-tool MD renders AFTER the tool notify.
if [ "$NOTIFY_LN" -lt "$POSTMD_LN" ]; then
  pass "phase34: the tool notify PRECEDES the post-tool MD render (NOTIFY=$NOTIFY_LN < POSTMD=$POSTMD_LN) — PostToolUse boundary held"
else
  record_fail "phase34: the post-tool MD rendered BEFORE the tool notify (POSTMD=$POSTMD_LN <= NOTIFY=$NOTIFY_LN) — the S6 wait drained a post-tool MD into the pre-tool flush (Item 1 mis-order)"
fi

echo ""
echo "--- Round 4 Item 1 pre-tool boundary phase complete ---"
