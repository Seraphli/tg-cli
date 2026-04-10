#!/bin/bash

# Guard against double-sourcing
[ -n "${_E2E_COMMON_LOADED:-}" ] && return 0
_E2E_COMMON_LOADED=1

# Shared config (allow override via env)
BOT_SESSION="${BOT_SESSION:-tg-cli-e2e-bot}"
E2E_SESSION="${E2E_SESSION:-tg-cli-e2e}"
export TMUX_TEST="tmux -L tg-cli-test -f /dev/null"
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

# Log pane capture to bot log file via /capture API
# Usage: pane_log "label"
pane_log() {
  local label="$1"
  local target="${2:-${E2E_PANE:-}}"
  if [ -z "$target" ]; then
    echo "=== PANE: $label === (no pane)" >> "$LOG_FILE"
    return
  fi
  local api_url="http://127.0.0.1:$TEST_PORT/capture?target=$(printf '%s' "$target" | jq -sRr @uri)"
  local capture
  capture=$(curl -s "$api_url" | jq -r '.content // "(empty)"' 2>/dev/null || echo "(capture failed)")
  {
    echo "=== PANE: $label ==="
    echo "$capture"
    echo "=== END PANE ==="
  } >> "$LOG_FILE"
}

inject_prompt() {
  local text="$1"
  local image="${2:-}"
  local api_url="http://127.0.0.1:$TEST_PORT/inject/message"
  local payload
  if [ -n "$image" ]; then
    payload=$(jq -n --arg t "$E2E_PANE" --arg txt "$text" --arg img "$image" \
      '{target: $t, text: $txt, imagePath: $img}')
  else
    payload=$(jq -n --arg t "$E2E_PANE" --arg txt "$text" \
      '{target: $t, text: $txt}')
  fi
  echo "  API call: POST $api_url target=$E2E_PANE text=${text:0:80}..."
  local resp
  resp=$(curl -s -w "\n%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d "$payload" \
    "$api_url")
  local code
  code=$(echo "$resp" | tail -1)
  if [ "$code" != "200" ]; then
    echo "  WARNING: inject/message API returned $code"
    return 1
  fi
  return 0
}

wait_for_bot_ready() {
  local timeout=${1:-$TIMEOUT}
  local elapsed=0
  while [ $elapsed -lt $timeout ]; do
    if curl -sf -o /dev/null "http://127.0.0.1:$TEST_PORT/session/idle" 2>/dev/null; then
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  echo "WARN: wait_for_bot_ready timed out after ${timeout}s"
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
  echo "WARN: wait_for_idle timed out after ${timeout}s"
  return 1
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
    if echo "$content" | grep -q "$pattern" 2>/dev/null; then
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
  # Count "Typing sent" entries between T1 and T2
  local typing_timestamps
  typing_timestamps=$(echo "$new_entries" | grep "Typing sent" | grep -oP '^\[\K[^]]+' || true)
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
  > "$LOG_FILE"
  > "$TYPING_LOG_FILE"
  # Clean stale state from previous runs
  rm -f /tmp/.tg-cli-test/pending/*.json 2>/dev/null || true
  rm -f "$TEST_CONFIG_DIR/inject-queue.json" "$TEST_CONFIG_DIR/merge-buffers.json" "$TEST_CONFIG_DIR/sessions.json" 2>/dev/null || true
  $TMUX_TEST new-session -d -s "$BOT_SESSION" 2>/dev/null || true
  $TMUX_TEST send-keys -t "$BOT_SESSION" \
    "cd $(pwd) && ./tg-cli --config-dir $TEST_CONFIG_DIR bot --port $TEST_PORT --tmux-server tg-cli-test --debug 2>&1 | tee -a $LOG_FILE" Enter
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
  python3 -c "import json;f='$TEST_SETTINGS';d=json.load(open(f));d['skipDangerousModePermissionPrompt']=True;json.dump(d,open(f,'w'),indent=2)"
  # Register MCP in isolated config
  CLAUDE_CONFIG_DIR="$TEST_CLAUDE_CONFIG_DIR" claude mcp add --transport stdio tg-cli -- "$(pwd)/tg-cli" --config-dir "$TEST_CONFIG_DIR" mcp --port "$TEST_PORT" 2>/dev/null || true
  # Write test app config
  mkdir -p "$TEST_CONFIG_DIR"
  local cc_cmd="BROWSER=none CLAUDE_CONFIG_DIR=$TEST_CLAUDE_CONFIG_DIR claude --model sonnet --allow-dangerously-skip-permissions"
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
  $TMUX_TEST kill-server 2>/dev/null || true
  return $exit_code
}

ensure_infrastructure() {
  if [ "${E2E_ORCHESTRATED:-}" = "1" ]; then
    return
  fi
  ensure_credentials
  echo "Building binary..."
  go build -o tg-cli 2>&1 || { echo "Build failed"; exit 1; }
  start_bot
  export LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
  setup_hooks
  start_claude
  trap cleanup_sessions EXIT
}
