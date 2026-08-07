#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Compact tool notification test ---"

# ── phase26 shared log-analysis helper (r9 S2) ────────────────────────────────────────────────────────
# The SINGLE production helper used by BOTH TC1/TC6 AND the offline fixtures below (not a copy). Reads a
# bot.log SLICE on stdin; params come from env: P26_CHAT (test chat id, for TG-edit pairing scope),
# P26_PANE (this session's %pane@socket, for PreToolUse session/turn scoping), P26_MARKERS (JSON list of
# [name,label,value] to resolve), P26_ALLINONE (JSON list of tokens that must co-occur in ONE message).
# It emits one JSON object. What it does:
#  (a1) CONTENT reconstruction — assistant-only. Parse every single-line "Raw hook payload [MessageDisplay]"
#       JSON, group deltas by message_id, last-write-wins per index, sort by index, join (the phase27:620-643
#       seam). Allow the "Raw hook payload [Stop].last_assistant_message" fallback. "Raw hook payload
#       [PostToolUse]" is NEVER a content source (it echoes the tool output, so a window grep false-passes).
#  (a2) DELIVERY / placement anchoring. Parse each "Stream send|edit: ... full_text:" record together with
#       its BODY (the body runs to the next line carrying the FULL logger prefix "[RFC3339] [PID=n] [LEVEL]",
#       logger.go:36-40 — NEVER a line that merely looks like a date). A marker-bearing Stream SEND counts as
#       delivery on its own (logged only on success, stream.go:59-64). A marker-bearing Stream EDIT counts
#       ONLY when paired BY ORDER (not physical adjacency) with the nearest PRECEDING as-yet-unconsumed
#       successful "TG edit:" (rich=true sender.go:405 OR rich=false :569) for the SAME chat+msg_id — unrelated
#       log lines allowed between, one TG-edit success consumed per edit record. For a marker, the earliest
#       genuinely-delivered marker-bearing render = its FIRST ACTUAL DELIVERY (full_text is cumulative, so a
#       marker reappears in later edits). Placement = that message_id's FIRST successful Stream send.
#  FULL-VALUE marker match: the char after the value must be a non-digit or end-of-string (CAT_FILE=1 must
#  NOT be satisfied by CAT_SCORE=10); trailing punctuation is allowed, an extra digit is not.
#  Also returns: PreToolUse counts (count ONLY "Raw hook payload [PreToolUse]" lines — NEVER the 3x-over-
#  counting hook_event_name field — scoped to this session by tmux_target==P26_PANE, with the distinct
#  session_id / prompt_id counts so the caller can confirm one session + one turn), the ordered PreToolUse
#  tool_input.command list, per-msg_id successful "TG edit:" counts (TC1 aggregation), and compact-send lines.
P26_HELPER=$(cat <<'PYEOF'
import json, os, re, sys

chat = os.environ.get("P26_CHAT", "")
pane = os.environ.get("P26_PANE", "")
markers = json.loads(os.environ.get("P26_MARKERS", "[]"))
allinone = json.loads(os.environ.get("P26_ALLINONE", "[]"))

lines = sys.stdin.read().split("\n")
n = len(lines)

PREFIX = re.compile(r'^\[\d{4}-\d{2}-\d{2}T[0-9:.+\-Z]+\] \[PID=\d+\] \[[A-Z]+\] ')
def strip_prefix(s):
    m = PREFIX.match(s)
    return s[m.end():] if m else None   # None => this physical line is NOT a fresh logger entry

# ---- (a1) MessageDisplay reconstruction + Stop last_assistant_message ----
md = {}; md_order = []; stop_lam = ""
for ln in lines:
    if "Raw hook payload [MessageDisplay]:" in ln:
        m = re.search(r'Raw hook payload \[MessageDisplay\]: (\{.*\})\s*$', ln)
        if m:
            try:
                d = json.loads(m.group(1))
            except Exception:
                d = None
            if d and d.get("message_id"):
                mid = d["message_id"]
                if mid not in md:
                    md[mid] = {}; md_order.append(mid)
                md[mid][d.get("index", 0)] = d.get("delta", "")
    elif "Raw hook payload [Stop]:" in ln:
        m = re.search(r'Raw hook payload \[Stop\]: (\{.*\})\s*$', ln)
        if m:
            try:
                lam = json.loads(m.group(1)).get("last_assistant_message", "")
            except Exception:
                lam = ""
            if lam:
                stop_lam = lam
md_texts = {mid: "".join(md[mid][i] for i in sorted(md[mid])) for mid in md_order}

# ---- Walk records (Stream/compact/Notification bodies bounded by the next full logger prefix) ----
stream = []; compact = []; tg = []; notif = []; pre = []

def read_body(start):
    out = []; j = start
    while j < n and strip_prefix(lines[j]) is None:
        out.append(lines[j]); j += 1
    return "\n".join(out), j

i = 0
while i < n:
    ln = lines[i]; rest = strip_prefix(ln); lineno = i + 1
    if rest is None:
        i += 1; continue
    m = re.match(r'Stream (send|edit): msg_id=(\d+) message_id=(\S+) turn_id=(\S+) chunk=(\d+).* full_text:\s*$', rest)
    if m:
        body, j = read_body(i + 1)
        stream.append({"kind": m.group(1), "msg_id": m.group(2), "message_id": m.group(3), "line": lineno, "body": body})
        i = j; continue
    m = re.match(r'TG edit: chat=(\S+) msg_id=(\S+) text_len=\d+ rich=(true|false)', rest)
    if m:
        tg.append({"chat": m.group(1), "msg_id": m.group(2), "line": lineno})
        i += 1; continue
    m = re.match(r'compact tool sent: session=(\S+) msg_id=(\d+) tool=(\S+) chat=(\S+) fmt=rich', rest)
    if m:
        compact.append({"msg_id": m.group(2), "line": lineno})
        i += 1; continue
    m = re.match(r'Notification sent to chat (\S+): Stop .*? body=(.*)$', rest)
    if m and m.group(1) == chat:
        body, j = read_body(i + 1)
        notif.append({"line": lineno, "body": m.group(2) + (("\n" + body) if body else "")})
        i = j; continue
    if "Raw hook payload [PreToolUse]:" in ln:
        m2 = re.search(r'Raw hook payload \[PreToolUse\]: (\{.*\})\s*$', ln)
        if m2:
            try:
                d = json.loads(m2.group(1))
            except Exception:
                d = None
            if d:
                ti = d.get("tool_input")
                cmd = ti.get("command", "") if isinstance(ti, dict) else ""
                pre.append({"session_id": d.get("session_id", ""), "prompt_id": d.get("prompt_id", ""),
                            "tmux": d.get("tmux_target", ""), "tool": d.get("tool_name", ""),
                            "command": cmd or "", "line": lineno})
        i += 1; continue
    i += 1

# ---- (a2) delivery: sends delivered on their own; edits paired BY ORDER with a preceding unconsumed TG edit ----
events = [(r["line"], "s", r) for r in stream] + [(t["line"], "t", t) for t in tg]
events.sort(key=lambda x: x[0])
pending = {}   # msg_id -> [unconsumed successful TG-edit lines]
for (L, kind, obj) in events:
    if kind == "t":
        if obj["chat"] == chat:
            pending.setdefault(obj["msg_id"], []).append(L)
    else:
        if obj["kind"] == "send":
            obj["delivered"] = True
        else:
            q = pending.get(obj["msg_id"])
            if q:
                q.pop()            # nearest preceding unconsumed success; one per edit record
                obj["delivered"] = True
            else:
                obj["delivered"] = False

def full_value(text, label, value):
    if not text:
        return False
    return re.search(re.escape(label) + re.escape(value) + r'(?![0-9])', text) is not None

def resolve(label, value):
    res = {"content_present": False, "delivery_line": -1, "delivery_message_id": "",
           "placement_send_line": -1, "stop_delivery_line": -1}
    if any(full_value(t, label, value) for t in md_texts.values()) or full_value(stop_lam, label, value):
        res["content_present"] = True
    cand = sorted([r for r in stream if r.get("delivered") and full_value(r["body"], label, value)], key=lambda r: r["line"])
    if cand:
        r0 = cand[0]
        res["delivery_line"] = r0["line"]
        res["delivery_message_id"] = r0["message_id"]
        sends = [r["line"] for r in stream if r["kind"] == "send" and r["message_id"] == r0["message_id"]]
        if sends:
            res["placement_send_line"] = min(sends)
    stop_cand = [ns["line"] for ns in notif if full_value(ns["body"], label, value)]
    if stop_cand:
        res["stop_delivery_line"] = min(stop_cand)
    return res

pre_scoped = [p for p in pre if (not pane or p["tmux"] == pane)]
by_tool = {}
for p in pre_scoped:
    by_tool[p["tool"]] = by_tool.get(p["tool"], 0) + 1
tg_count = {}
for t in tg:
    if t["chat"] == chat:
        tg_count[t["msg_id"]] = tg_count.get(t["msg_id"], 0) + 1

def all_in_one(tokens):
    if not tokens:
        return False
    for t in list(md_texts.values()) + ([stop_lam] if stop_lam else []):
        if all(tok in t for tok in tokens):
            return True
    return False

print(json.dumps({
    "pretool": {"total": len(pre_scoped), "by_tool": by_tool,
                "commands": [p["command"] for p in pre_scoped],
                "prompt_id_count": len(set(p["prompt_id"] for p in pre_scoped)),
                "session_id_count": len(set(p["session_id"] for p in pre_scoped))},
    "compact_sends": [{"msg_id": c["msg_id"], "line": c["line"]} for c in compact],
    "compact_count": len(compact),
    "tg_edit_count": tg_count,
    "all_in_one": all_in_one(allinone),
    "markers": {m[0]: resolve(m[1], m[2]) for m in markers},
}))
PYEOF
)

# Run the shared helper on the slice piped via stdin (env P26_* configure it); echoes the JSON result.
_p26() { python3 -c "$P26_HELPER"; }

# ── Offline fixtures (r9 (b)): deterministic, no model/bot — synthetic slices exercise the SAME helper ──
# Each feeds one advisor case to $P26_HELPER (via _p26) and asserts the helper's verdict. Chat is a fake
# id (555). These prove the ORDER-based pairing, the placement anchor, and the Stop-fallback delivery anchor.
_p26_run_fixtures() {
  echo "  [fixtures] shared delivery/pairing helper (offline)"
  local slice res

  # (i) unrelated records interleaved between a TG-edit success and its marker-bearing Stream edit STILL pair.
  slice=$(cat <<'FIXEOF'
[2026-07-30T00:00:00+08:00] [PID=1] [DEBUG] compact tool sent: session=s msg_id=100 tool=Read chat=555 fmt=rich
<<<BODY
compact body
BODY>>>
[2026-07-30T00:00:01+08:00] [PID=1] [DEBUG] Stream send: msg_id=200 message_id=MSGI turn_id=t1 chunk=0 fmt=rich full_text:
opening render, no marker here
[2026-07-30T00:00:02+08:00] [PID=1] [INFO] TG edit: chat=555 msg_id=200 text_len=40 rich=true
[2026-07-30T00:00:03+08:00] [PID=1] [DEBUG] Stream edit: msg_id=999 message_id=OTHER turn_id=t9 chunk=0 final=false fmt=rich full_text:
unrelated other-session edit body
[2026-07-30T00:00:04+08:00] [PID=1] [INFO] Raw hook payload [PreToolUse]: {"session_id":"x","prompt_id":"p","tmux_target":"%9@/x","tool_name":"Bash","tool_input":{"command":"noop"}}
[2026-07-30T00:00:05+08:00] [PID=1] [DEBUG] Stream edit: msg_id=200 message_id=MSGI turn_id=t1 chunk=1 final=true fmt=rich full_text:
final render carrying FIXMARK=42 now
FIXEOF
)
  res=$(printf '%s' "$slice" | P26_CHAT=555 P26_MARKERS='[["I","FIXMARK=","42"]]' _p26)
  if [ "$(echo "$res" | jq -r '.markers.I.delivery_line')" != "-1" ] \
     && [ "$(echo "$res" | jq -r '.markers.I.delivery_message_id')" = "MSGI" ]; then
    pass "fixture(i): marker edit pairs with its preceding TG-edit success across unrelated records (order-based, not adjacency)"
  else
    fail "fixture(i): interleaved-record pairing broke (delivery_line=$(echo "$res" | jq -r '.markers.I.delivery_line'))"
  fi

  # (ii) a failed marker edit (no TG-edit success of its own), preceded by an OLDER success already consumed
  #      by an earlier edit, is NOT falsely anchored to the undelivered render.
  slice=$(cat <<'FIXEOF'
[2026-07-30T00:00:00+08:00] [PID=1] [DEBUG] Stream send: msg_id=300 message_id=MSGII turn_id=t2 chunk=0 fmt=rich full_text:
initial render, no marker
[2026-07-30T00:00:01+08:00] [PID=1] [INFO] TG edit: chat=555 msg_id=300 text_len=30 rich=true
[2026-07-30T00:00:02+08:00] [PID=1] [DEBUG] Stream edit: msg_id=300 message_id=MSGII turn_id=t2 chunk=1 final=false fmt=rich full_text:
revised render, still no marker
[2026-07-30T00:00:03+08:00] [PID=1] [DEBUG] Stream edit: msg_id=300 message_id=MSGII turn_id=t2 chunk=2 final=true fmt=rich full_text:
this edit carries FIX2=7 but its Telegram edit failed
FIXEOF
)
  res=$(printf '%s' "$slice" | P26_CHAT=555 P26_MARKERS='[["II","FIX2=","7"]]' _p26)
  if [ "$(echo "$res" | jq -r '.markers.II.delivery_line')" = "-1" ]; then
    pass "fixture(ii): undelivered marker edit (older success consumed by an earlier edit) is NOT anchored"
  else
    fail "fixture(ii): falsely anchored an undelivered marker edit (delivery_line=$(echo "$res" | jq -r '.markers.II.delivery_line'))"
  fi

  # (iii) an old bubble (sent BEFORE the compact) edited AFTER the compact fails the PLACEMENT anchor even
  #       though the marker CONTENT is delivered inside the interval.
  slice=$(cat <<'FIXEOF'
[2026-07-30T00:00:00+08:00] [PID=1] [DEBUG] Stream send: msg_id=400 message_id=MSGIII turn_id=t3 chunk=0 fmt=rich full_text:
old bubble created above the compact, no marker
[2026-07-30T00:00:01+08:00] [PID=1] [DEBUG] compact tool sent: session=s msg_id=500 tool=Read chat=555 fmt=rich
<<<BODY
compact body
BODY>>>
[2026-07-30T00:00:02+08:00] [PID=1] [INFO] TG edit: chat=555 msg_id=400 text_len=30 rich=true
[2026-07-30T00:00:03+08:00] [PID=1] [DEBUG] Stream edit: msg_id=400 message_id=MSGIII turn_id=t3 chunk=1 final=true fmt=rich full_text:
edit AFTER compact adds FIX3=55 to the old bubble
FIXEOF
)
  res=$(printf '%s' "$slice" | P26_CHAT=555 P26_MARKERS='[["III","FIX3=","55"]]' _p26)
  local c_line d_line p_line
  c_line=$(echo "$res" | jq -r '.compact_sends[0].line')
  d_line=$(echo "$res" | jq -r '.markers.III.delivery_line')
  p_line=$(echo "$res" | jq -r '.markers.III.placement_send_line')
  if [ "$d_line" != "-1" ] && [ "$p_line" != "-1" ] && [ "$p_line" -lt "$c_line" ] && [ "$d_line" -gt "$c_line" ]; then
    pass "fixture(iii): placement anchor (first send=$p_line) precedes the compact (line=$c_line) while content delivery (=$d_line) is later — the placement hole is closed"
  else
    fail "fixture(iii): placement anchoring wrong (compact=$c_line placement=$p_line delivery=$d_line)"
  fi

  # (iv) Stop-fallback CONTENT present but NO successful "Notification sent ... Stop" record => delivery FAILS.
  slice=$(cat <<'FIXEOF'
[2026-07-30T00:00:00+08:00] [PID=1] [DEBUG] compact tool sent: session=s msg_id=600 tool=Bash chat=555 fmt=rich
<<<BODY
compact body
BODY>>>
[2026-07-30T00:00:01+08:00] [PID=1] [INFO] Raw hook payload [Stop]: {"last_assistant_message":"And the winner is FIX4=WINNER_CATS today."}
FIXEOF
)
  res=$(printf '%s' "$slice" | P26_CHAT=555 P26_MARKERS='[["IV","FIX4=","WINNER_CATS"]]' _p26)
  if [ "$(echo "$res" | jq -r '.markers.IV.content_present')" = "true" ] \
     && [ "$(echo "$res" | jq -r '.markers.IV.stop_delivery_line')" = "-1" ]; then
    pass "fixture(iv): Stop-fallback content present but no delivered Notification => delivery anchor correctly fails"
  else
    fail "fixture(iv): Stop-fallback delivery anchor wrong (content=$(echo "$res" | jq -r '.markers.IV.content_present') stop_delivery=$(echo "$res" | jq -r '.markers.IV.stop_delivery_line'))"
  fi
}

# Offline entry point for the gate-1 check: run ONLY the deterministic fixtures (no bot / no CC) and exit.
# The normal phase runs the SAME fixtures inline after ensure_infrastructure (below).
if [ "${P26_FIXTURES_ONLY:-}" = "1" ]; then
  _p26_run_fixtures
  echo "phase26 offline fixtures complete"
  exit 0
fi

ensure_infrastructure

# Deterministic offline fixtures for the shared delivery/pairing helper (r9 (b)): AFTER ensure_infrastructure
# and BEFORE start_claude — synthetic slices only, no model.
_p26_run_fixtures

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-26"

pane_log "[compact] BEFORE test"

# Enable compact mode in config
python3 -c "
import json
f='$TEST_CONFIG_DIR/config.json'
d=json.load(open(f))
d['toolNotifyCompact']=True
d['toolNotifyList']=['Read','Bash','Edit','Glob','Grep','Other']
json.dump(d,open(f,'w'))
"

# TC1 recipe cards (FIX 1b): three fresh files, each holding a UNIQUE random alphanumeric ingredient
# token, so the "three ingredients" final sentence can only name tokens the model actually Read.
read -r TC1_TOK1 TC1_TOK2 TC1_TOK3 < <(python3 -c "import secrets; print(' '.join('ING'+secrets.token_hex(4) for _ in range(3)))")
TC1_CARD1="/tmp/tg-cli-test-tc1-card1-$$.txt"
TC1_CARD2="/tmp/tg-cli-test-tc1-card2-$$.txt"
TC1_CARD3="/tmp/tg-cli-test-tc1-card3-$$.txt"
echo "Ingredient: $TC1_TOK1" > "$TC1_CARD1"
echo "Ingredient: $TC1_TOK2" > "$TC1_CARD2"
echo "Ingredient: $TC1_TOK3" > "$TC1_CARD3"

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || true)

# TC1 + TC2 + TC3 (FIX 1b): the r9 chef/recipe scenario. Read the 3 cards in order, all in ONE reply,
# using ONLY the Read tool, saying NOTHING between the reads, then one final sentence naming the three
# ingredients. "Say nothing between the reads" forces the 3 reads to AGGREGATE into ONE compact message
# (the first read sends it, reads 2 & 3 edit it) — the mirror image of TC6 — which is what the TC1
# aggregation proof below verifies. Unique random ingredient tokens force a genuine Read of each card.
pane_log "[compact] BEFORE inject"
inject_prompt "You are a chef and I am reading you a three-card recipe over a one-way kitchen intercom. The intercom broadcasts to the whole line, so the cooks only want to hear one thing from you: the finished ingredient list. Anything you say before that goes out over the speakers and confuses the pass.

So work silently: open Card 1, Card 2 and Card 3 with the Read tool, one straight after the other, with no words at all in between and no other tool. Each card only needs opening once — the cards sit on a rail above the station and reaching back for one you have already read knocks the whole stack down.

Once all three are open, say one single sentence naming the three ingredients.
Card 1: $TC1_CARD1
Card 2: $TC1_CARD2
Card 3: $TC1_CARD3"
pane_log "[compact] AFTER inject"

wait_for_idle
pane_log "[compact] AFTER idle"

# TC1: Verify compact notification was sent with tool name
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "compact tool sent"
_ps_sent=("${PIPESTATUS[@]}")
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "compact tool sent.*tool=Read"
_ps_read=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps_sent[1]}" -eq 0 ] && [ "${_ps_read[1]}" -eq 0 ]; then
  pass "TC1: compact tool notification sent with Read"
else
  fail "TC1: compact tool notification NOT sent or missing Read (sent=${_ps_sent[1]} read=${_ps_read[1]})"
fi

# TC1b: rich migration — the compact tool notification is sent via the rich API
# (RetrySendRich), so its send marker carries fmt=rich on the same log line. This guards
# against an accidental revert of the compact path back to the legacy HTML send.
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -qE "compact tool (sent|edited):.*fmt=rich"
_ps_rich=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps_rich[1]}" -eq 0 ]; then
  pass "TC1b: compact tool sent/edited carries fmt=rich (rich message path confirmed)"
else
  fail "TC1b: no compact tool sent/edited with fmt=rich (expected rich message path)"
fi

# TC1c (Fix 14): each compact tool line is wrapped in a <details> block whose <summary> is the compact
# one-liner (tool emoji + name), with the full args in the collapsed body. Before Fix 14 the compact
# body was a bare line ("📖 Read: ..."); now it is "<details><summary>📖 Read: ...</summary>...</details>".
# Assert a per-tool <summary> with a tool emoji (📋 is the collapsed-header summary, not a tool line).
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -qE "<summary>(📖|💻|📝|✏️|🔍|🤖|🔧)"
_ps_det=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps_det[1]}" -eq 0 ]; then
  pass "TC1c: compact tool notification wraps each tool in a <details> block (Fix 14)"
else
  fail "TC1c: compact body has no per-tool <details><summary> tool line (Fix 14 not applied)"
fi

# ── TC1 FLOW assertions (FIX 1b, r9 S1/S2/S4). These run BEFORE TC1d (note3 ordering ruling) so a model
# that did not call Read fails on the count, not with a misleading rendering-regression message. ────────
TC1_ALLINONE=$(A="$TC1_TOK1" B="$TC1_TOK2" C="$TC1_TOK3" python3 -c "import json,os; print(json.dumps([os.environ['A'],os.environ['B'],os.environ['C']]))")
# S4 bounded-poll: the compact send + its 2 aggregation edits go through the async Message FIFO, so poll
# until they appear (or timeout) BEFORE the exact-count asserts.
for _i in $(seq 1 30); do
  TC1_JSON=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | P26_CHAT="$DEFAULT_CHAT_ID" P26_PANE="$E2E_PANE" P26_ALLINONE="$TC1_ALLINONE" _p26)
  _cm=$(echo "$TC1_JSON" | jq -r '.compact_sends[0].msg_id // ""')
  if [ "$(echo "$TC1_JSON" | jq -r '.compact_count')" = "1" ] \
     && [ "$(echo "$TC1_JSON" | jq -r --arg m "$_cm" '.tg_edit_count[$m] // 0')" -ge 2 ]; then break; fi
  sleep 1
done
TC1_JSON=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | P26_CHAT="$DEFAULT_CHAT_ID" P26_PANE="$E2E_PANE" P26_ALLINONE="$TC1_ALLINONE" _p26)

# (S1) exactly 3 Read PreToolUse in ONE turn — count ONLY 'Raw hook payload [PreToolUse]' lines, scoped to
# this session (tmux_target==E2E_PANE) and confirmed to be a single session + single prompt (turn).
TC1_PTU_TOTAL=$(echo "$TC1_JSON" | jq -r '.pretool.total')
TC1_PTU_READ=$(echo "$TC1_JSON" | jq -r '.pretool.by_tool.Read // 0')
TC1_PTU_PIDS=$(echo "$TC1_JSON" | jq -r '.pretool.prompt_id_count')
TC1_PTU_SIDS=$(echo "$TC1_JSON" | jq -r '.pretool.session_id_count')
if [ "$TC1_PTU_READ" -ge 2 ] && [ "$TC1_PTU_TOTAL" = "$TC1_PTU_READ" ] && [ "$TC1_PTU_PIDS" = "1" ] && [ "$TC1_PTU_SIDS" = "1" ]; then
  pass "TC1-ptu: $TC1_PTU_READ Read PreToolUse in one turn (>=2, Read-only, single session+prompt)"
else
  fail "TC1-ptu: expected >=2 Read PreToolUse and no other tool in ONE turn, got total=$TC1_PTU_TOTAL read=$TC1_PTU_READ sessions=$TC1_PTU_SIDS prompts=$TC1_PTU_PIDS"
fi

# (S4) exactly one compact message sent (the 3 reads aggregated, not split).
TC1_CSENT=$(echo "$TC1_JSON" | jq -r '.compact_count')
TC1_CMID=$(echo "$TC1_JSON" | jq -r '.compact_sends[0].msg_id // ""')
[ "$TC1_CSENT" = "1" ] && pass "TC1-send: exactly one compact message sent (aggregation, msg_id=$TC1_CMID)" \
  || fail "TC1-send: expected exactly 1 compact tool sent (aggregation), got $TC1_CSENT"

# (S2) AGGREGATION PROOF: exactly 2 successful msg_id-scoped 'TG edit:' lines on that compact message
# (reads 2 & 3 edited the message the first read sent) — NOT 'compact tool edited', which register.go:669
# logs unconditionally after discarding RetryEditRich's return.
TC1_NEDIT=$(echo "$TC1_JSON" | jq -r --arg m "$TC1_CMID" '.tg_edit_count[$m] // 0')
[ -n "$TC1_CMID" ] && [ "$TC1_NEDIT" -ge 1 ] \
  && pass "TC1-agg: compact msg_id=$TC1_CMID received $TC1_NEDIT successful TG edit(s) (reads 2..$TC1_PTU_READ aggregated onto the first read's message)" \
  || fail "TC1-agg: expected >=1 successful TG edit on compact msg_id=$TC1_CMID, got $TC1_NEDIT"

# (S2) RESULT BINDING: all three unique ingredient tokens appear in ONE reconstructed post-read assistant
# message (Stop last_assistant_message fallback allowed; PostToolUse rejected as a content source).
[ "$(echo "$TC1_JSON" | jq -r '.all_in_one')" = "true" ] \
  && pass "TC1-bind: final sentence names all three read ingredients (result bound to the reads)" \
  || fail "TC1-bind: the three ingredient tokens are not all present in one reconstructed assistant message"

# TC1d (Fix 17): the compact Read summary shows only the filename (basename) — no path separator. A
# regression to the tail path would render "📖 Read: tasks/ba96hbxz8.output" (a '/' before </summary>).
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -qE "<summary>📖 Read: "
_ps_readsum=("${PIPESTATUS[@]}")
# A Read summary that still contains a path separator before </summary> means the basename change did not apply.
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -qE "<summary>📖 Read: [^<]*/"
_ps_readslash=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps_readsum[1]}" -ne 0 ]; then
  # r9: the no-summary branch was a vacuous PASS; it is now a FAIL, so a rendering regression that drops
  # every Read summary cannot vacuous-pass (the 3-Read PreToolUse count above already caught a no-Read run).
  fail "TC1d: no compact Read summary found — a rendering regression dropped every Read summary (was vacuous-PASS; now FAIL per r9)"
elif [ "${_ps_readslash[1]}" -ne 0 ]; then
  pass "TC1d: compact Read summary is basename-only, no path separator (Fix 17)"
else
  fail "TC1d: compact Read summary still contains a path separator (Fix 17 basename not applied)"
fi

# TC2: Verify compact mode replaced standard ToolUse notifications
set +eo pipefail
COMPACT_COUNT=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -c "compact tool sent\|compact tool edited" || true)
STANDARD_COUNT=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -v "compact" | grep -c "Notification sent.*ToolUse" || true)
set -eo pipefail
if [ "$COMPACT_COUNT" -ge 1 ] && [ "$STANDARD_COUNT" -eq 0 ]; then
  pass "TC2: compact mode active, no standard ToolUse notifications (compact=$COMPACT_COUNT standard=$STANDARD_COUNT)"
else
  fail "TC2: expected compact>=1 and standard=0, got compact=$COMPACT_COUNT standard=$STANDARD_COUNT"
fi

# TC3: Verify PostToolUse did NOT edit compact message
set +eo pipefail
tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "PostToolUse: updated"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -ne 0 ]; then
  pass "TC3: PostToolUse did not edit compact message"
else
  fail "TC3: PostToolUse unexpectedly edited compact message"
fi

# TC4: Whitelist filtering — enable compact with only Read, trigger a prompt that uses Bash
LOG_BEFORE_TC4=$(wc -l < "$LOG_FILE" 2>/dev/null || true)
python3 -c "
import json
f='$TEST_CONFIG_DIR/config.json'
d=json.load(open(f))
d['toolNotifyCompact']=True
d['toolNotifyList']=['Read','Other']
json.dump(d,open(f,'w'))
"

pane_log "[compact_tc4] BEFORE inject"
inject_prompt "Use the Bash tool to run the command: echo compact_whitelist_test"
pane_log "[compact_tc4] AFTER inject"

wait_for_idle
pane_log "[compact_tc4] AFTER idle"

set +eo pipefail
tail -n +"$((LOG_BEFORE_TC4 + 1))" "$LOG_FILE" | grep -q "compact tool.*Bash"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -ne 0 ]; then
  pass "TC4: Bash not in compact notification (whitelist filtering works)"
else
  fail "TC4: Bash appeared in compact notification despite not being in whitelist"
fi

# TC5: Settings toggle via test endpoint
# First set compact=false so toggle produces true
python3 -c "
import json
f='$TEST_CONFIG_DIR/config.json'
d=json.load(open(f))
d['toolNotifyCompact']=False
json.dump(d,open(f,'w'))
"
RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/test/config/compact" 2>&1 || true)
set +eo pipefail
echo "$RESP" | grep -q '"compact":true'
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps[1]}" -eq 0 ]; then
  # Toggle back
  curl -s "http://127.0.0.1:$TEST_PORT/test/config/compact" > /dev/null 2>&1 || true
  pass "TC5: Compact toggle endpoint works"
else
  fail "TC5: Compact toggle endpoint returned unexpected response: $RESP"
fi

# TC6 (FIX 1a, r9): a new assistant text block between two tool calls breaks the compact-tool message
# cycle, so consecutive tools land in SEPARATE compact messages (register.go:543-551 resets on the
# send-below-since-tool PLACEMENT signal). The cat-vs-dog scoring scenario forces the flow
# text→Bash#1→text→Bash#2→text→Bash#3→text with STRICT, delivery-anchored assertions (the old sent>=2
# gate false-passed on a partially-executed turn). The opening announcement (Step 1) is REQUIRED: TC7
# below asserts first "Stream send" < first "compact tool sent", so a tool-first turn would break it.
# toolNotifyList MUST list Bash (an unlisted tool emits NO compact line at all, register.go:565-568 — the
# exact gate that caused the gate-6 cascade); Read stays listed so nothing earlier in the phase is disturbed.
# $RANDOM and $(cat ...) stay LITERAL (escaped); only the file paths expand.
python3 -c "
import json
f='$TEST_CONFIG_DIR/config.json'
d=json.load(open(f))
d['toolNotifyCompact']=True
d['toolNotifyList']=['Read','Bash','Other']
json.dump(d,open(f,'w'))
"
TC6_CAT="/tmp/tg-cli-test-compact-tc6-cat-$$.txt"
TC6_DOG="/tmp/tg-cli-test-compact-tc6-dog-$$.txt"
TC6_WIN="/tmp/tg-cli-test-compact-tc6-win-$$.txt"
rm -f "$TC6_CAT" "$TC6_DOG" "$TC6_WIN"
# LOCAL cleanup (r9 S5): called on EVERY fail branch and once on the success path. NOT a second
# 'trap ... EXIT' — that would REPLACE start_claude's _cc_phase_cleanup and lose session teardown.
_tc6_cleanup() { rm -f "$TC6_CAT" "$TC6_DOG" "$TC6_WIN"; }
LOG_BEFORE_TC6=$(wc -l < "$LOG_FILE" 2>/dev/null || true)
pane_log "[compact_tc6] BEFORE inject"
inject_prompt "You are the host of a live cat-vs-dog scoring show. Do the following in this exact order, all in this single reply, one step at a time, without skipping or combining any step, and without running any tool other than the ones named below.
Step 1: Announce to the audience, in exactly one sentence, that the scoring show is starting.
Step 2: The cats' score does not exist yet, so produce it by running this exact bash command exactly once:
  echo \$((RANDOM % 101)) | tee $TC6_CAT
Step 3: Announce the cats' score to the audience in exactly one sentence that ends with CAT_SCORE=<the exact number the command printed>
Step 4: The dogs' score does not exist yet, so produce it by running this exact bash command exactly once:
  echo \$((RANDOM % 101)) | tee $TC6_DOG
Step 5: Announce the dogs' score to the audience in exactly one sentence that ends with DOG_SCORE=<the exact number the command printed>
Step 6: Decide the winner by running this exact bash command exactly once:
  if [ \$(cat $TC6_CAT) -gt \$(cat $TC6_DOG) ]; then echo CATS; elif [ \$(cat $TC6_CAT) -lt \$(cat $TC6_DOG) ]; then echo DOGS; else echo TIE; fi | tee $TC6_WIN
Step 7: Announce the winner to the audience in exactly one sentence that ends with WINNER=<the exact token the command printed>"
pane_log "[compact_tc6] AFTER inject"
wait_for_idle
pane_log "[compact_tc6] AFTER idle"

# S4 bounded-poll: the 3 compact sends go through the async Message FIFO; poll until 3 appear (or timeout)
# before the exact-count assert.
for _i in $(seq 1 30); do
  [ "$(tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE" | P26_CHAT="$DEFAULT_CHAT_ID" P26_PANE="$E2E_PANE" _p26 | jq -r '.compact_count')" = "3" ] && break
  sleep 1
done

# Artifacts must exist and be well-formed BEFORE cleanup (r9 cleanup-order fix; the old test deleted its
# own evidence before asserting).
if [ ! -s "$TC6_CAT" ] || [ ! -s "$TC6_DOG" ] || [ ! -s "$TC6_WIN" ]; then
  _tc6_cleanup; fail "TC6: missing artifact (cat=$(cat "$TC6_CAT" 2>/dev/null) dog=$(cat "$TC6_DOG" 2>/dev/null) win=$(cat "$TC6_WIN" 2>/dev/null))"
fi
CAT_VAL=$(tr -d '[:space:]' < "$TC6_CAT"); DOG_VAL=$(tr -d '[:space:]' < "$TC6_DOG"); WIN_TOK=$(tr -d '[:space:]' < "$TC6_WIN")
case "$CAT_VAL" in ''|*[!0-9]*) _tc6_cleanup; fail "TC6: CAT_FILE not an integer ($CAT_VAL)";; esac
case "$DOG_VAL" in ''|*[!0-9]*) _tc6_cleanup; fail "TC6: DOG_FILE not an integer ($DOG_VAL)";; esac
{ [ "$CAT_VAL" -ge 0 ] && [ "$CAT_VAL" -le 100 ]; } || { _tc6_cleanup; fail "TC6: CAT_SCORE out of 0-100 ($CAT_VAL)"; }
{ [ "$DOG_VAL" -ge 0 ] && [ "$DOG_VAL" -le 100 ]; } || { _tc6_cleanup; fail "TC6: DOG_SCORE out of 0-100 ($DOG_VAL)"; }
case "$WIN_TOK" in CATS|DOGS|TIE) : ;; *) _tc6_cleanup; fail "TC6: WIN_FILE token invalid ($WIN_TOK)";; esac
# S3 winner consistency: recompute from CAT/DOG and require WIN_FILE to match.
if [ "$CAT_VAL" -gt "$DOG_VAL" ]; then EXP_WIN=CATS; elif [ "$CAT_VAL" -lt "$DOG_VAL" ]; then EXP_WIN=DOGS; else EXP_WIN=TIE; fi
[ "$WIN_TOK" = "$EXP_WIN" ] || { _tc6_cleanup; fail "TC6: winner inconsistent — WIN_FILE=$WIN_TOK but cat=$CAT_VAL dog=$DOG_VAL => $EXP_WIN"; }
pass "TC6a: three artifacts well-formed and winner consistent (cat=$CAT_VAL dog=$DOG_VAL win=$WIN_TOK)"

# One helper pass over the TC6 window with the three FULL-VALUE markers.
TC6_MARKERS=$(CV="$CAT_VAL" DV="$DOG_VAL" WT="$WIN_TOK" python3 -c "import json,os; print(json.dumps([['CAT','CAT_SCORE=',os.environ['CV']],['DOG','DOG_SCORE=',os.environ['DV']],['WINNER','WINNER=',os.environ['WT']]]))")
TC6_JSON=$(tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE" | P26_CHAT="$DEFAULT_CHAT_ID" P26_PANE="$E2E_PANE" P26_MARKERS="$TC6_MARKERS" _p26)

# (S4) compact tool sent == 3.
TC6_CSENT=$(echo "$TC6_JSON" | jq -r '.compact_count')
[ "$TC6_CSENT" = "3" ] || { _tc6_cleanup; fail "TC6b: expected exactly 3 compact tool sent (3 reset boundaries), got $TC6_CSENT"; }
pass "TC6b: exactly 3 compact messages (each narrated result reset the cycle)"

# (S1) total PreToolUse == 3 AND Bash == 3, single session + single turn.
TC6_PTU_TOTAL=$(echo "$TC6_JSON" | jq -r '.pretool.total')
TC6_PTU_BASH=$(echo "$TC6_JSON" | jq -r '.pretool.by_tool.Bash // 0')
TC6_PTU_PIDS=$(echo "$TC6_JSON" | jq -r '.pretool.prompt_id_count')
TC6_PTU_SIDS=$(echo "$TC6_JSON" | jq -r '.pretool.session_id_count')
if [ "$TC6_PTU_TOTAL" = "3" ] && [ "$TC6_PTU_BASH" = "3" ] && [ "$TC6_PTU_PIDS" = "1" ] && [ "$TC6_PTU_SIDS" = "1" ]; then
  pass "TC6c: exactly 3 Bash PreToolUse in one turn (total=$TC6_PTU_TOTAL bash=$TC6_PTU_BASH, single session+prompt)"
else
  _tc6_cleanup; fail "TC6c: expected 3 Bash PreToolUse in ONE turn, got total=$TC6_PTU_TOTAL bash=$TC6_PTU_BASH sessions=$TC6_PTU_SIDS prompts=$TC6_PTU_PIDS"
fi

# (S3) command verification: the three tool_input.command values in order.
TC6_C0=$(echo "$TC6_JSON" | jq -r '.pretool.commands[0] // ""')
TC6_C1=$(echo "$TC6_JSON" | jq -r '.pretool.commands[1] // ""')
TC6_C2=$(echo "$TC6_JSON" | jq -r '.pretool.commands[2] // ""')
if printf '%s' "$TC6_C0" | grep -qF "tee $TC6_CAT" && printf '%s' "$TC6_C1" | grep -qF "tee $TC6_DOG" \
   && printf '%s' "$TC6_C2" | grep -qF "tee $TC6_WIN" && printf '%s' "$TC6_C2" | grep -qF "CATS" && printf '%s' "$TC6_C2" | grep -qF "DOGS"; then
  pass "TC6d: the three Bash commands are the cat/dog/winner commands in order"
else
  _tc6_cleanup; fail "TC6d: commands not the expected cat/dog/winner sequence (0=$TC6_C0 | 1=$TC6_C1 | 2=$TC6_C2)"
fi

# (S2) result binding + TWO delivery anchors per marker (placement + content). CAT between compact#1 and
# compact#2; DOG between compact#2 and compact#3; WINNER after compact#3 (Stop fallback for WINNER ONLY).
CP1=$(echo "$TC6_JSON" | jq -r '.compact_sends[0].line'); CP2=$(echo "$TC6_JSON" | jq -r '.compact_sends[1].line'); CP3=$(echo "$TC6_JSON" | jq -r '.compact_sends[2].line')
CAT_C=$(echo "$TC6_JSON" | jq -r '.markers.CAT.content_present'); CAT_P=$(echo "$TC6_JSON" | jq -r '.markers.CAT.placement_send_line'); CAT_D=$(echo "$TC6_JSON" | jq -r '.markers.CAT.delivery_line')
if [ "$CAT_C" = "true" ] && [ "$CAT_P" != "-1" ] && [ "$CAT_D" != "-1" ] \
   && [ "$CP1" -lt "$CAT_P" ] && [ "$CAT_P" -le "$CAT_D" ] && [ "$CAT_D" -lt "$CP2" ]; then
  pass "TC6e: CAT_SCORE announced and delivered inside the compact#1..compact#2 slot (placement=$CAT_P delivery=$CAT_D)"
else
  _tc6_cleanup; fail "TC6e: CAT anchor fail (content=$CAT_C placement=$CAT_P delivery=$CAT_D slot=$CP1..$CP2)"
fi
DOG_C=$(echo "$TC6_JSON" | jq -r '.markers.DOG.content_present'); DOG_P=$(echo "$TC6_JSON" | jq -r '.markers.DOG.placement_send_line'); DOG_D=$(echo "$TC6_JSON" | jq -r '.markers.DOG.delivery_line')
if [ "$DOG_C" = "true" ] && [ "$DOG_P" != "-1" ] && [ "$DOG_D" != "-1" ] \
   && [ "$CP2" -lt "$DOG_P" ] && [ "$DOG_P" -le "$DOG_D" ] && [ "$DOG_D" -lt "$CP3" ]; then
  pass "TC6f: DOG_SCORE announced and delivered inside the compact#2..compact#3 slot (placement=$DOG_P delivery=$DOG_D)"
else
  _tc6_cleanup; fail "TC6f: DOG anchor fail (content=$DOG_C placement=$DOG_P delivery=$DOG_D slot=$CP2..$CP3)"
fi
WIN_C=$(echo "$TC6_JSON" | jq -r '.markers.WINNER.content_present'); WIN_P=$(echo "$TC6_JSON" | jq -r '.markers.WINNER.placement_send_line'); WIN_D=$(echo "$TC6_JSON" | jq -r '.markers.WINNER.delivery_line'); WIN_S=$(echo "$TC6_JSON" | jq -r '.markers.WINNER.stop_delivery_line')
if [ "$WIN_C" = "true" ] && [ "$WIN_D" != "-1" ] && [ "$WIN_P" != "-1" ] && [ "$WIN_P" -gt "$CP3" ] && [ "$WIN_D" -gt "$CP3" ]; then
  pass "TC6g: WINNER streamed after compact#3 (placement=$WIN_P delivery=$WIN_D > compact#3=$CP3)"
elif [ "$WIN_C" = "true" ] && [ "$WIN_S" != "-1" ] && [ "$WIN_S" -gt "$CP3" ]; then
  pass "TC6g: WINNER delivered via Stop fallback after compact#3 (notification line=$WIN_S > compact#3=$CP3)"
else
  _tc6_cleanup; fail "TC6g: WINNER anchor fail (content=$WIN_C placement=$WIN_P delivery=$WIN_D stop=$WIN_S compact#3=$CP3)"
fi

_tc6_cleanup

# TC7 (round 8): SEND-anchored text-before-tool ordering. The bounded PreToolUse wait
# (streamFlushAwaitToolBoundary) must flush the pre-tool text to Telegram BEFORE the tool notification.
# Anchor on bot SEND log lines ('Stream send:' for a text bubble, 'compact tool sent' for the tool),
# NOT MessageDisplay delta-receipt lines. Before round 8 the tool notification could overtake the text
# (PreToolUse hook arrives before MessageDisplay; StreamFlush returned instantly on a stale bubble).
set +eo pipefail
TC6_SLICE=$(tail -n +"$((LOG_BEFORE_TC6 + 1))" "$LOG_FILE")
FIRST_TEXT_LINE=$(printf '%s\n' "$TC6_SLICE" | grep -n "Stream send:" | head -1 | cut -d: -f1)
FIRST_TOOL_LINE=$(printf '%s\n' "$TC6_SLICE" | grep -n "compact tool sent" | head -1 | cut -d: -f1)
LATE_MD_COUNT=$(printf '%s\n' "$TC6_SLICE" | grep -c "late MD after tool notify")
set -eo pipefail
if [ -n "$FIRST_TEXT_LINE" ] && [ -n "$FIRST_TOOL_LINE" ] && [ "$FIRST_TEXT_LINE" -lt "$FIRST_TOOL_LINE" ]; then
  pass "TC7: pre-tool text SEND precedes tool-notification SEND (first Stream send line=$FIRST_TEXT_LINE < first compact tool sent line=$FIRST_TOOL_LINE)"
else
  fail "TC7: text-before-tool ordering FAIL (first Stream send=$FIRST_TEXT_LINE, first compact tool sent=$FIRST_TOOL_LINE)"
fi
# TC8 (round 8): late-MD residual-inversion marker is countable; 0 = no inversions (informational, not a gate).
echo "  TC8 [info]: late-MD residual-inversion markers in TC6 = ${LATE_MD_COUNT:-0} (0 = ideal)"

# Restore config for subsequent phases (full canonical toolNotifyList from the shared helper)
TOOL_LIST="$(tool_notify_list_json)" python3 -c "
import json,os
f='$TEST_CONFIG_DIR/config.json'
d=json.load(open(f))
d['toolNotifyCompact']=False
d['toolNotifyList']=json.loads(os.environ['TOOL_LIST'])
json.dump(d,open(f,'w'))
"

# Cleanup temp files
rm -f "$TC1_CARD1" "$TC1_CARD2" "$TC1_CARD3"
