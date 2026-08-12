#!/bin/bash
# Phase 17 = Round-4 Item 5 (boss ruling: "可以，如果是网络问题重试的这种，可以加豁免" — exempt ONLY the
# network/error-caused retry), REVISITED under Item 7. pi re-runs the agent inside ONE user turn for THREE
# distinct reasons, only one of which is an error: (1) auto-retry after a retryable provider error, (2) a queued
# continuation, (3) overflow-compaction re-running the aborted turn. A second agent_start alone would exempt all
# three (pi fires agent_start per prompt AND per continue), which is WIDER than the boss authorised.
#
# Item 7 adds a dedicated pi-extension signal, agent_retry, POSTed at the retry's agent_start ONLY when the
# previous run of the SAME turn ended stopReason=error (cmd/config/pi-extension.ts). That single bot.log line is
# turn-scoped ("turn_id":"tN") AND error-specific — it carries the whole "network/error-caused retry" fact. So the
# V3 exemption now keys DIRECTLY on agent_retry (validate_phase_log in e2e_common.sh); the old two-leg conjunction
# (a: a second agent_start; b: a jq transcript stopReason=error cross-check) is REMOVED (Occam — two mechanisms
# for the same fact = duplicate state). A flagged same-turn pair WITHOUT an agent_retry line stays a real
# violation, so a NON-error re-run (a second agent_start but no agent_retry) is NOT exempted.
#
# cc stays FULLY sensitive: agent_retry (like agent_start) is pi-extension only — cmd/config/pi-extension.ts posts
# it, register.go receives it; cc emits neither, so retry[] is never set and the exemption never engages. The
# CC-SHAPE sub-test proves it.
#
# FULLY SYNTHETIC + DETERMINISTIC — no bot, no live pi. It builds bot-log FIXTURES and drives the real
# validate_phase_log (e2e_common.sh) against them in a subshell, asserting its pass/FAIL verdict directly.
#   POSITIVE (retry): flagged same-turn pair WITH an agent_retry line for that turn -> EXEMPT (validate passes,
#             prints the retry-after-error NOTE).
#   NEGATIVE (rerun): same flagged pair, a SECOND agent_start present but NO agent_retry -> V3 STILL FAILs (proves
#             the exemption keys on the error-specific agent_retry, not on any same-turn re-run). This is the point.
#   COMPACT-SHAPE (compact): the context-overflow re-run shape — a session_compact line AND a second agent_start but
#             NO agent_retry -> V3 STILL FAILs, not exempted. pi re-runs the SAME turn after compacting an overflow
#             (which arrives as stopReason=error), and the extension's session_compact discriminator suppresses
#             agent_retry for it, so bot.log carries no agent_retry line. This proves V3 keys on agent_retry ALONE:
#             a session_compact line is neither a separator nor an exemption, so a compaction re-run is never
#             exempted. (Go does not currently forward session_compact; the line is a DEFENSIVE fixture.)
#   CC-SHAPE:  flagged pair with NO agent_start and NO agent_retry -> plain V3 violation, no exemption (cc guard).
# RED (pre-Item-7 validator, keyed on a second agent_start + transcript): POSITIVE built with only an agent_retry
# line FAILs (no second agent_start). GREEN (post-change): POSITIVE passes on the agent_retry line alone.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- pi Item5/Item7: V3 exemption for pi retry-after-error keys on the agent_retry signal (synthetic) ---"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/phase17-v3-XXXXXX")
cleanup17() { rm -rf "$WORK" 2>/dev/null || true; }
trap cleanup17 EXIT

# make_botlog <out> <mode: retry|rerun|compact|cc>
# Emits a phase slice: warmup turn t1, then turn t2 with two adjacent Message sends (message_id=2 -> 3) and NO
# separator between them (the flagged pair). Between the two t2 sends, mode controls the re-run marker:
#   retry   -> a Raw hook payload [agent_retry] line for t2 (Item 7 signal, error-specific)  -> exempt
#   rerun   -> a SECOND Raw hook payload [agent_start] for t2 but NO agent_retry (non-error re-run) -> still a violation
#   compact -> a Raw hook payload [session_compact] line AND a second [agent_start] for t2, but NO agent_retry
#              (the overflow-compaction re-run shape) -> still a violation (session_compact is not an exemption)
#   cc      -> neither (no agent_start / no agent_retry at all)                               -> plain violation
make_botlog() {
  local out="$1" mode="$2"
  {
    if [ "$mode" != "cc" ]; then
      echo '[2026-08-11T10:00:00+08:00] [PID=99001] [INFO] Raw hook payload [agent_start]: {"hook_event_name":"agent_start","session_id":"synthv3","backend":"pi","tmux_target":"%99@synthv3","prompt_id":"p1","turn_id":"t1"}'
    fi
    echo '[2026-08-11T10:00:01+08:00] [PID=99001] [DEBUG] Stream send: msg_id=99001 message_id=1 turn_id=t1 chunk=0 fmt=rich full_text: warmup reply text'
    if [ "$mode" != "cc" ]; then
      echo '[2026-08-11T10:00:02+08:00] [PID=99001] [INFO] Raw hook payload [agent_start]: {"hook_event_name":"agent_start","session_id":"synthv3","backend":"pi","tmux_target":"%99@synthv3","prompt_id":"p2","turn_id":"t2"}'
    fi
    echo '[2026-08-11T10:00:03+08:00] [PID=99001] [DEBUG] Stream send: msg_id=99002 message_id=2 turn_id=t2 chunk=0 fmt=rich full_text: first essay attempt about the color blue'
    if [ "$mode" = "retry" ]; then
      echo '[2026-08-11T10:00:04+08:00] [PID=99001] [INFO] Raw hook payload [agent_retry]: {"hook_event_name":"agent_retry","session_id":"synthv3","backend":"pi","tmux_target":"%99@synthv3","prompt_id":"p2","turn_id":"t2","error_message":"upstream stream truncated: ended without finish_reason"}'
    elif [ "$mode" = "rerun" ]; then
      echo '[2026-08-11T10:00:04+08:00] [PID=99001] [INFO] Raw hook payload [agent_start]: {"hook_event_name":"agent_start","session_id":"synthv3","backend":"pi","tmux_target":"%99@synthv3","prompt_id":"p2","turn_id":"t2"}'
    elif [ "$mode" = "compact" ]; then
      # The overflow-compaction re-run shape: pi emits session_compact THEN re-runs the same turn (a 2nd agent_start),
      # and the extension SUPPRESSES agent_retry for it, so no agent_retry line appears. The session_compact line must
      # NOT be read by V3 as a separator or exemption -> the two sends stay adjacent -> still a violation.
      echo '[2026-08-11T10:00:04+08:00] [PID=99001] [INFO] Raw hook payload [session_compact]: {"hook_event_name":"session_compact","session_id":"synthv3","backend":"pi","tmux_target":"%99@synthv3","prompt_id":"p2","turn_id":"t2","reason":"overflow"}'
      echo '[2026-08-11T10:00:04+08:00] [PID=99001] [INFO] Raw hook payload [agent_start]: {"hook_event_name":"agent_start","session_id":"synthv3","backend":"pi","tmux_target":"%99@synthv3","prompt_id":"p2","turn_id":"t2"}'
    fi
    echo '[2026-08-11T10:00:05+08:00] [PID=99001] [DEBUG] Stream send: msg_id=99003 message_id=3 turn_id=t2 chunk=0 fmt=rich full_text: retried essay about the color blue'
  } > "$out"
}

# Drive the REAL validate_phase_log in a subshell: LOG_FILE=fixture, log_before=0, no pane. `fail` (V3 violation)
# exits the subshell non-zero; a clean pass returns 0. A scratch E2E_RESULTS_FILE keeps the driven run's pass/FAIL
# lines out of this phase's own results. Returns the validator's exit; stdout captured by the caller.
run_validator() {
  (
    LOG_FILE="$1"
    E2E_RESULTS_FILE="$WORK/scratch-results.txt"
    E2E_PANE=""
    validate_phase_log "phase17-v3" 0 ""
  )
}

POS_LOG="$WORK/bot-positive.log"; make_botlog "$POS_LOG" retry
NEG_LOG="$WORK/bot-negative.log"; make_botlog "$NEG_LOG" rerun
CMP_LOG="$WORK/bot-compact.log";  make_botlog "$CMP_LOG"  compact
CC_LOG="$WORK/bot-ccshape.log";   make_botlog "$CC_LOG"  cc

# --- POSITIVE: an agent_retry line for the flagged turn -> EXEMPT (turn-scoped + error-specific, single mechanism) ---
if pos_out=$(run_validator "$POS_LOG" 2>&1); then pos_rc=0; else pos_rc=$?; fi
echo "  DEBUG positive rc=$pos_rc"; printf '%s\n' "$pos_out" | sed 's/^/    /'
if [ "$pos_rc" -eq 0 ] && printf '%s\n' "$pos_out" | grep -q "V3 exemption - pi retry-after-error"; then
  pass "phase17 POSITIVE: retry-after-error exemption applied (validate passed + agent_retry NOTE printed)"
else
  record_fail "phase17 POSITIVE: expected V3 exemption (pass + retry-after-error NOTE), got rc=$pos_rc"
fi

# --- NEGATIVE: a second agent_start but NO agent_retry -> V3 STILL FAILs (a non-error re-run is not exempted) ---
if neg_out=$(run_validator "$NEG_LOG" 2>&1); then neg_rc=0; else neg_rc=$?; fi
echo "  DEBUG negative rc=$neg_rc"; printf '%s\n' "$neg_out" | sed 's/^/    /'
if [ "$neg_rc" -ne 0 ] \
  && printf '%s\n' "$neg_out" | grep -q "consecutive Message sends in same turn_id=t2" \
  && ! printf '%s\n' "$neg_out" | grep -q "V3 exemption"; then
  pass "phase17 NEGATIVE: a second agent_start without agent_retry does NOT exempt — V3 still FAILs"
else
  record_fail "phase17 NEGATIVE: expected V3 FAIL with no exemption (agent_retry absent), got rc=$neg_rc"
fi

# --- COMPACT-SHAPE: a session_compact line + a second agent_start but NO agent_retry -> V3 STILL FAILs (the
#     overflow-compaction re-run is never exempted; session_compact is neither a separator nor an exemption) ---
if cmp_out=$(run_validator "$CMP_LOG" 2>&1); then cmp_rc=0; else cmp_rc=$?; fi
echo "  DEBUG compact-shape rc=$cmp_rc"; printf '%s\n' "$cmp_out" | sed 's/^/    /'
if [ "$cmp_rc" -ne 0 ] \
  && printf '%s\n' "$cmp_out" | grep -q "consecutive Message sends in same turn_id=t2" \
  && ! printf '%s\n' "$cmp_out" | grep -q "V3 exemption"; then
  pass "phase17 COMPACT-SHAPE: a compaction re-run (session_compact + 2nd agent_start, no agent_retry) stays a V3 violation — not exempted"
else
  record_fail "phase17 COMPACT-SHAPE: expected V3 FAIL with no exemption (compaction re-run, agent_retry absent), got rc=$cmp_rc"
fi

# --- CC-SHAPE: no agent_start / no agent_retry -> plain V3 violation, no exemption (cc-sensitivity guard) ---
if cc_out=$(run_validator "$CC_LOG" 2>&1); then cc_rc=0; else cc_rc=$?; fi
echo "  DEBUG cc-shape rc=$cc_rc"; printf '%s\n' "$cc_out" | sed 's/^/    /'
if [ "$cc_rc" -ne 0 ] \
  && printf '%s\n' "$cc_out" | grep -q "consecutive Message sends in same turn_id=t2" \
  && ! printf '%s\n' "$cc_out" | grep -q "V3 exemption"; then
  pass "phase17 CC-SHAPE: no agent_retry -> plain V3 violation, exemption never engages (cc stays sensitive)"
else
  record_fail "phase17 CC-SHAPE: expected a plain V3 violation with no exemption, got rc=$cc_rc"
fi

echo ""
echo "--- pi Item5/Item7 V3 retry-after-error exemption phase complete ---"
