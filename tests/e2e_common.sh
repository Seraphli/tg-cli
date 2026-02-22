#!/bin/bash

# Shared config (allow override via env)
BOT_SESSION="${BOT_SESSION:-tg-cli-e2e-bot}"
CLAUDE_SESSION="${CLAUDE_SESSION:-tg-cli-e2e-claude}"
TEST_CONFIG_DIR="$HOME/.tg-cli-test"
TEST_SETTINGS="$TEST_CONFIG_DIR/claude-settings.json"
TEST_PORT=12501
LOG_FILE="$TEST_CONFIG_DIR/bot.log"
CREDENTIALS="$TEST_CONFIG_DIR/credentials.json"
TIMEOUT=60

# Results tracking via shared file
E2E_RESULTS_FILE="${E2E_RESULTS_FILE:-/tmp/tg-cli-e2e-results-$$.txt}"
export E2E_RESULTS_FILE

pass() { echo "PASS|$1" >> "$E2E_RESULTS_FILE"; echo "  PASS: $1"; }
fail() { echo "FAIL|$1" >> "$E2E_RESULTS_FILE"; echo "  FAIL: $1"; }

# Log pane capture to bot log file via /capture API
# Usage: pane_log "label"
pane_log() {
  local label="$1"
  local api_url="http://127.0.0.1:$TEST_PORT/capture?target=$(printf '%s' "$CLAUDE_PANE" | jq -sRr @uri)"
  local capture
  capture=$(curl -s "$api_url" | jq -r '.content // "(empty)"' 2>/dev/null || echo "(capture failed)")
  {
    echo "=== PANE: $label ==="
    echo "$capture"
    echo "=== END PANE ==="
  } >> "$LOG_FILE"
}

# Inject prompt into Claude pane via bot API
inject_prompt() {
  local text="$1"
  local api_url="http://127.0.0.1:$TEST_PORT/inject"
  local payload
  payload=$(jq -n --arg t "$CLAUDE_PANE" --arg txt "$text" '{target: $t, text: $txt}')
  echo "  API call: POST $api_url target=$CLAUDE_PANE text=${text:0:80}..."
  local resp
  resp=$(curl -s -w "\n%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d "$payload" \
    "$api_url")
  local code
  code=$(echo "$resp" | tail -1)
  if [ "$code" != "200" ]; then
    echo "  WARNING: Inject API returned $code"
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

wait_for_cc_idle() {
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
    sleep 2
    elapsed=$((elapsed + 2))
  done
  echo "WARN: wait_for_cc_idle timed out after ${timeout}s"
  return 1
}

wait_for_pane_content() {
  local pattern="$1"
  local timeout=${2:-$TIMEOUT}
  local target=${3:-$CLAUDE_PANE}
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
  tmux new-session -d -s "$BOT_SESSION" 2>/dev/null || true
  tmux send-keys -t "$BOT_SESSION" \
    "cd $(pwd) && ./tg-cli --config-dir $TEST_CONFIG_DIR bot --port $TEST_PORT --debug" Enter
  echo "Waiting for bot to start..."
  wait_for_bot_ready
}

start_claude() {
  tmux new-session -d -s "$CLAUDE_SESSION" 2>/dev/null || true
  CLAUDE_PANE=$(tmux list-panes -t "$CLAUDE_SESSION" -F '#{pane_id}')
  export CLAUDE_PANE
  tmux send-keys -t "$CLAUDE_SESSION" \
    "claude --model haiku --allow-dangerously-skip-permissions --setting-sources local --settings $TEST_SETTINGS" Enter
  echo "Waiting for Claude to start..."
  # Check if trust dialog is present before sending Enter
  sleep 5
  PANE_CONTENT=$(tmux capture-pane -t "$CLAUDE_PANE" -p 2>/dev/null || true)
  if echo "$PANE_CONTENT" | grep -qi "trust"; then
    tmux send-keys -t "$CLAUDE_SESSION" Enter
    echo "Trust dialog detected, confirmed."
  else
    echo "No trust dialog detected, skipping."
  fi
  echo "Waiting for Claude to reach idle state..."
  wait_for_cc_idle
}

setup_hooks() {
  ./tg-cli --config-dir "$TEST_CONFIG_DIR" setup --port "$TEST_PORT" --settings "$TEST_SETTINGS"
  echo "Hooks installed."
}

cleanup_sessions() {
  echo ""
  echo "Cleaning up..."
  rm -f "$TEST_SETTINGS"
  tmux kill-session -t "$BOT_SESSION" 2>/dev/null || true
  tmux kill-session -t "$CLAUDE_SESSION" 2>/dev/null || true
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
