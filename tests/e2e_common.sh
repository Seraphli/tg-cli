#!/bin/bash

# Guard against double-sourcing
[ -n "${_E2E_COMMON_LOADED:-}" ] && return 0
_E2E_COMMON_LOADED=1

# Shared config (allow override via env)
BOT_SESSION="${BOT_SESSION:-tg-cli-e2e-bot}"
E2E_SESSION="${E2E_SESSION:-tg-cli-e2e}"
export TMUX_TEST="tmux -u -L tg-cli-test -f /dev/null"
TEST_CONFIG_DIR="$HOME/.tg-cli-test"
TEST_CLAUDE_CONFIG_DIR="${TEST_CLAUDE_CONFIG_DIR:-$TEST_CONFIG_DIR/claude-config}"
TEST_SETTINGS="$TEST_CLAUDE_CONFIG_DIR/settings.json"
TEST_PORT=12501
LOG_FILE="$TEST_CONFIG_DIR/bot.log"
TYPING_LOG_FILE="$TEST_CONFIG_DIR/typing.log"
CREDENTIALS="$TEST_CONFIG_DIR/credentials.json"
# Codex is significantly slower than CC; use longer timeout
if [ "${E2E_BACKEND:-}" = "codex" ]; then
  TIMEOUT=180
else
  TIMEOUT=60
fi

# Results tracking via shared file
E2E_RESULTS_FILE="${E2E_RESULTS_FILE:-/tmp/tg-cli-e2e-results-$$.txt}"
export E2E_RESULTS_FILE

pass() { echo "PASS|$1" >> "$E2E_RESULTS_FILE"; echo "  PASS: $1"; }
fail() { echo "FAIL|$1" >> "$E2E_RESULTS_FILE"; echo "  FAIL: $1"; exit 1; }
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
  echo "  ERROR: $_e2e_last_error"
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

wait_for_idle() {
  local timeout=${1:-$TIMEOUT}
  local target=${2:-}
  local url="http://127.0.0.1:$TEST_PORT/session/idle"
  if [ -n "$target" ]; then
    local encoded_target
    encoded_target=$(printf '%s' "$target" | python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read()))")
    url="${url}?target=${encoded_target}"
  fi
  local elapsed=0
  while [ $elapsed -lt $timeout ]; do
    local idle
    idle=$(curl -sf "$url" 2>/dev/null \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('idle',False))" 2>/dev/null) || true
    if [ "$idle" = "True" ]; then
      sleep 5
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  local debug_resp debug_target
  debug_resp=$(curl -sf "$url" 2>/dev/null || echo '{"error":"curl failed"}')
  debug_target="${target:-${E2E_PANE:-}}"
  pane_log "wait_for_idle TIMEOUT after ${timeout}s (idle_resp: $debug_resp)" "$debug_target"
  fail "wait_for_idle timed out after ${timeout}s on target=${debug_target:-<none>} (idle_resp: $debug_resp)"
}

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
    echo "  DEBUG: grep '$pattern' PIPESTATUS=${_ps[*]}"
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
  _LAST_BOT_LAUNCH_CMD="cd $(pwd) && ./tg-cli --config-dir $TEST_CONFIG_DIR bot --port $TEST_PORT --tmux-server tg-cli-test --debug 2>&1"
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
  # Write minimal .claude.json (skip onboarding, no MCP leak)
  cat > "$TEST_CLAUDE_CONFIG_DIR/.claude.json" << 'MINEOF'
{"hasCompletedOnboarding":true,"hasInitOnboardingBeenShown":true,"numStartups":100,"autoUpdates":false}
MINEOF
  # Install hooks to user-level settings (CLAUDE_CONFIG_DIR/settings.json)
  TEST_SETTINGS="$TEST_CLAUDE_CONFIG_DIR/settings.json"
  echo "" | ./tg-cli --config-dir "$TEST_CONFIG_DIR" install --port "$TEST_PORT" --settings "$TEST_SETTINGS" --skip-tmux
  # Add skipDangerousModePermissionPrompt to settings
  python3 -c "import json;f='$TEST_SETTINGS';d=json.load(open(f));d['skipDangerousModePermissionPrompt']=True;p=d.setdefault('permissions',{});a=p.setdefault('allow',[]);r='Read(//tmp/**)';a.append(r) if r not in a else None;json.dump(d,open(f,'w'),indent=2)"
  # Write test app config
  mkdir -p "$TEST_CONFIG_DIR"
  local cc_cmd="BROWSER=none CLAUDE_CONFIG_DIR=$TEST_CLAUDE_CONFIG_DIR claude --model sonnet --dangerously-skip-permissions --setting-sources user"
  echo "{\"toolNotifyList\":[\"Bash\",\"AskUserQuestion\"],\"claudeCommand\":\"$cc_cmd\",\"paginationMaxRunes\":500}" > "$TEST_CONFIG_DIR/config.json"
  echo "Hooks installed (isolated config: $TEST_CLAUDE_CONFIG_DIR)."
}

cleanup_sessions() {
  local exit_code=$?
  echo ""
  echo "Cleaning up..."
  # Preserve config dirs for post-failure analysis (setup_hooks cleans at start of next run)
  $TMUX_TEST kill-session -t "=$E2E_SESSION" 2>/dev/null || true
  sleep 2
  $TMUX_TEST kill-session -t "=$BOT_SESSION" 2>/dev/null || true
  # Do NOT kill-server — debug sessions (tg-cli-usage-*) may be kept alive for investigation
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
}

ensure_infrastructure() {
  if [ "${E2E_ORCHESTRATED:-}" = "1" ]; then
    return
  fi
  ensure_credentials
  build_test_binary
  start_bot
  export LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
  setup_hooks
  trap cleanup_sessions EXIT
}
