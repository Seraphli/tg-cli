#!/bin/bash
# Phase 33 = Round-3 ticker paragraph-boundary flush alignment (boss ruling: send/edit only at a "\n\n"
# boundary). Fully SYNTHETIC + DETERMINISTIC (no live model): drive the streaming pipeline via /hook/MessageDisplay
# deltas with controlled "\n\n" structure + timed sleeps, a /hook/PreToolUse for the non-ticker pre-tool full
# flush (cb.FlushStreamOp, register.go:556 — reached for backend!=codex, tool!=AskUserQuestion), and /hook/Stop
# for the finalize render. Observed via the messages.db archive operations(op,text_len,content) rows in op_id
# order (Round-2 Item A precedent) + bot-log Stream send/edit ordering (phase3/phase28). Each sub-test uses a
# UNIQUE session_id/message_id so the Hook FIFOs do not cross-talk.
#
# Coverage (each RED-demonstrated against a distinct source revert — see rounds/3 report):
#   (a) a stream crossing several "\n\n" renders only up to a boundary at each intermediate edit
#       (RED: whole alignment block reverted — the ticker renders the full body incl. the not-yet-bounded tail).
#   (b) a stream with no "\n\n" produces no intermediate Stream send/edit before Stop and renders full at Stop
#       (RED: alignment block reverted — the ticker renders the partial as an intermediate edit).
#   (c) F5 — after a pre-tool flush renders a mid-paragraph FULL body, no later body for that message shrinks
#       (RED: F5 guard reverted — a shorter aligned body renders after the full one, text_len decreases).
#   (d) a pre-tool flush renders the FULL text incl. the mid-paragraph tail
#       (RED: mode-gate reverted — the pre-tool flush aligns and drops the tail).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Round 3 ticker paragraph-boundary flush alignment (synthetic) ---"

ensure_infrastructure

command -v sqlite3 >/dev/null || fail "phase33 requires the sqlite3 CLI to read the archived rendered bodies"
DB="$TEST_CONFIG_DIR/messages.db"
CFG="$TEST_CONFIG_DIR/config.json"
CWD_NOW="$(pwd)"

# Snapshot config.json so the throttle changes below do not leak into later phases in a full suite run.
CFG_BACKUP="$TEST_CONFIG_DIR/config.json.phase33bak"
[ -f "$CFG" ] && command cp -f "$CFG" "$CFG_BACKUP" || true
restore_cfg() { [ -f "$CFG_BACKUP" ] && command cp -f "$CFG_BACKUP" "$CFG" && rm -f "$CFG_BACKUP" || rm -f "$CFG"; }
trap restore_cfg EXIT

post_hook() {
  curl -s -X POST "http://127.0.0.1:${TEST_PORT}/hook/$1" -H "Content-Type: application/json" -d "$2" >/dev/null 2>&1 || true
}

# md payload: sid tgt mid turn idx delta(final backslash-n stays literal -> JSON parses to newline) final
md() {
  post_hook MessageDisplay "$(printf '{"session_id":"%s","tmux_target":"%s","cwd":"%s","project":"tg-cli","backend":"cc","hook_event_name":"MessageDisplay","message_id":"%s","turn_id":"t_%s","prompt_id":"p_%s","index":%s,"delta":"%s","final":%s}' \
    "$1" "$2" "$CWD_NOW" "$3" "$3" "$3" "$4" "$5" "$6")"
}

set_throttle() { # $1 = ms  (atomic rewrite; the bot re-reads config.json per tick/hook)
  CFG="$CFG" VAL="$1" python3 - <<'PY'
import json, os
p = os.environ["CFG"]
try:
    c = json.load(open(p))
except Exception:
    c = {}
c["streamThrottleMs"] = int(os.environ["VAL"])
tmp = p + ".tmp"
json.dump(c, open(tmp, "w"))
os.replace(tmp, p)
PY
}

# count of archived bodies matching a WHERE clause (isolated by a unique marker in content).
db_count() { sqlite3 -batch -cmd ".timeout 5000" "$DB" "SELECT count(*) FROM operations WHERE $1;" 2>/dev/null || echo 0; }

PHASE_LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)

# =============================================================================
# TC-a: crosses several "\n\n" — each intermediate edit renders only up to a boundary.
# =============================================================================
echo ""
echo "--- TC-a: crosses several boundaries, each intermediate edit stops at a \\n\\n ---"
SID_A="align-a-$RANDOM"; TGT_A="%97@/tmp/tmux-1000/tg-cli-test-align-a"
set_throttle 300
# Distinct markers per paragraph AND per partial-vs-completed state so each assertion is RED-distinguishing:
# a *HEAD marker names the partial tail of a paragraph; a *DONE marker appears only once that paragraph is
# COMPLETED in the next delta. This prevents a full (unaligned) render of an earlier delta from vacuously
# satisfying a later "boundary reached" assertion.
# delta0: complete alpha paragraph + boundary + partial beta (BBBHEAD, not yet BBBDONE).
md "$SID_A" "$TGT_A" "mAAA" 0 'AAAPARA alpha paragraph body.\n\nBBBHEAD partial beta' false
sleep 2  # let the ticker fire the first aligned render (alpha only)
# delta1: completes beta (BBBDONE) + boundary + partial gamma (CCCHEAD, not yet CCCDONE).
md "$SID_A" "$TGT_A" "mAAA" 1 'BBBHEAD BBBDONE full beta paragraph.\n\nCCCHEAD partial gamma' false
sleep 2  # ticker fires the second aligned render (up to and incl the beta boundary)
# delta2 final: completes gamma (CCCDONE).
md "$SID_A" "$TGT_A" "mAAA" 2 'CCCHEAD CCCDONE full gamma paragraph. The very end.' true
sleep 2

echo "  DEBUG-a bodies (op|text_len|first80):"
sqlite3 -batch -cmd ".timeout 5000" "$DB" "SELECT op||' | '||text_len||' | '||substr(replace(content,char(10),' '),1,80) FROM operations WHERE content LIKE '%AAAPARA%' ORDER BY op_id;" 2>/dev/null | sed 's/^/    /' || true

# (a.1) an intermediate body has AAAPARA but NOT BBBHEAD (alpha rendered, partial beta withheld at the boundary).
A1=$(db_count "content LIKE '%AAAPARA%' AND content NOT LIKE '%BBBHEAD%'")
[ "$A1" -ge 1 ] && pass "TC-a.1: an intermediate edit rendered up to the first boundary (AAAPARA present, partial beta BBBHEAD withheld)" \
  || record_fail "TC-a.1: no AAAPARA-without-BBBHEAD body — the partial tail was NOT withheld (alignment not applied)"
# (a.2) an intermediate body has BBBDONE but NOT CCCHEAD (beta completed and rendered at its boundary, partial
# gamma withheld). BBBDONE appears ONLY once beta is completed (delta1), so a full render of delta0 cannot satisfy it.
A2=$(db_count "content LIKE '%BBBDONE%' AND content NOT LIKE '%CCCHEAD%'")
[ "$A2" -ge 1 ] && pass "TC-a.2: the next intermediate edit stopped at the beta boundary (BBBDONE present, partial gamma CCCHEAD withheld)" \
  || record_fail "TC-a.2: no BBBDONE-without-CCCHEAD body — the second partial tail was NOT withheld (only the first boundary aligned, or full render)"
# (a.3) F3 no-loss: the final render carries CCCDONE (nothing lost at finalize). Green both — a completeness guard.
A3=$(db_count "content LIKE '%CCCDONE%'")
[ "$A3" -ge 1 ] && pass "TC-a.3: finalize rendered the full body incl. the last paragraph (CCCDONE, no text lost)" \
  || record_fail "TC-a.3: CCCDONE never rendered — text lost at finalize (F3 violated)"

# =============================================================================
# TC-b: no "\n\n" — no intermediate render before Stop; full body at Stop.
# =============================================================================
echo ""
echo "--- TC-b: no blank line -> no intermediate edit, renders in full at Stop ---"
SID_B="align-b-$RANDOM"; TGT_B="%97@/tmp/tmux-1000/tg-cli-test-align-b"
set_throttle 300
LOG_B=$(wc -l < "$LOG_FILE")
md "$SID_B" "$TGT_B" "mNOBRK" 0 'NOBREAKZ one single paragraph with no blank line at all it just keeps running on' false
sleep 3  # several ticker cycles (300ms throttle + 200ms tick) — the ticker must NOT render (no boundary)
set +eo pipefail
tail -n +"$((LOG_B + 1))" "$LOG_FILE" | grep -qE "Stream (send|edit):.*message_id=mNOBRK"
_ps_pre=("${PIPESTATUS[@]}")
set -eo pipefail
# (b.1) no intermediate Stream send/edit for mNOBRK before Stop.
[ "${_ps_pre[1]}" -ne 0 ] && pass "TC-b.1: no intermediate render for a no-boundary stream (nothing sent before Stop)" \
  || record_fail "TC-b.1: an intermediate Stream send/edit for mNOBRK appeared BEFORE Stop — a boundaryless partial was rendered"
# Stop finalizes -> full render.
post_hook Stop "{\"session_id\":\"$SID_B\",\"tmux_target\":\"$TGT_B\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"Stop\",\"last_assistant_message\":\"NOBREAKZ one single paragraph with no blank line at all it just keeps running on\"}"
sleep 3
# (b.2) after Stop, the full body (NOBREAKZ) is rendered.
B2=$(db_count "content LIKE '%NOBREAKZ%'")
[ "$B2" -ge 1 ] && pass "TC-b.2: the no-boundary reply rendered in full at Stop (NOBREAKZ present)" \
  || record_fail "TC-b.2: NOBREAKZ never rendered — the no-boundary reply was lost (Stop finalize did not render full)"

# =============================================================================
# TC-c/d: pre-tool flush renders FULL mid-paragraph (d), then F5 — no later body shrinks (c).
# =============================================================================
echo ""
echo "--- TC-c/d: pre-tool flush renders full (d); F5 never shrinks (c) ---"
SID_CD="align-cd-$RANDOM"; TGT_CD="%97@/tmp/tmux-1000/tg-cli-test-align-cd"
set_throttle 60000  # keep the ticker from firing during setup so the FIRST render is the pre-tool full flush
md "$SID_CD" "$TGT_CD" "mMID" 0 'MIDPARA complete first paragraph.\n\nTAILPARA mid-paragraph tail with no trailing boundary' false
sleep 1
# PreToolUse -> flushStreamOp renders the FULL current text (non-ticker path).
post_hook PreToolUse "{\"session_id\":\"$SID_CD\",\"tmux_target\":\"$TGT_CD\",\"cwd\":\"$CWD_NOW\",\"project\":\"tg-cli\",\"backend\":\"cc\",\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Bash\",\"tool_use_id\":\"tuCD1\",\"prompt_id\":\"p_mMID\",\"turn_id\":\"t_mMID\"}"
sleep 3  # bounded pre-tool drain (<=1.5s) + flush + archive write

echo "  DEBUG-cd bodies (op|text_len|first80):"
sqlite3 -batch -cmd ".timeout 5000" "$DB" "SELECT op||' | '||text_len||' | '||substr(replace(content,char(10),' '),1,80) FROM operations WHERE content LIKE '%MIDPARA%' ORDER BY op_id;" 2>/dev/null | sed 's/^/    /' || true

# (d) the pre-tool flush rendered the FULL mid-paragraph text (TAILPARA present).
D1=$(db_count "content LIKE '%TAILPARA%'")
[ "$D1" -ge 1 ] && pass "TC-d: the pre-tool flush rendered the FULL text incl. the mid-paragraph tail (TAILPARA present)" \
  || record_fail "TC-d: TAILPARA absent — the pre-tool flush truncated to a boundary (mode-gate leaked alignment into the non-ticker flush)"

# Grow the SAME paragraph with NO new boundary, then let the ticker fire — F5 must skip (no shrink).
md "$SID_CD" "$TGT_CD" "mMID" 1 ' and still more tail text with no newline' false
set_throttle 300
sleep 3  # the ticker fires; the aligned body would end at the OLDER boundary (shorter) -> F5 skip

# (c) F5: the archived body text_len for this message is NON-DECREASING by op_id (never shrinks).
LENS=$(sqlite3 -batch -cmd ".timeout 5000" "$DB" "SELECT text_len FROM operations WHERE content LIKE '%MIDPARA%' ORDER BY op_id;" 2>/dev/null || echo "")
echo "  DEBUG-c text_len sequence: $(printf '%s' "$LENS" | tr '\n' ' ')"
prev=-1; shrunk=0
while IFS= read -r n; do
  [ -z "$n" ] && continue
  if [ "$n" -lt "$prev" ]; then shrunk=1; fi
  prev="$n"
done <<< "$LENS"
[ "$shrunk" -eq 0 ] && pass "TC-c: F5 — the rendered body never shrank (text_len non-decreasing after the full mid-paragraph render)" \
  || record_fail "TC-c: F5 VIOLATED — a later rendered body was SHORTER than an earlier one (text_len decreased) — the bubble shrank"

echo ""
echo "--- Round 3 ticker alignment phase complete ---"
