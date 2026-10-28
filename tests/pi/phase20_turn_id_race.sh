#!/bin/bash
# Phase 20 = rounds/3 PLAN.md Addendum 3 Point 3 + Addendum 4 Points 1 and 3 + Addendum 5 Point 1: the pi
# extension's turn_id race (fast-tool continuation mislabeled with a concurrently-steered prompt's turn_id).
# This file is the regression test for the fix that landed in round 3 (task 18): Addendum 4's text-matching
# pendingQueued design in cmd/config/pi-extension.ts — NOT Addendum 3's index-only design, in which Addendum 4
# found a stale-reservation bug. With the fix in place both sub-tests are expected to PASS; the file was written
# to discriminate all three extension variants per the table below, not merely pass/fail as a whole.
#
# Sub-test B (normal race, Addendum 3 Point 3 / Addendum 4 Point 3): a fast Bash tool call races a steered
# prompt injected while the tool runs. Asserts the ack message and the post-tool continuation message share
# the SAME turn_id (the bug mislabels the continuation with the steered prompt's turn_id instead), and the
# steered reply gets its own, later turn_id.
#
# Sub-test A (Addendum 5 Point 1, new): guards against the stale-reservation bug Addendum 4 found in
# Addendum 3's index-only design. A steered prompt is queued, then discarded via Escape BEFORE it is ever
# activated, then a direct prompt is sent. Asserts the direct prompt's reply carries its OWN turn_id, not
# the discarded reservation's.
#
# Expected results against the three variants (rounds/3 PLAN.md Addendum 5 Point 1 table):
#   Unfixed (original, no pendingQueued, unconditional overwrite): A PASS (nothing to mislabel — a
#     discarded steer never produces a MessageDisplay line at all) / B FAIL (the real, original race).
#   Index-only (Addendum 3, shift() on pendingQueued.length>0): A FAIL (the stale reservation survives the
#     ESC and gets incorrectly shift()ed onto the direct prompt) / B PASS.
#   Text-matching (Addendum 4, task 18): A PASS / B PASS.
#
# Messages are identified from the bot.log slice by LINE ORDER and FIELD MATCHING (never by assumed
# message_id/turn_id values) — see py_identify_b/py_identify_a below, which implement the exact procedure
# from Addendum 4 Point 3 / Addendum 5 Point 1.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

# f6_new_reset — copied verbatim from phase15_abort_not_completed.sh (not shared via pi_common.sh). F6-1
# /new same-pane reset: redo /new, re-fetch the new SID, reset the LOG anchor, rename the session.
# Args: $1 = target session name, $2 = fail extra suffix, $3 = echo extra suffix, $4 = rebuild flag
# ("rebuild" to redo a fixture copy — unused by this phase, no fixture file is needed).
# Globals written (MUST remain global): SID, SID_BEFORE, PI_SESSION_NAME, _F6_NEW_BEFORE,
# _F6_NEW_BEFORE_C, CAND, STILL, _f6_ok, _f6i.
f6_new_reset() {
  local _target="$1"
  local _fail_extra="${2:-}"
  local _echo_extra="${3:-}"
  local _rebuild="${4:-}"
  if [ "$(check_session_idle "$E2E_PANE")" = busy ]; then $TMUX_TEST send-keys -t "$E2E_SESSION" Escape; fi
  wait_for_idle "$TIMEOUT" "$E2E_PANE"
  _F6_NEW_BEFORE=$(wc -l < "$LOG_FILE")
  _F6_NEW_BEFORE_C="$_F6_NEW_BEFORE"
  SID_BEFORE="$SID"
  # Clear the editor first (mirrors tg-cli's own injector, internal/injector/injector.go:147-150: send
  # C-u, then sleep 500ms) -- note2 correction 2026-10-23 (harness bug found via phase995 RED run 474957):
  # pi's Escape puts the queued prompt's text back into the editor instead of discarding it, so without
  # this clear, "/new" gets typed and submitted appended onto that leftover text (e.g.
  # "...Do not use any tools./new") instead of running the /new command, and the F6 fallback silently
  # reuses SID_BEFORE for the "new" session -- contaminating every later F8 retry attempt with the prior
  # attempt's own session.
  $TMUX_TEST send-keys -t "$E2E_SESSION" C-u
  sleep 0.5
  $TMUX_TEST send-keys -t "$E2E_SESSION" "/new" Enter
  _f6_ok=false
  for _f6i in $(seq 1 30); do
    CAND=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 "$SCRIPT_DIR/session_list.py" cand "$E2E_PANE" "$SID_BEFORE" 2>/dev/null || echo "")
    if [ -n "$CAND" ] && [ "$CAND" != "$SID_BEFORE" ]; then SID="$CAND"; _f6_ok=true; break; fi
    sleep 1
  done
  if [ "$_f6_ok" != true ]; then
    STILL=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 "$SCRIPT_DIR/session_list.py" still "$E2E_PANE" 2>/dev/null || echo "")
    if [ -n "$STILL" ] && [ "$STILL" = "$SID_BEFORE" ]; then
      echo "  F6 /new: no new SID observed but pane still has SID_BEFORE=$SID_BEFORE — accepting fallback (same-pane, /new did not emit new SessionStart)"
      SID="$STILL"; _f6_ok=true
    fi
  fi
  [ "$_f6_ok" = true ] && [ -n "$SID" ] || fail "F6-1: /new did not produce any SID within 30s (SID_BEFORE=$SID_BEFORE pane=$E2E_PANE)${_fail_extra}"
  local _label="${_target##*-}"
  PI_SESSION_NAME="$_target"
  [ -n "$SID" ] && curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SID&name=$PI_SESSION_NAME" >/dev/null 2>&1 || true
  echo "  pi session id=$SID named=$PI_SESSION_NAME target=$E2E_PANE (F6 /new $_label fresh${_echo_extra}, LOG_BEFORE=$_F6_NEW_BEFORE)"
  if [ "$_rebuild" = "rebuild" ]; then
    : # no fixture file is used by this phase
  fi
}

# poll_busy_fast — echoes true/false; polls check_session_idle every 0.2s up to 150 iterations (30s).
poll_busy_fast() {
  local i
  for i in $(seq 1 150); do
    if [ "$(check_session_idle "$E2E_PANE")" = "busy" ]; then echo true; return; fi
    sleep 0.2
  done
  echo false
}

# wait_for_prompt_ups — $1=start-line $2=exact "prompt" field value $3=max-seconds ; echoes true/false.
# Two-stage grep (same convention as phase13:249/phase25:1104/phase15's wait_for_bash_cmd): narrow to
# UserPromptSubmit lines, then match the prompt field with -F (fixed-string) so the prompt text's own
# punctuation is never regex-interpreted.
wait_for_prompt_ups() {
  local start="$1" text="$2" max="$3" i
  for i in $(seq 1 "$max"); do
    if grep -qF "\"prompt\":\"$text\"" <<< "$(tail -n +"$((start + 1))" "$LOG_FILE" | grep "Raw hook payload \[UserPromptSubmit\]")"; then echo true; return; fi
    sleep 1
  done
  echo false
}

# jget — $1=json string $2=field key ; echoes the value (stringified) or empty for null/missing.
jget() {
  printf '%s' "$1" | python3 -c '
import sys, json
d = json.load(sys.stdin)
v = d.get(sys.argv[1])
print("" if v is None else v)
' "$2"
}

# jmap_single — $1=json string $2=field key (a {turn_id: count} map) ; echoes the sole key if the map
# has exactly one distinct key, else echoes empty string (boss ruling 2026-10-23 Q1=1: an assertion that
# "every delta carries the same turn_id" is true only when the map collapses to one key).
jmap_single() {
  printf '%s' "$1" | python3 -c '
import sys, json
d = json.load(sys.stdin)
m = d.get(sys.argv[1]) or {}
keys = list(m.keys())
print(keys[0] if len(keys) == 1 else "")
' "$2"
}

# jmap_all_gt — $1=json string $2=field key (a {turn_id: count} map) $3=base turn_id (e.g. "t5") ; echoes
# true if the map is non-empty and every key, compared numerically after stripping a leading "t", is
# greater than $3 (boss ruling 2026-10-23 Q1=1: "every steered-reply MessageDisplay carries a turn_id
# numerically later than the ack's").
jmap_all_gt() {
  printf '%s' "$1" | python3 -c '
import sys, json
d = json.load(sys.stdin)
m = d.get(sys.argv[1]) or {}
base = sys.argv[2]
def num(t): return int(t[1:]) if t.startswith("t") else int(t)
print("true" if m and all(num(k) > num(base) for k in m.keys()) else "false")
' "$2" "$3"
}

# py_identify_b — $1=bot.log slice text ; echoes a JSON object with the full sub-test B identification
# (Addendum 4 Point 3 procedure, redesigned per boss ruling 2026-10-23 Q1=1: every delta checked;
# precondition = queued UPS before the continuation's last MessageDisplay). L_ups/L_pre/L_post and the mode0 leg (l_arm/l_route, tied to L_pre by log order),
# ack/continuation message_id + the turn_id of EVERY MessageDisplay line for each of ack/continuation/
# steered as {turn_id: count} maps, the continuation's last MessageDisplay line (l_cont_last, the
# final:true seal counts), the second prompt's own UPS line (precondition leg b), and the steered-reply
# message_id (identified by joined delta text, trailing punctuation stripped).
py_identify_b() {
  printf '%s' "$1" | python3 -c '
import sys, json

FIRST_PROMPT = sys.argv[1]
SECOND_PROMPT = sys.argv[2]
STEERED_TEXT = sys.argv[3]
CMD_MARKER = sys.argv[4]

lines = sys.stdin.read().split("\n")

def extract(line, event):
    marker = "Raw hook payload [" + event + "]: "
    i = line.find(marker)
    if i < 0:
        return None
    try:
        return json.loads(line[i + len(marker):])
    except Exception:
        return None

def norm(t): return t.rstrip().rstrip(".,!?;:。，！？；：").rstrip()

result = {}

l_ups = None
for idx, line in enumerate(lines, start=1):
    d = extract(line, "UserPromptSubmit")
    if d is not None and d.get("prompt") == FIRST_PROMPT:
        l_ups = idx
        break
result["l_ups"] = l_ups

l_pre = None
tool_use_id = None
for idx, line in enumerate(lines, start=1):
    if "Raw hook payload [PreToolUse]" not in line or CMD_MARKER not in line:
        continue
    d = extract(line, "PreToolUse")
    if d is None:
        continue
    l_pre = idx
    tool_use_id = d.get("tool_use_id")
    break
result["l_pre"] = l_pre
result["tool_use_id"] = tool_use_id

l_post = None
if tool_use_id is not None:
    for idx, line in enumerate(lines, start=1):
        d = extract(line, "PostToolUse")
        if d is not None and d.get("tool_use_id") == tool_use_id:
            l_post = idx
            break
result["l_post"] = l_post

# mode0 (precond_a): tie the mode=0 routing line to l_pre by log order. The routing line carries no tool_use_id,
# but the window armed by the MD-final is signalled only by the FIRST PreToolUse/Stop raw payload after the
# arming line (InjectRouteStore.SignalEvent), so the routing line belongs to l_pre exactly when l_pre is the
# only PreToolUse/Stop raw payload between the arming line (l_arm) and the routing line (l_route).
l_arm = None
if l_pre is not None:
    for idx in range(l_pre - 1, 0, -1):
        if "routeInjectQueue: MD-final trigger armed routing window" in lines[idx - 1]:
            l_arm = idx
            break
result["l_arm"] = l_arm

l_route = None
if l_arm is not None:
    for idx in range(l_arm + 1, len(lines) + 1):
        if "routeInjectQueue: routing target=" in lines[idx - 1]:
            l_route = idx
            break
result["l_route"] = l_route

mode0 = False
if l_pre is not None and l_arm is not None and l_route is not None and l_route > l_pre:
    signal_lines = [idx for idx in range(l_arm + 1, l_route) if "Raw hook payload [PreToolUse]" in lines[idx - 1] or "Raw hook payload [Stop]" in lines[idx - 1]]
    mode0 = signal_lines == [l_pre] and "event=\"PreToolUse\" tool=\"Bash\" mode=0" in lines[l_route - 1]
result["mode0"] = mode0

ack_mid = None
if l_ups is not None and l_pre is not None:
    for idx, line in enumerate(lines, start=1):
        if idx <= l_ups or idx >= l_pre:
            continue
        d = extract(line, "MessageDisplay")
        if d is None:
            continue
        ack_mid = d.get("message_id")
        break
result["ack_message_id"] = ack_mid

cont_mid = None
if l_post is not None and ack_mid is not None:
    for idx, line in enumerate(lines, start=1):
        if idx <= l_post:
            continue
        d = extract(line, "MessageDisplay")
        if d is None:
            continue
        mid = d.get("message_id")
        if mid != ack_mid:
            cont_mid = mid
            break
result["continuation_message_id"] = cont_mid

groups = {}
last_idx_by_mid = {}
for idx, line in enumerate(lines, start=1):
    d = extract(line, "MessageDisplay")
    if d is None:
        continue
    mid = d.get("message_id")
    g = groups.setdefault(mid, {"deltas": {}, "turns": {}})
    g["deltas"][d.get("index")] = d.get("delta", "")
    turn = d.get("turn_id")
    g["turns"][turn] = g["turns"].get(turn, 0) + 1
    last_idx_by_mid[mid] = idx

l_cont_last = last_idx_by_mid.get(cont_mid)
result["l_cont_last"] = l_cont_last
result["ack_turn_ids"] = groups.get(ack_mid, {}).get("turns", {})
result["continuation_turn_ids"] = groups.get(cont_mid, {}).get("turns", {})

l_ups2 = None
for idx, line in enumerate(lines, start=1):
    d = extract(line, "UserPromptSubmit")
    if d is not None and d.get("prompt") == SECOND_PROMPT:
        l_ups2 = idx
        break
result["l_ups2"] = l_ups2

precond_b = bool(l_ups2 is not None and l_cont_last is not None and l_ups2 < l_cont_last)
result["precond_a"] = mode0
result["precond_b"] = precond_b
result["precond_met"] = bool(mode0 and precond_b)

steered_mid = None
for mid, g in groups.items():
    idxs = sorted(k for k in g["deltas"].keys() if isinstance(k, int))
    text = "".join(g["deltas"][k] for k in idxs)
    if norm(text) == STEERED_TEXT:
        steered_mid = mid
        break
result["steered_message_id"] = steered_mid
result["steered_turn_ids"] = groups.get(steered_mid, {}).get("turns", {})

print(json.dumps(result))
' "$PROMPT_1" "$PROMPT_2_B" "$STEERED_TEXT_B" "$CMD_MARKER"
}

# py_identify_a — $1=bot.log slice text ; echoes a JSON object with the full sub-test A identification
# (Addendum 5 Point 1 procedure): the queued prompt's own UPS turn_id, the direct prompt's own UPS
# turn_id, whether the queued turn_id ever appears as any MessageDisplay turn_id (precondition leg i /
# final assertion 2), whether any message's deltas equal the queued marker text (precondition leg ii), and
# the direct reply's message_id+turn_id (identified by joined delta text, trailing punctuation stripped).
py_identify_a() {
  printf '%s' "$1" | python3 -c '
import sys, json

QUEUED_PROMPT = sys.argv[1]
DIRECT_PROMPT = sys.argv[2]
QUEUED_MARKER = sys.argv[3]
DIRECT_MARKER = sys.argv[4]

lines = sys.stdin.read().split("\n")

def extract(line, event):
    marker = "Raw hook payload [" + event + "]: "
    i = line.find(marker)
    if i < 0:
        return None
    try:
        return json.loads(line[i + len(marker):])
    except Exception:
        return None

def norm(t): return t.rstrip().rstrip(".,!?;:。，！？；：").rstrip()

result = {}

l_ups_q = None
turn_q = None
for idx, line in enumerate(lines, start=1):
    d = extract(line, "UserPromptSubmit")
    if d is not None and d.get("prompt") == QUEUED_PROMPT:
        l_ups_q = idx
        turn_q = d.get("turn_id")
        break
result["l_ups_q"] = l_ups_q
result["turn_q"] = turn_q

l_ups_d = None
turn_d = None
for idx, line in enumerate(lines, start=1):
    d = extract(line, "UserPromptSubmit")
    if d is not None and d.get("prompt") == DIRECT_PROMPT:
        l_ups_d = idx
        turn_d = d.get("turn_id")
        break
result["l_ups_d"] = l_ups_d
result["turn_d"] = turn_d

groups = {}
md_turn_ids = []
for idx, line in enumerate(lines, start=1):
    d = extract(line, "MessageDisplay")
    if d is None:
        continue
    mid = d.get("message_id")
    g = groups.setdefault(mid, {"deltas": {}, "turn": d.get("turn_id")})
    g["deltas"][d.get("index")] = d.get("delta", "")
    md_turn_ids.append(d.get("turn_id"))

precond_i = bool(turn_q is not None and turn_q not in md_turn_ids)
result["precond_i"] = precond_i

precond_ii = True
for mid, g in groups.items():
    idxs = sorted(k for k in g["deltas"].keys() if isinstance(k, int))
    text = "".join(g["deltas"][k] for k in idxs)
    if norm(text) == QUEUED_MARKER:
        precond_ii = False
        break
result["precond_ii"] = precond_ii
result["precond_met"] = bool(turn_q is not None and precond_i and precond_ii)

direct_mid = None
direct_turn = None
for mid, g in groups.items():
    idxs = sorted(k for k in g["deltas"].keys() if isinstance(k, int))
    text = "".join(g["deltas"][k] for k in idxs)
    if norm(text) == DIRECT_MARKER:
        direct_mid = mid
        direct_turn = g["turn"]
        break
result["direct_message_id"] = direct_mid
result["direct_turn_id"] = direct_turn

print(json.dumps(result))
' "$PROMPT_Q" "$PROMPT_D" "$MARKER_Q" "$MARKER_D"
}

# Prompts/markers, shared verbatim across sub-tests B and A per the task spec.
PROMPT_1='Give at most one short sentence of acknowledgement, then immediately use the Bash tool to run this exact command as your very next tool action: echo race_marker_ready. After it finishes, write a long, detailed explanation of at least ten full sentences describing what the command did, what its output means, and how the echo builtin works in a shell. Write in complete sentences. Do not ask for confirmation.'
PROMPT_2_B='Reply with exactly: queued_after_fast_tool. Do not use any tools.'
STEERED_TEXT_B='queued_after_fast_tool'
CMD_MARKER='"command":"echo race_marker_ready"'
PROMPT_Q='Reply with exactly: queued_before_escape. Do not use any tools.'
PROMPT_D='Reply with exactly: direct_after_escape. Do not use any tools.'
MARKER_Q='queued_before_escape'
MARKER_D='direct_after_escape'

echo ""
echo "--- pi turn_id race (Addendum 3 Pt3 / Addendum 4 Pts 1+3 / Addendum 5 Pt1): sub-test B (normal race) + sub-test A (ESC-then-direct stale-reservation guard) ---"

ensure_infrastructure
start_pi "e2e-pi-20"

# Initial SID fetch (needed only so f6_new_reset's SID_BEFORE/CAND bookkeeping has a starting value on the
# first /new reset — this phase never names the session for cross-session messaging).
PI_SESSION_NAME="e2e-pi20"
SID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); sys.exit(0)
pane = sys.argv[1]
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane.split("@")[0] + "@"):
        print(s.get("id", "")); sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
[ -n "$SID" ] && curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SID&name=$PI_SESSION_NAME" >/dev/null 2>&1 || true
echo "  pi session id=$SID named=$PI_SESSION_NAME target=$E2E_PANE"

# =============================================================================
# Sub-test B: normal race (ack+fast-tool+long-continuation vs. a steered prompt injected mid-tool).
# =============================================================================
echo ""
echo "--- B: normal race ---"

PRECOND_B_MET=false
FINAL_RESULT_B=""
RESULT_B=""
for attempt_b in 1 2 3; do
  if [ "$attempt_b" -gt 1 ]; then
    echo "  F8 B retry attempt $attempt_b/3: redoing /new (previous attempt did not meet the precondition)"
    f6_new_reset "e2e-pi-20-B" " F8 B attempt=$attempt_b/3" " retry $attempt_b/3"
  fi
  LOG_B=$(wc -l < "$LOG_FILE")
  pane_log "[pi/turn_id_race-B] before inject attempt=$attempt_b/3"
  inject_prompt "$PROMPT_1"
  BUSY_B=$(poll_busy_fast)
  if [ "$BUSY_B" != true ]; then
    echo "  B attempt $attempt_b/3: never observed busy — retrying"
    continue
  fi
  inject_prompt "$PROMPT_2_B"
  wait_for_idle "$TIMEOUT" "$E2E_PANE"
  pane_log "[pi/turn_id_race-B] after settle attempt=$attempt_b/3"
  SLICE_B=$(tail -n +"$((LOG_B + 1))" "$LOG_FILE")
  RESULT_B=$(py_identify_b "$SLICE_B")
  echo "  DEBUG B attempt $attempt_b/3 identification: $RESULT_B"
  if [ "$(jget "$RESULT_B" precond_met)" = "True" ]; then
    PRECOND_B_MET=true
    FINAL_RESULT_B="$RESULT_B"
    echo "  B: precondition met on attempt $attempt_b/3 (precondition hit count report: $attempt_b/3 attempts needed; mode=0 tie in slice lines: arming line $(jget "$RESULT_B" l_arm), l_pre $(jget "$RESULT_B" l_pre), routing line $(jget "$RESULT_B" l_route); slice = bot.log after line $LOG_B)"
    break
  fi
  if [ "$attempt_b" -eq 3 ]; then
    fail "pi turn_id_race-B: precondition (mode=0 routing at the fast tool's own PreToolUse + queued-prompt UPS strictly before the continuation's LAST MessageDisplay) never held after 3 attempts — F8 hard FAIL. Last identification: $RESULT_B"
  fi
  echo "  B attempt $attempt_b/3: precondition NOT met (precond_a=$(jget "$RESULT_B" precond_a) precond_b=$(jget "$RESULT_B" precond_b)) — retrying"
done
[ "$PRECOND_B_MET" = true ] || fail "pi turn_id_race-B: precondition (mode=0 routing at the fast tool's own PreToolUse + queued-prompt UPS strictly before the continuation's LAST MessageDisplay) never held after 3 attempts — F8 hard FAIL. Last identification: $RESULT_B"

ACK_TURN_B=$(jmap_single "$FINAL_RESULT_B" ack_turn_ids)
ACK_TURN_IDS_B=$(jget "$FINAL_RESULT_B" ack_turn_ids)
CONT_TURN_B=$(jmap_single "$FINAL_RESULT_B" continuation_turn_ids)
CONT_TURN_IDS_B=$(jget "$FINAL_RESULT_B" continuation_turn_ids)
CONT_MID_B=$(jget "$FINAL_RESULT_B" continuation_message_id)
STEERED_MID_B=$(jget "$FINAL_RESULT_B" steered_message_id)
STEERED_TURN_IDS_B=$(jget "$FINAL_RESULT_B" steered_turn_ids)
echo "  DEBUG B final: ack_turn_ids=$ACK_TURN_IDS_B continuation_message_id=$CONT_MID_B continuation_turn_ids=$CONT_TURN_IDS_B steered_message_id=$STEERED_MID_B steered_turn_ids=$STEERED_TURN_IDS_B"

if [ -n "$ACK_TURN_B" ] && [ -n "$CONT_TURN_B" ] && [ "$ACK_TURN_B" = "$CONT_TURN_B" ]; then
  pass "pi turn_id_race-B: every ack MessageDisplay and every continuation MessageDisplay carries the SAME single turn_id ($ACK_TURN_B) — ack_turn_ids=$ACK_TURN_IDS_B continuation_turn_ids=$CONT_TURN_IDS_B — no delta of the continuation was mislabeled"
else
  record_fail "pi turn_id_race-B: the ack and continuation MessageDisplay lines do not all carry one single shared turn_id — ack_turn_ids=$ACK_TURN_IDS_B continuation_turn_ids=$CONT_TURN_IDS_B — at least one delta of the continuation was mislabeled with a different turn's id (the turn_id race)"
fi

if [ -n "$STEERED_MID_B" ] && [ -n "$CONT_MID_B" ] && [ "$STEERED_MID_B" != "$CONT_MID_B" ]; then
  pass "pi turn_id_race-B: the steered reply (message_id=$STEERED_MID_B, matched by joined delta text, trailing punctuation stripped) is a DIFFERENT message from the continuation (message_id=$CONT_MID_B)"
else
  record_fail "pi turn_id_race-B: the steered reply's message_id ($STEERED_MID_B) is not distinct from the continuation's message_id ($CONT_MID_B) — the steered reply's own exact-text identification failed or collapsed into the continuation"
fi

STEERED_ALL_LATER_B=$(jmap_all_gt "$FINAL_RESULT_B" steered_turn_ids "$ACK_TURN_B")
if [ -n "$ACK_TURN_B" ] && [ "$STEERED_ALL_LATER_B" = "true" ]; then
  pass "pi turn_id_race-B: every steered-reply MessageDisplay turn_id (steered_turn_ids=$STEERED_TURN_IDS_B) is numerically later than the ack's turn_id ($ACK_TURN_B)"
else
  record_fail "pi turn_id_race-B: not every steered-reply MessageDisplay turn_id is numerically later than the ack's turn_id ($ACK_TURN_B) — steered_turn_ids=$STEERED_TURN_IDS_B"
fi

# =============================================================================
# Sub-test A: ESC-then-direct-prompt stale-reservation guard (Addendum 5 Point 1).
# =============================================================================
echo ""
echo "--- A: ESC-then-direct-prompt stale-reservation guard ---"

f6_new_reset "e2e-pi-20-A"

PRECOND_A_MET=false
RESULT_A_PRE=""
LOG_A=0
for attempt_a in 1 2 3; do
  if [ "$attempt_a" -gt 1 ]; then
    echo "  F8 A retry attempt $attempt_a/3: redoing /new (previous attempt did not meet the precondition)"
    f6_new_reset "e2e-pi-20-A" " F8 A attempt=$attempt_a/3" " retry $attempt_a/3"
  fi
  LOG_A=$(wc -l < "$LOG_FILE")
  pane_log "[pi/turn_id_race-A] before inject attempt=$attempt_a/3"
  inject_prompt "$PROMPT_1"
  BUSY_A=$(poll_busy_fast)
  if [ "$BUSY_A" != true ]; then
    echo "  A attempt $attempt_a/3: never observed busy — retrying"
    continue
  fi
  inject_prompt "$PROMPT_Q"
  UPS_Q_SEEN=$(wait_for_prompt_ups "$LOG_A" "$PROMPT_Q" "$TIMEOUT")
  if [ "$UPS_Q_SEEN" != true ]; then
    echo "  A attempt $attempt_a/3: the queued prompt's own UserPromptSubmit never observed — retrying"
    continue
  fi
  pane_log "[pi/turn_id_race-A] queued prompt's own UPS seen, aborting via Escape attempt=$attempt_a/3"
  $TMUX_TEST send-keys -t "$E2E_SESSION" Escape
  wait_for_idle "$TIMEOUT" "$E2E_PANE"
  pane_log "[pi/turn_id_race-A] after ESC settle attempt=$attempt_a/3"
  SLICE_A_PRE=$(tail -n +"$((LOG_A + 1))" "$LOG_FILE")
  RESULT_A_PRE=$(py_identify_a "$SLICE_A_PRE")
  echo "  DEBUG A attempt $attempt_a/3 pre-direct identification: $RESULT_A_PRE"
  if [ "$(jget "$RESULT_A_PRE" precond_met)" != "True" ]; then
    echo "  A attempt $attempt_a/3: the queued prompt was NOT cleanly discarded (precond_i=$(jget "$RESULT_A_PRE" precond_i) precond_ii=$(jget "$RESULT_A_PRE" precond_ii)) — retrying"
    continue
  fi
  inject_prompt "$PROMPT_D"
  wait_for_idle "$TIMEOUT" "$E2E_PANE"
  pane_log "[pi/turn_id_race-A] after direct-prompt settle attempt=$attempt_a/3"
  PRECOND_A_MET=true
  echo "  A: precondition met on attempt $attempt_a/3 (precondition hit count report: $attempt_a/3 attempts needed)"
  break
done
if [ "$PRECOND_A_MET" != true ]; then
  fail "pi turn_id_race-A: precondition (the queued prompt's own reservation discarded by Escape before any message, never drained into any MessageDisplay) never held after 3 attempts — F8 hard FAIL. Last identification: $RESULT_A_PRE"
fi

SLICE_A=$(tail -n +"$((LOG_A + 1))" "$LOG_FILE")
RESULT_A=$(py_identify_a "$SLICE_A")
echo "  DEBUG A final identification: $RESULT_A"

TURN_Q=$(jget "$RESULT_A" turn_q)
TURN_D=$(jget "$RESULT_A" turn_d)
DIRECT_MID=$(jget "$RESULT_A" direct_message_id)
DIRECT_TURN=$(jget "$RESULT_A" direct_turn_id)
PRECOND_I_FINAL=$(jget "$RESULT_A" precond_i)
echo "  DEBUG A final: turn_q=$TURN_Q turn_d=$TURN_D direct_message_id=$DIRECT_MID direct_turn=$DIRECT_TURN precond_i_final=$PRECOND_I_FINAL"

if [ -n "$DIRECT_MID" ] && [ -n "$DIRECT_TURN" ] && [ -n "$TURN_D" ] && [ "$DIRECT_TURN" = "$TURN_D" ]; then
  pass "pi turn_id_race-A: the direct reply (message_id=$DIRECT_MID, matched by joined delta text, trailing punctuation stripped) carries turn_id=$DIRECT_TURN, EQUAL to the direct prompt's own UserPromptSubmit turn_id=$TURN_D"
else
  record_fail "pi turn_id_race-A: the direct reply's turn_id ($DIRECT_TURN, message_id=$DIRECT_MID) does not equal the direct prompt's own UserPromptSubmit turn_id ($TURN_D) — a stale reservation was incorrectly activated instead of the direct prompt's own ids"
fi

if [ "$PRECOND_I_FINAL" = "True" ]; then
  pass "pi turn_id_race-A: the discarded queued prompt's own turn_id ($TURN_Q) never appears as any MessageDisplay line's turn_id in the captured slice — the stale reservation was never consumed by anything"
else
  record_fail "pi turn_id_race-A: the discarded queued prompt's own turn_id ($TURN_Q) DOES appear as a MessageDisplay line's turn_id in the captured slice — the stale reservation WAS consumed by something"
fi

echo ""
echo "  pi turn_id_race (phase20) complete."
