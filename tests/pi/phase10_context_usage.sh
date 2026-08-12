#!/bin/bash
# Phase 10 = round-1 feature: a pi session shows the 📊 Context segment in Telegram with pi's
# denominator = contextWindow - reserveTokens (distance to auto-compaction), reading the CONFIGURED
# reserve. ensure_pi_config sets compaction.reserveTokens=32768 (!= the 16384 default); this phase
# asserts context_total == formatK(contextWindow - 32768), which FAILS loudly if the extension fell
# back to the default reserve or used the raw window as the denominator.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi (context) Telegram shows 📊 Context (window - reserveTokens denominator) ---"

ensure_infrastructure
start_pi "e2e-pi-10"

# One real turn: agent_settled writes the context file with post-turn tokens; streamed notifications
# carry the 📊 Context header. A multi-sentence answer guarantees streaming renders (not just a Stop flush).
LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
pane_log "[pi/context] before prompt inject"
inject_prompt "Write exactly three short sentences about the color blue. Do not use any tools."
wait_for_idle "$TIMEOUT" "$E2E_PANE"
pane_log "[pi/context] after wait_for_idle"

# Resolve the pi session id (== getSessionId(), the key the extension writes and the bot reads).
SESSION_ID=$(curl -s "http://127.0.0.1:$TEST_PORT/session/list" | python3 -c '
import sys, json
pane = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); sys.exit(0)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane.split("@")[0] + "@"):
        print(s.get("id", "")); sys.exit(0)
print("")
' "$E2E_PANE" 2>/dev/null || echo "")
if [ -z "$SESSION_ID" ]; then
  fail "pi (context): could not resolve session id for target $E2E_PANE"
fi
echo "  DEBUG: pi session id = $SESSION_ID"

CTX_FILE="/tmp/tg-cli/context/$SESSION_ID.json"
if [ ! -f "$CTX_FILE" ]; then
  fail "pi (context): extension did not write context file $CTX_FILE"
fi
echo "  DEBUG: context file: $(cat "$CTX_FILE")"

# (10.1) the file carries the CONFIGURED reserve (32768) — proves the settings.json read path executed
# (not the 16384 default). This is the decision lever note3 requires.
RESERVE_IN_FILE=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("reserve_tokens"))' "$CTX_FILE")
if [ "$RESERVE_IN_FILE" = "32768" ]; then
  pass "pi (context): extension read configured reserveTokens=32768 (not the 16384 default)"
else
  fail "pi (context): reserve_tokens=$RESERVE_IN_FILE in context file, expected 32768 (configured-reserve read did not execute)"
fi

PI_BACKEND=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("backend",""))' "$CTX_FILE")
W=$(python3 -c 'import json,sys; print(int(json.load(open(sys.argv[1])).get("context_window",0)))' "$CTX_FILE")
T=$(python3 -c 'import json,sys; print(int(json.load(open(sys.argv[1])).get("context_tokens",0)))' "$CTX_FILE")
if [ "$PI_BACKEND" != "pi" ]; then
  fail "pi (context): context file backend=$PI_BACKEND, expected pi"
fi
if [ "$W" -le 32768 ]; then
  fail "pi (context): captured contextWindow W=$W not greater than reserve 32768 (no valid denominator)"
fi

# session log --format json emits context_pct/context_used/context_total via ReadContextUsage (same file).
curl -s "http://127.0.0.1:$TEST_PORT/session/name?session_id=$SESSION_ID&name=e2e-pi-ctx" > /dev/null 2>&1 || true
JSON_OUTPUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" session log --name e2e-pi-ctx --port "$TEST_PORT" --lines 5 --format json 2>&1) || true
echo "  DEBUG: session log json: $JSON_OUTPUT"

# (10.2) exact asserts: total==formatK(W-32768), used==formatK(T), pct==int(T/(W-32768)*100) in 0..100, used<=total.
ASSERT=$(W="$W" T="$T" python3 -c '
import json, os, sys
raw = sys.stdin.read()
try:
    d = json.loads(raw)
except Exception as e:
    print("json parse error: %s :: %r" % (e, raw[:200])); sys.exit(0)
W = int(os.environ["W"]); T = int(os.environ["T"]); R = 32768
eff = W - R
def fk(n): return "%.1fk" % (n / 1000)
exp_total = fk(eff); exp_used = fk(T); exp_pct = int(T / eff * 100)
gt = d.get("context_total"); gu = d.get("context_used"); gp = d.get("context_pct")
errs = []
if gt != exp_total: errs.append("context_total=%r expected %r (contextWindow %d - 32768 = %d)" % (gt, exp_total, W, eff))
if gu != exp_used: errs.append("context_used=%r expected %r" % (gu, exp_used))
if gp != exp_pct: errs.append("context_pct=%r expected %r" % (gp, exp_pct))
if not isinstance(gp, int) or gp < 0 or gp > 100: errs.append("context_pct %r not int in 0..100" % (gp,))
if T > eff: errs.append("used(%d) > total(%d)" % (T, eff))
print("OK" if not errs else "FAIL: " + "; ".join(errs))
' <<<"$JSON_OUTPUT")
if [ "$ASSERT" = "OK" ]; then
  pass "pi (context): session log json context_total==contextWindow-32768, used==%.1fk(T), pct==int(T/(W-32768)*100) in 0..100"
else
  fail "pi (context): $ASSERT"
fi

# (10.3) at least one Telegram notification for THIS phase's pi turn carried the 📊 Context segment.
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "📊 Context:"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  pass "pi (context): a Telegram notification carried the 📊 Context segment"
else
  fail "pi (context): no notification carried the 📊 Context segment in this phase's log"
fi

pane_log "[pi/context] end"
echo "  pi (context) test complete."
