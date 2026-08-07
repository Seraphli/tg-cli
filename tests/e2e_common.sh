#!/bin/bash

# Guard against double-sourcing
[ -n "${_E2E_COMMON_LOADED:-}" ] && return 0
_E2E_COMMON_LOADED=1

# Unset CC-injected env vars to prevent inheritance by E2E tmux server
unset CLAUDE_CODE_CHILD_SESSION CLAUDE_CODE_SESSION_ID CLAUDECODE
unset CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_EXECPATH
unset CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING CLAUDE_CODE_DISABLE_LEGACY_MODEL_REMAP
unset CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL
unset CLAUDE_EFFORT CLAUDE_PLUGIN_DATA
unset AI_AGENT CODEX_COMPANION_SESSION_ID
unset GIT_EDITOR COREPACK_ENABLE_AUTO_PIN

# Per-run isolation via run ID (PID of top-level process, inherited by subprocesses)
E2E_RUN_ID="${E2E_RUN_ID:-$$}"
export E2E_RUN_ID
TMUX_SERVER_NAME="${TMUX_SERVER_NAME:-tg-cli-test-${E2E_RUN_ID}}"
export TMUX_SERVER_NAME
BOT_SESSION="${BOT_SESSION:-tg-cli-e2e-bot-${E2E_RUN_ID}}"
export BOT_SESSION
E2E_SESSION="${E2E_SESSION:-tg-cli-e2e-${E2E_RUN_ID}}"
export E2E_SESSION
export TMUX_TEST="tmux -u -L $TMUX_SERVER_NAME -f /dev/null"

# Canonical toolNotifyList: ALL knownTools (cmd/helpers/version.go) + AskUserQuestion + "Other".
# Single source of truth so every config write uses the same full list (a text -> tool -> text turn
# never trips the V3 ordering check because every tool is a valid separator). Sourcing scripts
# (e.g. cc/phase26) call this to restore the full list instead of duplicating the literal.
tool_notify_list_json() {
  echo '["Edit","Write","Bash","Read","Glob","Grep","Agent","WebFetch","WebSearch","MCP","Skill","TaskCreate","TaskUpdate","TaskGet","TaskList","TaskStop","TaskOutput","NotebookEdit","EnterPlanMode","ExitPlanMode","EnterWorktree","ExitWorktree","AskUserQuestion","Other"]'
}
# Per-run dirs nest under one parent (~/.tg-cli-test/<run-id>) instead of cluttering $HOME with
# ~/.tg-cli-test-<run-id>. The parent doubles as _E2E_SHARED_CONFIG (holds the shared credentials.json);
# the shared creds file and the per-run subdirs coexist, and cleanup only removes the specific run subdir.
TEST_CONFIG_DIR="${TEST_CONFIG_DIR:-$HOME/.tg-cli-test/${E2E_RUN_ID}}"
export TEST_CONFIG_DIR
_E2E_SHARED_CONFIG="$HOME/.tg-cli-test"
mkdir -p "$TEST_CONFIG_DIR"
if [ ! -f "$TEST_CONFIG_DIR/credentials.json" ] && [ -f "$_E2E_SHARED_CONFIG/credentials.json" ]; then
  cp "$_E2E_SHARED_CONFIG/credentials.json" "$TEST_CONFIG_DIR/credentials.json"
fi
TEST_CLAUDE_CONFIG_DIR="${TEST_CLAUDE_CONFIG_DIR:-$TEST_CONFIG_DIR/claude-config}"
export TEST_CLAUDE_CONFIG_DIR
TEST_SETTINGS="$TEST_CLAUDE_CONFIG_DIR/settings.json"
export TEST_SETTINGS
TEST_PORT="${TEST_PORT:-$(python3 -c "import socket; s=socket.socket(); s.bind(('127.0.0.1',0)); print(s.getsockname()[1]); s.close()")}"
export TEST_PORT
LOG_FILE="$TEST_CONFIG_DIR/bot.log"
export LOG_FILE
TYPING_LOG_FILE="$TEST_CONFIG_DIR/typing.log"
export TYPING_LOG_FILE
CREDENTIALS="$TEST_CONFIG_DIR/credentials.json"
export CREDENTIALS
CC_WORKDIR="$TEST_CONFIG_DIR/cwd"
mkdir -p "$CC_WORKDIR"
export CC_WORKDIR
# One common wait budget for all backends, sized for the slowest (codex). Formerly split by E2E_BACKEND;
# flattened per boss ruling 2026-07-31 so a range run gives codex the same 180 the all-phases branch already did.
TIMEOUT=180

# Results tracking via shared file
E2E_RESULTS_FILE="${E2E_RESULTS_FILE:-/tmp/tg-cli-e2e-results-$$.txt}"
export E2E_RESULTS_FILE

pass() { echo "PASS|$1" >> "$E2E_RESULTS_FILE"; echo "  PASS: $1"; }
fail() { echo "FAIL|$1" >> "$E2E_RESULTS_FILE"; echo "  FAIL: $1"; exit 1; }
record_fail() { echo "FAIL|$1" >> "$E2E_RESULTS_FILE"; echo "  FAIL: $1"; }
pass_opt() { echo "OPT_PASS|$1" >> "$E2E_RESULTS_FILE"; echo "  OPT_PASS: $1"; }
warn() { echo "WARN|$1" >> "$E2E_RESULTS_FILE"; echo "  WARN: $1"; }
# Global error-exit logging
set -E

_e2e_last_error=""

_e2e_on_error() {
  local rc="$1"
  local line="$2"
  local cmd="$3"
  local pipestatus="$4"
  _e2e_last_error="rc=$rc line=$line cmd=$cmd pipestatus=$pipestatus"
  echo "ERROR|$_e2e_last_error" >> "$E2E_RESULTS_FILE"
}

_e2e_on_exit() {
  local rc="$?"
  if [ "$rc" -ne 0 ]; then
    echo "EXIT|rc=$rc last_error=${_e2e_last_error:-none}" >> "$E2E_RESULTS_FILE"
    echo "  EXIT: rc=$rc last_error=${_e2e_last_error:-none}"
  fi
}

trap '_e2e_on_error $? "$LINENO" "$BASH_COMMAND" "${PIPESTATUS[*]}"' ERR
trap '_e2e_on_exit' EXIT

# Log pane capture to bot log file via /capture API
# Usage: pane_log "label"
pane_log() {
  local label="$1"
  local target="${2:-${E2E_PANE:-}}"
  if [ -z "$target" ]; then
    echo "  === PANE: $label === (no pane)"
    return
  fi
  local pane_id="${target%@*}"
  local capture
  capture=$($TMUX_TEST capture-pane -t "$pane_id" -p -S - 2>/dev/null || echo "(capture failed)")
  echo "  === PANE: $label ==="
  echo "$capture"
  echo "  === END PANE ==="
}

# Reconstruct the final TG message content from bot-log Stream send/edit lines.
# Streaming logs each TG message (continuation chunk) multiple times as it grows: a 'Stream send'
# then one or more 'Stream edit ... full_text:'. Only the LAST continuation chunk of a turn is logged
# 'final=true'; earlier chunks finalize as 'final=false'. To reconstruct what TG actually shows we must,
# per chunk (msg_id), keep the LATEST full_text — NOT filter on final=true (that would drop every
# non-last chunk's grown content). Chunks are emitted in order, so concatenate them in first-seen order.
# Usage: reconstruct_tg_full_text "$NEW_LOGS" > out.html
reconstruct_tg_full_text() {
  printf '%s\n' "$1" | awk '
    /Stream (send|edit):.*full_text:/ {
      msg=""
      for (i = 1; i <= NF; i++) if ($i ~ /^msg_id=/) { msg=$i; break }
      if (!(msg in seen)) { order[++n]=msg; seen[msg]=1 }
      body[msg]=""
      sub(/.*full_text:/, "")
      if ($0 != "") body[msg]=$0 "\n"
      cur=msg
      next
    }
    cur != "" && /^\[[0-9]{4}-/ { cur=""; next }
    cur != "" { body[cur]=body[cur] $0 "\n" }
    END { for (i = 1; i <= n; i++) printf "%s", body[order[i]] }
  '
}

inject_prompt() {
  local text="$1"
  local image="${2:-}"
  local target="${3:-$E2E_PANE}"
  local api_url="http://127.0.0.1:$TEST_PORT/inject/message"
  local payload
  if [ -n "$image" ]; then
    payload=$(jq -n --arg t "$target" --arg txt "$text" --arg img "$image" \
      '{target: $t, text: $txt, imagePath: $img}')
  else
    payload=$(jq -n --arg t "$target" --arg txt "$text" \
      '{target: $t, text: $txt}')
  fi
  echo "  API call: POST $api_url target=$target text=${text:0:80}..."
  local resp
  resp=$(curl -s -w "\n%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d "$payload" \
    "$api_url")
  local code
  code=$(echo "$resp" | tail -1)
  if [ "$code" != "200" ]; then
    echo "  WARNING: inject/message API returned $code"
    pane_log "[inject_prompt] failed ($code) target=$target"
    return 1
  fi
  return 0
}

wait_for_bot_ready() {
  local timeout=${1:-$TIMEOUT}
  local elapsed=0
  while [ $elapsed -lt $timeout ]; do
    if curl -sf -o /dev/null "http://127.0.0.1:$TEST_PORT/session/idle" 2>/dev/null; then
      echo "  bot ready after ${elapsed}s (port $TEST_PORT responding)"
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  echo "WARN: wait_for_bot_ready timed out after ${timeout}s"
  # Boss directive: a non-starting bot MUST be diagnosable post-hoc. Capture the bot launch command,
  # a live startup probe, the bot tmux pane, and the current bot.log into a per-run file BEFORE the
  # next phase truncates the shared bot.log; also echo the pane so it lands in the tee'd run log.
  local ts diag
  ts=$(date +%Y%m%d-%H%M%S)
  diag="$TEST_CONFIG_DIR/bot-startup-fail-${ts}.log"
  {
    echo "=== wait_for_bot_ready TIMEOUT (${timeout}s) at $ts ==="
    echo "--- last bot launch command ---"
    echo "${_LAST_BOT_LAUNCH_CMD:-<unknown>}"
    echo "--- startup probe: curl http://127.0.0.1:$TEST_PORT/session/idle ---"
    curl -sS -m 5 "http://127.0.0.1:$TEST_PORT/session/idle" 2>&1 || echo "(probe failed: rc=$?)"
    echo ""
    echo "--- bot tmux pane ($BOT_SESSION) ---"
    $TMUX_TEST capture-pane -t "$BOT_SESSION" -p -S - 2>&1 || echo "(pane capture failed)"
    echo ""
    echo "--- bot.log ($LOG_FILE) ---"
    cat "$LOG_FILE" 2>&1 || echo "(no bot.log)"
  } > "$diag" 2>&1
  echo "  Bot startup diagnostics saved to: $diag"
  echo "  === BOT STARTUP FAIL PANE ($BOT_SESSION) ==="
  $TMUX_TEST capture-pane -t "$BOT_SESSION" -p -S - 2>/dev/null || echo "  (pane capture failed)"
  echo "  === END BOT STARTUP FAIL PANE ==="
  return 1
}

# check_session_idle — diagnose a session's busy/idle/unknown state via the idle API.
# Echoes one of: idle | busy | unknown. $1 = tmux target (pane_id@socket_path; empty = aggregate query).
# Parses the tg-cli /session/idle top-level .idle field (True->idle, False->busy, curl-fail/other->
# unknown) — the same shape the wait_for_idle poll and codex_api_idle (codex_common.sh) use. --max-time
# caps a stalled (accepted-but-silent) connection so it can't block forever.
check_session_idle() {
  local target="$1"
  local url="http://127.0.0.1:$TEST_PORT/session/idle"
  if [ -n "$target" ]; then
    local enc
    enc=$(printf '%s' "$target" | python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read()))")
    url="${url}?target=${enc}"
  fi
  local resp
  resp=$(curl -sf --max-time 5 "$url" 2>/dev/null) || { echo "unknown"; return; }
  local val
  val=$(printf '%s' "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('idle',''))" 2>/dev/null) || { echo "unknown"; return; }
  case "$val" in
    True)  echo "idle" ;;
    False) echo "busy" ;;
    *)     echo "unknown" ;;
  esac
}

# wait_for_idle — wait for a session to go idle, with a CA-style busy-extend so genuinely-slow models
# (e.g. grok-4.5) don't trip a hard timeout while the LLM is still computing. Polls /session/idle every
# 1s for a `timeout` window; on window-timeout it diagnoses via check_session_idle:
#   busy    -> extend by another window, up to max_retries=3 additional rounds (budget ~timeout*4)
#   idle    -> condition met (return 0). For wait_for_idle the wait condition IS idle, so an idle
#              diagnosis is SUCCESS, not a functional failure (differs from CA wait_for_event on purpose,
#              where idle-at-timeout means "pattern never emitted"). No double-poll: the inner loop
#              already hits the idle API, so check_session_idle runs once after the window (Occam).
#   unknown -> API unavailable: fail (original timeout behavior).
# TIMEOUT constants (60 CC / 180 codex) are UNCHANGED; the busy-extend handles slowness automatically.
# All curls use --max-time 5 so a stalled connection can't make the budget a lie — approximate budget =
# timeout*windows plus bounded overhead (the 5s settle sleep, and up to --max-time per stalled poll).
wait_for_idle() {
  # Leading settle: an inject confirms (HTTP 200) ~1s before the TUI busy state
  # propagates, so a first idle-poll fired immediately can read STALE idle and
  # return early — the next inject then lands mid-turn, is queued, and the 5s
  # MD-final routing window co-expires with the settle sleep so the UPS grep runs
  # before submit (full7 phase19 image-only killer: 200 @10:38:10, UPS @10:38:11,
  # first poll saw idle -> early return). Sleeping 5s before the first poll lets
  # busy propagate, so we wait for the real turn end. Covers every callsite incl.
  # the codex phase14 twin (phase14:21/50/81 call this same helper).
  sleep 5
  local timeout=${1:-$TIMEOUT}
  local target=${2:-}
  local url="http://127.0.0.1:$TEST_PORT/session/idle"
  if [ -n "$target" ]; then
    local encoded_target
    encoded_target=$(printf '%s' "$target" | python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read()))")
    url="${url}?target=${encoded_target}"
  fi
  local max_retries=3
  local retry=0
  while true; do
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
      local idle
      idle=$(curl -sf --max-time 5 "$url" 2>/dev/null \
        | python3 -c "import sys,json; print(json.load(sys.stdin).get('idle',False))" 2>/dev/null) || true
      if [ "$idle" = "True" ]; then
        sleep 5
        return 0
      fi
      sleep 1
      elapsed=$((elapsed + 1))
    done
    # Inner window timed out — diagnose busy vs idle vs unknown.
    local diagnosis
    diagnosis=$(check_session_idle "$target")
    if [ "$diagnosis" = "busy" ] && [ $retry -lt $max_retries ]; then
      retry=$((retry + 1))
      echo "  BUSY: LLM still processing, extending wait (round ${retry}/${max_retries}, +${timeout}s)..."
      continue
    fi
    if [ "$diagnosis" = "idle" ]; then
      # Session went idle right at the window boundary — condition met.
      sleep 5
      return 0
    fi
    break   # unknown, or busy retries exhausted
  done
  local total=$(( timeout * (retry + 1) ))
  local debug_resp debug_target
  debug_resp=$(curl -sf --max-time 5 "$url" 2>/dev/null || echo '{"error":"curl failed"}')
  debug_target="${target:-${E2E_PANE:-}}"
  pane_log "wait_for_idle TIMEOUT after ~${total}s (${retry}/${max_retries} busy-extends, idle_resp: $debug_resp)" "$debug_target"
  fail "wait_for_idle timed out after ~${total}s (${retry}/${max_retries} busy-extends) on target=${debug_target:-<none>} (idle_resp: $debug_resp)"
}

# wait_for_pane_content — poll the pane for a text pattern. NOTE: its `return 1` on timeout IS fatal to
# callers under `set -euo pipefail` when called unguarded (e.g. phase10_escape.sh:88, no `|| true`), so
# it is NOT "non-fatal". It is intentionally left WITHOUT the busy-extend anyway: its callers invoke it
# AFTER wait_for_idle (model slowness already absorbed) and it waits for pane-content PROPAGATION (a
# sub-second TUI render), not LLM computation — so it does not hit its timeout in practice, and the
# busy-extend (an LLM-slowness tool) is the wrong instrument here.
wait_for_pane_content() {
  local pattern="$1"
  local timeout=${2:-$TIMEOUT}
  local target=${3:-$E2E_PANE}
  local encoded_target
  encoded_target=$(printf '%s' "$target" | python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read()))")
  local elapsed=0
  while [ $elapsed -lt $timeout ]; do
    local content
    content=$(curl -sf "http://127.0.0.1:$TEST_PORT/capture?target=${encoded_target}" 2>/dev/null) || true
    set +eo pipefail
    echo "$content" | grep -q "$pattern" 2>/dev/null
    _ps=("${PIPESTATUS[@]}")
    set -eo pipefail
    if [ "${_ps[1]}" -eq 0 ]; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  echo "WARN: wait_for_pane_content('$pattern') timed out after ${timeout}s"
  return 1
}

# Check typing continuity between inject and end event.
# Usage: check_typing_continuity <typing_log_before> <end_event> <label>
#   end_event: "Stop" or "PreToolUse"
check_typing_continuity() {
  local typing_log_before="$1"
  local end_event="$2"
  local label="$3"
  local new_entries
  new_entries=$(tail -n +"$((typing_log_before + 1))" "$TYPING_LOG_FILE")
  # Find T1 (UserPromptSubmit) and T2 (end_event) timestamps
  local t1_ts t2_ts
  t1_ts=$(echo "$new_entries" | grep "state: event=UserPromptSubmit" | head -1 | grep -oP '^\[\K[^]]+' || true)
  t2_ts=$(echo "$new_entries" | grep "state: event=$end_event" | head -1 | grep -oP '^\[\K[^]]+' || true)
  if [ -z "$t1_ts" ] || [ -z "$t2_ts" ]; then
    fail "[$label] Typing: missing state timestamps (T1=${t1_ts:-none} T2=${t2_ts:-none})"
    return
  fi
  local t1_epoch t2_epoch duration
  t1_epoch=$(date -d "$t1_ts" +%s 2>/dev/null || echo 0)
  t2_epoch=$(date -d "$t2_ts" +%s 2>/dev/null || echo 0)
  duration=$((t2_epoch - t1_epoch))
  # Count typing SEND ATTEMPTS (ticker fired) between T1 and T2. The tick line is logged before
  # the HTTP sendChatAction call, so a transient network send failure does NOT create a gap — only
  # a missed attempt would. 3s ticker, so the expected count math below (duration/3 - 2) is unchanged.
  local typing_timestamps
  typing_timestamps=$(echo "$new_entries" | grep 'tick:.*sending=true' | grep -oP '^\[\K[^]]+' || true)
  local count=0 max_gap=0 prev_epoch=""
  if [ -n "$typing_timestamps" ]; then
    while IFS= read -r ts; do
      local epoch
      epoch=$(date -d "$ts" +%s 2>/dev/null || echo "")
      if [ -z "$epoch" ]; then continue; fi
      if [ "$epoch" -ge "$t1_epoch" ] && [ "$epoch" -le "$t2_epoch" ]; then
        count=$((count + 1))
        if [ -n "$prev_epoch" ]; then
          local gap=$((epoch - prev_epoch))
          if [ "$gap" -gt "$max_gap" ]; then max_gap=$gap; fi
        fi
        prev_epoch="$epoch"
      fi
    done <<< "$typing_timestamps"
  fi
  # Expected count: duration/3 with margin of 2
  local expected=1
  if [ "$duration" -gt 0 ]; then
    expected=$((duration / 3 - 2))
    if [ "$expected" -lt 1 ]; then expected=1; fi
  fi
  if [ "$count" -ge "$expected" ]; then
    pass "[$label] Typing continuity: $count actions in ${duration}s (expected >= $expected)"
  else
    fail "[$label] Typing continuity: $count actions in ${duration}s (expected >= $expected)"
  fi
  # Max gap check (only meaningful with >= 2 entries)
  if [ "$count" -ge 2 ]; then
    if [ "$max_gap" -le 5 ]; then
      pass "[$label] Typing gap: max ${max_gap}s (<= 5s)"
    else
      fail "[$label] Typing gap: max ${max_gap}s (> 5s)"
    fi
  fi
}

# validate_phase_log: after a CC phase completes, validate that phase's bot-log slice for
#   V1 - hook raw-payload completeness (every "Raw hook payload [E]:" has a JSON body; MessageDisplay too)
#   V2 - TG message content completeness (stream full_text non-empty; no truncated "Notification sent" body)
#   V3 - message ordering (no two consecutive DISTINCT Message-type sends without a separator send between)
# CC-scoped: skips phases with no CC streaming markers (codex/common phases unaffected). Failures call fail.
# Usage: validate_phase_log <label> <log_before_phase>
validate_phase_log() {
  local label="$1"
  local log_before="$2"
  local pane_target="${3:-${E2E_PANE:-}}"
  local slice
  slice=$(tail -n +"$((log_before + 1))" "$LOG_FILE" 2>/dev/null || true)
  # CC-scoped: only validate phases that produced CC streaming output. Use a here-string, NOT
  # `printf '%s\n' "$slice" | grep -q ...`: under `set -o pipefail`, grep -q closing the pipe on its first
  # match makes printf exit SIGPIPE(141), so the pipeline reads as failure and the check would WRONGLY skip
  # large phases (e.g. streaming) where the marker appears early. (Project lesson 2026-02-14.)
  if ! grep -q -e "Stream send:" -e "MessageDisplay delta:" <<< "$slice"; then
    return 0
  fi

  # --- V1: hook raw-payload completeness ---
  local bad_raw
  bad_raw=$(printf '%s\n' "$slice" | grep -E 'Raw hook payload \[[^]]+\]:' | grep -vE 'Raw hook payload \[[^]]+\]: *\{' || true)
  if [ -n "$bad_raw" ]; then
    echo "  [validate_phase_log:$label] V1 FAIL — raw-payload line(s) without a JSON body:"
    printf '%s\n' "$bad_raw" | head -5
    pane_log "[$label] V1 FAIL pane" "$pane_target"
    fail "[$label] V1 raw-payload completeness: empty/missing raw payload"
  fi
  if grep -q "MessageDisplay delta:" <<< "$slice"; then
    if ! grep -q 'Raw hook payload \[MessageDisplay\]:' <<< "$slice"; then
      pane_log "[$label] V1 FAIL pane" "$pane_target"
      fail "[$label] V1 raw-payload completeness: MessageDisplay deltas present but no 'Raw hook payload [MessageDisplay]' line (bot must run with --debug)"
    fi
  fi

  # --- V2: TG message content completeness (no truncation) ---
  if grep -q "Stream send:" <<< "$slice"; then
    local recon
    recon=$(reconstruct_tg_full_text "$slice")
    if [ -z "$(printf '%s' "$recon" | tr -d '[:space:]')" ]; then
      pane_log "[$label] V2 FAIL pane" "$pane_target"
      fail "[$label] V2 content completeness: Stream send lines present but reconstructed full_text is empty"
    fi
  fi
  # No "Notification sent" line shows the old truncation signature (large body_len but a ~200-rune+'...' body).
  # Conservative threshold (body_len>220 && on-line body<=210 chars && ends with '...') to avoid false positives.
  local trunc
  trunc=$(printf '%s\n' "$slice" | awk '
    /^\[[0-9]{4}-/ && /Notification sent .*body_len=[0-9]+ body=/ {
      if (match($0, /body_len=[0-9]+/)) {
        blen = substr($0, RSTART + 9, RLENGTH - 9) + 0
        idx = index($0, " body=")
        body = substr($0, idx + 6)
        if (blen > 220 && length(body) <= 210 && body ~ /\.\.\.$/) print
      }
    }' || true)
  if [ -n "$trunc" ]; then
    echo "  [validate_phase_log:$label] V2 FAIL — truncated 'Notification sent' line(s):"
    printf '%s\n' "$trunc" | head -5
    pane_log "[$label] V2 FAIL pane" "$pane_target"
    fail "[$label] V2 content completeness: truncated Notification sent body"
  fi

  # --- V3: message ordering (Problem-1 regression guard) ---
  # The Problem-1 bug is WITHIN one assistant turn (the CompactTools cycle is per-turn, reset at Stop):
  # a merged tool leaves two text bubbles adjacent with no tool between. So flag two distinct CC
  # message_id= "Stream send:" lines ONLY when they share the SAME turn_id with no separator between.
  # A turn_id change is a new user turn = a legitimate boundary (cross-turn text replies are normal).
  # Pagination chunks share message_id= -> counted once. Separator = a tool/interactive TG SEND, or an AskUserQuestion answer (responded) landing mid-turn.
  # f20 exemption (boss ruling): when the intervening tool's notification was DELIBERATELY disabled by
  # config, the missing separator is EXPECTED. A test phase that drops a tool from toolNotifyList (e.g.
  # phase13's ptu-flush sub-test) makes the bot log "ToolUse: notification disabled for tool=<T>" and emit
  # NO ToolUse separator, so a text->tool->text turn legitimately shows two adjacent same-turn sends. Scope
  # the evidence per turn: track the current turn_id from any turn-tagged line (MessageDisplay delta and
  # Stream send both carry turn_id=); a "notification disabled" line marks that turn. The disabled line can
  # precede the pair's first send (PreToolUse processed while the first delta was in flight), so keying on
  # the turn (not on position between the two sends) is required. A flagged pair whose turn is marked is
  # EXEMPTED with a visible NOTE (no silent pass). Production keeps every tool in toolNotifyList, so the
  # disabled line never appears and V3 stays fully sensitive.
  local v3raw v3notes v3viol
  v3raw=$(printf '%s\n' "$slice" | awk '
    !/^\[[0-9]{4}-/ { next }   # only real log lines (skip multi-line full_text content)
    { if (match($0, /turn_id=[^ ]+/)) curtid = substr($0, RSTART, RLENGTH) }   # current turn from any turn-tagged line
    /ToolUse: notification disabled for tool=/ { if (curtid != "") td[curtid] = 1; next }   # f20: mark this turn tool-notify-disabled
    /compact tool sent|compact tool overflow sent/ { last = "SEP"; next }
    /Notification sent .*: ToolUse / { last = "SEP"; next }
    /AskUserQuestion sent: msg_id=/ { last = "SEP"; next }
    /AskUserQuestion responded: msg_id=/ { last = "SEP"; next }   # cross-turn AskQ: an answered AskQ landing between two same-turn bubbles is a valid separator
    /Permission request sent:/ { last = "SEP"; next }
    /Stream send: / {
      mid = ""; tid = ""
      if (match($0, /message_id=[^ ]+/)) mid = substr($0, RSTART, RLENGTH)
      if (match($0, /turn_id=[^ ]+/)) tid = substr($0, RSTART, RLENGTH)
      if (mid == lastmid) next   # same logical message (pagination) — not a new Message event
      if (tid != lasttid) { last = "M"; lastmid = mid; lasttid = tid; next }   # new turn = boundary, not a violation
      if (last == "M") {
        if (td[tid])   # f20: tool notification disabled by config in this turn -> adjacency is expected
          print "NOTE: V3 exemption - same-turn consecutive sends accepted, ToolUse notification disabled by config in this turn (" tid "): " lastmid " -> " mid
        else
          print "consecutive Message sends in same " tid " with no separator: " lastmid " -> " mid
      }
      last = "M"; lastmid = mid
      next
    }' || true)
  v3notes=$(printf '%s\n' "$v3raw" | grep "^NOTE:" || true)
  v3viol=$(printf '%s\n' "$v3raw" | grep "^consecutive " || true)
  if [ -n "$v3notes" ]; then
    echo "  [validate_phase_log:$label] V3 exemption(s) applied (ToolUse notification disabled by config):"
    printf '%s\n' "$v3notes" | head -5
  fi
  if [ -n "$v3viol" ]; then
    echo "  [validate_phase_log:$label] V3 FAIL — message ordering:"
    printf '%s\n' "$v3viol" | head -5
    pane_log "[$label] V3 FAIL pane" "$pane_target"
    fail "[$label] V3 message ordering: $(printf '%s' "$v3viol" | head -1)"
  fi

  pass "[$label] log validations V1/V2/V3"
}

# validate_phase_inline: run the V1/V2/V3 log validations from WITHIN a phase, while its CC pane is
# still alive, so a FAIL capture shows what CC was doing (run_phase runs after the phase's cleanup has
# already killed the pane -> "(no pane)"). Touches a flag file so run_phase skips its post-phase
# fallback validation (no double count). Call at the end of a CC phase, before it tears down its CC
# session. Uses PHASE_LOG_BEFORE (exported by run_phase) as the phase's starting log offset.
# Usage: validate_phase_inline <pane_target>
validate_phase_inline() {
  local pane_target="${1:-${E2E_PANE:-}}"
  # Ensure CC has gone idle before validating: a still-busy pane means the phase injected/checked with a
  # timing gap and the log may be incomplete. wait_for_idle fails the phase if CC never settles (timing bug).
  if [ -n "$pane_target" ]; then
    wait_for_idle "${TIMEOUT:-180}" "$pane_target"
  fi
  validate_phase_log "$(basename "$0")" "${PHASE_LOG_BEFORE:-0}" "$pane_target"
  touch "$TEST_CONFIG_DIR/.phase-validated" 2>/dev/null || true
}

ensure_credentials() {
  if [ ! -f "$CREDENTIALS" ]; then
    echo "ERROR: $CREDENTIALS not found. Complete pairing first."
    exit 1
  fi
  export DEFAULT_CHAT_ID=$(jq -r '.pairingAllow.defaultChatId // empty' "$CREDENTIALS")
  if [ -z "$DEFAULT_CHAT_ID" ]; then
    echo "ERROR: No defaultChatId in credentials. Complete pairing first."
    exit 1
  fi
  echo "Paired chat ID: $DEFAULT_CHAT_ID"
}

start_bot() {
  # Boss directive: preserve the previous phase's bot.log before truncating, so a prior phase's bot
  # output survives across per-phase (separate-invocation) runs instead of being overwritten.
  if [ -s "$LOG_FILE" ]; then
    local prev_archive
    prev_archive="$TEST_CONFIG_DIR/bot-prev-$(date +%Y%m%d-%H%M%S).log"
    cp "$LOG_FILE" "$prev_archive" 2>/dev/null && echo "  Previous bot.log archived to: $prev_archive"
  fi
  > "$LOG_FILE"
  > "$TYPING_LOG_FILE"
  # Clean stale state from previous runs
  rm -f /tmp/.tg-cli-test/pending/*.json 2>/dev/null || true
  rm -f "$TEST_CONFIG_DIR/inject-queue.json" "$TEST_CONFIG_DIR/merge-buffers.json" "$TEST_CONFIG_DIR/sessions.json" "$TEST_CONFIG_DIR/at-channels.json" 2>/dev/null || true
  rm -rf "$TEST_CONFIG_DIR/mailbox" 2>/dev/null || true
  # Record the launch command so wait_for_bot_ready's failure diagnostics can report it.
  _LAST_BOT_LAUNCH_CMD="cd $(pwd) && ./tg-cli --config-dir $TEST_CONFIG_DIR bot --port $TEST_PORT --tmux-server $TMUX_SERVER_NAME --debug 2>&1"
  export _LAST_BOT_LAUNCH_CMD
  echo "  Bot launch command: $_LAST_BOT_LAUNCH_CMD"
  $TMUX_TEST new-session -d -s "$BOT_SESSION" 2>/dev/null || true
  $TMUX_TEST send-keys -t "$BOT_SESSION" "$_LAST_BOT_LAUNCH_CMD" Enter
  echo "Waiting for bot to start..."
  wait_for_bot_ready
}

setup_hooks() {
  # Clean previous test config
  rm -rf "$TEST_CLAUDE_CONFIG_DIR" 2>/dev/null || true
  mkdir -p "$TEST_CLAUDE_CONFIG_DIR"
  # Isolate Codex hooks via CODEX_HOME (must NOT be under /tmp — Codex refuses helper binaries there)
  export CODEX_HOME="$TEST_CONFIG_DIR/codex-home"
  rm -rf "$CODEX_HOME" 2>/dev/null || true
  mkdir -p "$CODEX_HOME"
  cp "$HOME/.codex/auth.json" "$CODEX_HOME/auth.json" 2>/dev/null || true
  # Copy credentials (CLAUDE_CONFIG_DIR maps to ~/.claude)
  cp "$HOME/.claude/.credentials.json" "$TEST_CLAUDE_CONFIG_DIR/.credentials.json" 2>/dev/null || true
  # Write minimal .claude.json (skip onboarding, no MCP leak, trust CWD + CC_WORKDIR)
  local cwd_escaped ccwd_escaped
  cwd_escaped=$(python3 -c "import json; print(json.dumps(\"$(pwd)\"))")
  ccwd_escaped=$(python3 -c "import json; print(json.dumps(\"$CC_WORKDIR\"))")
  cat > "$TEST_CLAUDE_CONFIG_DIR/.claude.json" << EOF
{"hasCompletedOnboarding":true,"hasInitOnboardingBeenShown":true,"numStartups":100,"autoUpdates":false,"projects":{$cwd_escaped:{"hasTrustDialogAccepted":true},$ccwd_escaped:{"hasTrustDialogAccepted":true}}}
EOF
  # Install hooks to user-level settings (CLAUDE_CONFIG_DIR/settings.json)
  TEST_SETTINGS="$TEST_CLAUDE_CONFIG_DIR/settings.json"
  echo "" | ./tg-cli --config-dir "$TEST_CONFIG_DIR" install --port "$TEST_PORT" --settings "$TEST_SETTINGS" --skip-tmux
  # Add skipDangerousModePermissionPrompt to settings
  python3 -c "import json;f='$TEST_SETTINGS';d=json.load(open(f));d['skipDangerousModePermissionPrompt']=True;p=d.setdefault('permissions',{});a=p.setdefault('allow',[]);r='Read(//tmp/**)';a.append(r) if r not in a else None;json.dump(d,open(f,'w'),indent=2)"
  # Write test app config
  mkdir -p "$TEST_CONFIG_DIR"
  # Build the claude launch command for bot-launched (session new) CC — e.g. phase23's e2e-integ-agent,
  # which does NOT go through start_claude. Mirror start_claude: BROWSER=none + config dir + provider/model
  # env passthrough + configurable model, so a custom-model run also applies to session-new CC. Values are
  # shell-quoted (printf %q). config.json is written via python3 (env-passed) so shell-escaped values with
  # special chars can't break the JSON.
  local cc_cmd
  cc_cmd="$(cc_launch_cmd --dangerously-skip-permissions)"
  # toolNotifyList = ALL knownTools (cmd/helpers/version.go) + AskUserQuestion + "Other", covering EVERY hook
  # tool category, so every tool use emits a ToolUse notification and every tool that REMAINS available is a
  # valid V3 separator (a text -> tool -> text turn never trips the ordering check). The CC --tools allowlist
  # applied inside cc_launch_cmd is a SEPARATE, narrower mechanism: it restricts which tools the session may
  # call at all (Agent is excluded), independent of this notification classification.
  CC_CMD="$cc_cmd" TOOL_LIST="$(tool_notify_list_json)" python3 -c "import json,os; json.dump({'toolNotifyList':json.loads(os.environ['TOOL_LIST']),'claudeCommand':os.environ['CC_CMD'],'paginationMaxRunes':500}, open('$TEST_CONFIG_DIR/config.json','w'))"
  echo "Hooks installed (isolated config: $TEST_CLAUDE_CONFIG_DIR)."
}

# cc_launch_cmd emits (to stdout, exactly ONE line, nothing else — callers use command substitution) the
# canonical Claude Code launch command used by every CC session the suite starts. perm_flag ($1) is REQUIRED:
# a missing arg fails early under set -u. It forwards the same nine provider/model env vars as the harness
# (printf %q quoted so tokens/URLs with special chars cannot break or inject into the command), preserves the
# ${ANTHROPIC_MODEL:-sonnet} fallback, and pins --tools to an allowlist that STRUCTURALLY removes the Agent
# tool, so a model cannot spawn a background subagent regardless of the prompt. The allowlist literal lives
# ONLY here.
cc_launch_cmd() {
  local perm_flag="$1"
  local prefix="BROWSER=none CLAUDE_CONFIG_DIR=$TEST_CLAUDE_CONFIG_DIR"
  local _v
  for _v in ANTHROPIC_BASE_URL ANTHROPIC_AUTH_TOKEN ANTHROPIC_API_KEY \
            ANTHROPIC_MODEL ANTHROPIC_DEFAULT_OPUS_MODEL ANTHROPIC_DEFAULT_SONNET_MODEL \
            ANTHROPIC_DEFAULT_HAIKU_MODEL CLAUDE_CODE_SUBAGENT_MODEL CLAUDE_CODE_EFFORT_LEVEL; do
    if [ -n "${!_v:-}" ]; then
      prefix="${prefix} ${_v}=$(printf '%q' "${!_v}")"
    fi
  done
  printf '%s\n' "$prefix claude --model ${ANTHROPIC_MODEL:-sonnet} $perm_flag --setting-sources user --no-chrome --tools \"Bash,Read,AskUserQuestion,TaskList\""
}

cleanup_sessions() {
  local exit_code=$?
  echo ""
  echo "Cleaning up..."
  $TMUX_TEST kill-session -t "=$E2E_SESSION" 2>/dev/null || true
  sleep 2
  $TMUX_TEST kill-session -t "=$BOT_SESSION" 2>/dev/null || true
  $TMUX_TEST kill-server 2>/dev/null || true
  if [ $exit_code -eq 0 ]; then
    rm -rf "$TEST_CONFIG_DIR" 2>/dev/null || true
  else
    echo "  Per-run config preserved for debugging: $TEST_CONFIG_DIR"
  fi
  return $exit_code
}

# Build the test binary with diagnostics. Used by BOTH ensure_infrastructure (standalone-phase path)
# and tests/e2e.sh's 3 build sites (orchestrated path), so cwd + binary info is logged at every entry
# point — a wrong cwd (building the master binary instead of the worktree) is then visible post-hoc.
build_test_binary() {
  echo "build_test_binary: cwd=$(pwd)"
  echo "build_test_binary: git toplevel=$(git rev-parse --show-toplevel 2>/dev/null || echo '<not a git repo>')"
  echo "Building binary..."
  go build -o tg-cli 2>&1 || { echo "Build failed (cwd=$(pwd))"; exit 1; }
  echo "Built binary: $(pwd)/tg-cli"
  ls -l tg-cli 2>/dev/null || echo "  WARN: tg-cli binary not found after build"
  # TC14 (v14): the chromedp table-image dependency must be fully removed. A clean build plus a
  # chromedp-free go.mod/go.sum proves the native rich <table> path replaced the PNG renderer.
  if grep -iq chromedp go.mod go.sum 2>/dev/null; then
    echo "Build check FAILED: chromedp still present in go.mod/go.sum after migration"
    exit 1
  fi
  echo "Build check: chromedp-free build confirmed (TC14)"
}

ensure_infrastructure() {
  # The heavy bootstrap runs only in a standalone run; an orchestrated run (E2E_ORCHESTRATED=1) already
  # had it done once by e2e.sh. The config baseline below runs UNCONDITIONALLY at the COMMON TAIL.
  if [ "${E2E_ORCHESTRATED:-}" != "1" ]; then
    ensure_credentials
    build_test_binary
    start_bot
    export LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
    setup_hooks
    trap cleanup_sessions EXIT
  fi
  # FIX 2 (r9): bot-notification config baseline. toolNotifyList / toolNotifyCompact / busyIndicator live
  # in the TEST config.json and are read by the bot daemon per hook event (register.go ShouldNotifyTool);
  # a phase that crashes mid-way (e.g. phase26 TC6 before its config-restore) would otherwise leak its
  # mutated whitelist into every LATER phase (the F1->F2/F4 cascade). Re-baselining these 3 fields at the
  # top of every phase removes that at its source. config.json is created only by setup_hooks, so this MUST
  # run at the tail (after bootstrap in a standalone run; immediately in an orchestrated run, where e2e.sh
  # already created config.json). Written ATOMICALLY (temp file + os.replace, the phase13:378-386 template)
  # because the bot daemon reads config.json continuously; load-merge preserves claudeCommand /
  # paginationMaxRunes. A phase that intentionally mutates these fields (phase13/26/31) does so AFTER this
  # baseline, so it is unaffected.
  TOOL_LIST="$(tool_notify_list_json)" python3 -c "
import json, os, tempfile
path = os.path.join('$TEST_CONFIG_DIR', 'config.json')
with open(path) as f:
    cfg = json.load(f)
cfg['toolNotifyCompact'] = False
cfg['toolNotifyList'] = json.loads(os.environ['TOOL_LIST'])
cfg.pop('busyIndicator', None)
d = os.path.dirname(path)
fd, tmp = tempfile.mkstemp(dir=d, prefix='.config.', suffix='.tmp')
with os.fdopen(fd, 'w') as f:
    json.dump(cfg, f)
os.replace(tmp, path)
"
}
