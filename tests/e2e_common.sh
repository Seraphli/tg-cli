#!/bin/bash

# Shared config (allow override via env)
BOT_SESSION="${BOT_SESSION:-tg-cli-e2e-bot}"
CLAUDE_SESSION="${CLAUDE_SESSION:-tg-cli-e2e-claude}"
export TMUX_TEST="tmux -L tg-cli-test"
TEST_CONFIG_DIR="$HOME/.tg-cli-test"
TEST_CLAUDE_CONFIG_DIR="${TEST_CLAUDE_CONFIG_DIR:-$(mktemp -d /tmp/tg-cli-e2e-claude-XXXXXX)}"
TEST_SETTINGS="$TEST_CLAUDE_CONFIG_DIR/settings.json"
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
  local target="${2:-$CLAUDE_PANE}"
  local api_url="http://127.0.0.1:$TEST_PORT/capture?target=$(printf '%s' "$target" | jq -sRr @uri)"
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
  $TMUX_TEST new-session -d -s "$BOT_SESSION" 2>/dev/null || true
  $TMUX_TEST send-keys -t "$BOT_SESSION" \
    "cd $(pwd) && ./tg-cli --config-dir $TEST_CONFIG_DIR bot --port $TEST_PORT --tmux-server tg-cli-test --debug" Enter
  echo "Waiting for bot to start..."
  wait_for_bot_ready
}

start_claude() {
  $TMUX_TEST kill-session -t "$CLAUDE_SESSION" 2>/dev/null || true
  $TMUX_TEST new-session -d -s "$CLAUDE_SESSION"
  CLAUDE_PANE=$($TMUX_TEST list-panes -t "$CLAUDE_SESSION" -F '#{pane_id}')
  export CLAUDE_PANE
  $TMUX_TEST send-keys -t "$CLAUDE_SESSION" \
    "BROWSER=none CLAUDE_CONFIG_DIR=$TEST_CLAUDE_CONFIG_DIR claude --model sonnet --allow-dangerously-skip-permissions" Enter
  echo "Waiting for Claude to start..."
  # Check if trust dialog is present before sending Enter
  sleep 5
  pane_log "[start_claude] after 5s sleep, before trust check"
  PANE_CONTENT=$($TMUX_TEST capture-pane -t "$CLAUDE_PANE" -p -S - 2>/dev/null || true)
  if echo "$PANE_CONTENT" | grep -qi "Bypass Permissions"; then
    # Bypass Permissions dialog: cursor defaults to "No, exit", need Down then Enter
    $TMUX_TEST send-keys -t "$CLAUDE_SESSION" Down
    sleep 1
    $TMUX_TEST send-keys -t "$CLAUDE_SESSION" C-m
    echo "Bypass Permissions dialog detected, accepted."
  elif echo "$PANE_CONTENT" | grep -qi "trust"; then
    $TMUX_TEST send-keys -t "$CLAUDE_SESSION" C-m
    echo "Trust dialog detected, confirmed."
  else
    echo "No dialog detected, skipping."
  fi
  pane_log "[start_claude] after trust dialog handling"
  echo "Waiting for Claude to reach idle state..."
  pane_log "[start_claude] before wait_for_cc_idle"
  wait_for_cc_idle
  pane_log "[start_claude] after wait_for_cc_idle"
}

setup_hooks() {
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
  local cc_cmd="BROWSER=none CLAUDE_CONFIG_DIR=$TEST_CLAUDE_CONFIG_DIR claude --model haiku --allow-dangerously-skip-permissions"
  echo "{\"toolNotifyList\":[\"Bash\"],\"claudeCommand\":\"$cc_cmd\",\"paginationMaxRunes\":500}" > "$TEST_CONFIG_DIR/config.json"
  echo "Hooks installed (isolated config: $TEST_CLAUDE_CONFIG_DIR)."
}

cleanup_sessions() {
  local exit_code=$?
  echo ""
  echo "Cleaning up..."
  if [ -n "$TEST_CLAUDE_CONFIG_DIR" ] && [ -d "$TEST_CLAUDE_CONFIG_DIR" ]; then
    rm -rf "$TEST_CLAUDE_CONFIG_DIR"
  fi
  $TMUX_TEST kill-session -t "$CLAUDE_SESSION" 2>/dev/null || true
  sleep 2
  $TMUX_TEST kill-session -t "$BOT_SESSION" 2>/dev/null || true
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
