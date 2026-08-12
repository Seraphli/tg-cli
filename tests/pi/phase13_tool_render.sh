#!/bin/bash
# Phase 13 = Round-2 Item 2 (compact mode) + Item A (standard mode), each via one pi bash tool call.
#
#   Item 2 — the COMPACT tool notification must render "💻 Bash: <command>" (normalized name), NOT the
#            pre-fix "🔧 bash: <random-map-field>" default. Its fix lives in BuildCompactToolLine, exercised
#            only when toolNotifyCompact=true.
#   Item A — the tool RESULT append must render the text output, NOT the raw JSON array
#            ([{"type":"text","text":…}]). Its fix lives in BuildToolResultText.
#
# toolNotifyCompact is a per-test flag; the bot re-reads config.json PER HOOK EVENT (register.go LoadAppConfig
# :575), so we toggle it live via an ATOMIC rewrite (temp + os.replace — a partial write would break the
# concurrent per-hook read). Item 2 runs with it TRUE, Item A with it FALSE.
#
# WHY Item A must run in STANDARD (non-compact) mode (note3-corrected — it is STRUCTURAL, not a race):
# ToolUseMsgs.Store has exactly one call site (register.go:747) inside the STANDARD-mode branch; compact mode
# accumulates into CompactTools and NEVER stores a ToolUseMsgs entry. BuildToolResultText's only non-test call
# site (register.go:1045, applyToolResultEditAt) is reachable only when a ToolUseMsgs entry exists. So in
# compact mode the result is NEVER appended for ANY backend (a separate scope question note3 is putting to the
# boss); in standard mode the entry is stored, PostToolUse's Get hits, and the result renders.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi Item2: tool compact-line render ---"

ensure_infrastructure
start_pi "e2e-pi-13"

set_compact() {
  CFG_JSON="$TEST_CONFIG_DIR/config.json" VAL="$1" python3 - <<'PY'
import json, os
p = os.environ["CFG_JSON"]
c = json.load(open(p))
c["toolNotifyCompact"] = (os.environ["VAL"] == "true")
tmp = p + ".tmp"
json.dump(c, open(tmp, "w"))
os.replace(tmp, p)
PY
}
set_compact true

MARK="PI_TOOL_RENDER_MARKER"
LOG_BEFORE=$(wc -l < "$LOG_FILE")
pane_log "[pi/item2] before bash inject (compact mode on)"
inject_prompt "Run the bash command: echo $MARK. Do not explain — just run the bash tool."
wait_for_idle "$TIMEOUT" "$E2E_PANE"
sleep 3
SLICE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE")
pane_log "[pi/item2] after bash run"

# Extract the rendered notification BODY blocks (between <<<BODY and BODY>>>). Item 2 must be asserted on the
# RENDERED body (not the raw hook payload, which carries pi's lowercase name verbatim regardless of the fix).
BODIES=$(printf '%s\n' "$SLICE" | awk '/<<<BODY/{c=1;next} /BODY>>>/{c=0;next} c')
echo "  === DEBUG: rendered notification BODY blocks ==="
printf '%s\n' "$BODIES" | sed 's/^/    /'
echo "  === END DEBUG ==="

# PRECONDITION: a compact tool line must have rendered (else Item 2 is un-exercised).
printf '%s\n' "$BODIES" | grep -q "echo $MARK" || fail "pi Item2 PRECONDITION: no rendered compact tool line for the bash call — cannot test the compact-line render"

# Item 2: the compact tool line must use the normalized "💻 Bash", not the pre-fix "🔧 bash" default.
set +eo pipefail
printf '%s\n' "$BODIES" | grep -qE "💻 Bash: echo $MARK"
_ps_bash=("${PIPESTATUS[@]}")
printf '%s\n' "$BODIES" | grep -qE "🔧 bash"
_ps_generic=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps_bash[1]}" -eq 0 ] && pass "pi Item2: compact tool line renders 💻 Bash: <command> (normalized name)" \
  || record_fail "pi Item2: compact tool line NOT normalized (no 💻 Bash: <command>) — pi lowercase name hit the 🔧 default"
[ "${_ps_generic[1]}" -ne 0 ] && pass "pi Item2: no 🔧 bash generic/random-field line in the rendered body" \
  || record_fail "pi Item2: 🔧 bash generic line present in the rendered body — normalization not applied"

# ---------------- Part 2: Item A — tool RESULT render in STANDARD (non-compact) mode ----------------
# In standard mode the PostToolUse result is appended to the tool notification via an EDIT. RetryEditRich logs
# only "TG edit ... text_len" (no body), but the FULL edited body (tool call + rendered result) is archived
# in messages.db operations(op='edit', content) — that is the observable for the RENDERED result.
command -v sqlite3 >/dev/null || fail "pi Item A: phase13 requires the sqlite3 CLI to read the archived rendered result"
set_compact false
# Marker deliberately has NO "Result" substring — else the archive filter below (which keys on
# BuildToolResultText's "✅ Result:" prefix) would also match the assistant Stop message that echoes the
# marker via last_assistant_message, and pick the wrong (raw-array-free) row -> a false green on pre-fix.
MARKA="PISTDOUT_ZZ99_MARKER"
pane_log "[pi/item A] before bash inject (standard mode)"
inject_prompt "Run the bash command: echo $MARKA. Do not explain — just run the bash tool."
wait_for_idle "$TIMEOUT" "$E2E_PANE"
sleep 3

DB="$TEST_CONFIG_DIR/messages.db"
# The tool-result edit's archived body carries the tool call + BuildToolResultText's "✅ Result:" prefix +
# the rendered result (present in BOTH pre-fix and post-fix; pre-fix dumps the raw array after "Result:",
# post-fix the joined text). "Result:" is BuildToolResultText-specific and isolates the tool-result edit from
# the assistant/tool-call edits.
EDIT_BODY=$(sqlite3 -batch -cmd ".timeout 5000" "$DB" \
  "SELECT content FROM operations WHERE op='edit' AND content LIKE '%Result:%' AND content LIKE '%$MARKA%' ORDER BY op_id DESC LIMIT 1;" 2>/dev/null || echo "")
echo "  === DEBUG-A: archived tool-result edit body (first 400 chars) ==="
echo "    ${EDIT_BODY:0:400}"
echo "  === END DEBUG-A ==="

# PRECONDITION: the result must have been appended in standard mode (else Item A is un-exercised).
[ -n "$EDIT_BODY" ] || fail "pi Item A PRECONDITION: no archived tool-result edit carrying '$MARKA' — the result was not appended (standard-mode ToolUseMsgs path did not run)"

# Item A: the rendered result must be the text output, NOT the raw JSON array structure
# ("type":"text", HTML-escaped as &quot;type&quot;:&quot;text&quot;).
set +eo pipefail
printf '%s' "$EDIT_BODY" | grep -qE '&quot;type&quot;:&quot;text&quot;|"type":"text"|\[\{&quot;type|\[\{"type"'
_ps_rawA=("${PIPESTATUS[@]}")
printf '%s' "$EDIT_BODY" | grep -q "$MARKA"
_ps_markA=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps_rawA[1]}" -ne 0 ] && pass "pi Item A: standard-mode tool result renders text, not the raw JSON array" \
  || record_fail "pi Item A: standard-mode tool result dumped the raw JSON array ([{\"type\":\"text\"…]) into the appended body"
[ "${_ps_markA[1]}" -eq 0 ] && pass "pi Item A: standard-mode tool result carries the echo output ($MARKA)" \
  || record_fail "pi Item A: standard-mode tool result missing the echo output ($MARKA)"

echo "  pi Items 2 (compact) + A (standard) tool render test complete."
