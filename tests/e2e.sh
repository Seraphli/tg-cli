#!/bin/bash
set -euo pipefail

# Config
BOT_SESSION="tg-cli-e2e-bot"
CLAUDE_SESSION="tg-cli-e2e-claude"
LOG_FILE="$HOME/.tg-cli/bot.log"
CREDENTIALS="$HOME/.tg-cli/credentials.json"
TIMEOUT=60

# Prerequisite: check pairing is done
if [ ! -f "$CREDENTIALS" ]; then
  echo "ERROR: $CREDENTIALS not found. Complete pairing first."
  exit 1
fi

DEFAULT_CHAT_ID=$(jq -r '.pairingAllow.defaultChatId // empty' "$CREDENTIALS")
if [ -z "$DEFAULT_CHAT_ID" ]; then
  echo "ERROR: No defaultChatId in credentials. Complete pairing first."
  exit 1
fi
echo "Paired chat ID: $DEFAULT_CHAT_ID"

cleanup() {
  echo "Cleaning up..."
  tmux kill-session -t "$BOT_SESSION" 2>/dev/null || true
  tmux kill-session -t "$CLAUDE_SESSION" 2>/dev/null || true
}
trap cleanup EXIT

echo "=== tg-cli E2E Test ==="

# 1. Record bot log position before test
touch "$LOG_FILE"
LOG_BEFORE=$(wc -l < "$LOG_FILE")

# 2. Start bot in tmux with --debug
tmux new-session -d -s "$BOT_SESSION"
tmux send-keys -t "$BOT_SESSION" "cd $(pwd) && ./tg-cli bot --debug" Enter
echo "Waiting for bot to start..."
sleep 5

# 3. Build project (needed for hook binary)
go build -o tg-cli
echo "Project built."

# 4. Setup hooks
./tg-cli setup
echo "Hooks installed."

# 5. Start Claude in tmux (--allow-dangerously-skip-permissions enables bypass option without forcing it)
tmux new-session -d -s "$CLAUDE_SESSION"
tmux send-keys -t "$CLAUDE_SESSION" "claude --allow-dangerously-skip-permissions" Enter
echo "Waiting for Claude to start..."
sleep 5

# 5b. Handle trust dialog if shown (press Enter to confirm "Yes, I trust this folder")
tmux send-keys -t "$CLAUDE_SESSION" Enter
echo "Confirmed trust dialog."
sleep 8

# 6. Send a simple command to trigger hook
tmux send-keys -t "$CLAUDE_SESSION" -l "say hello"
sleep 1
tmux send-keys -t "$CLAUDE_SESSION" Enter
echo "Command sent, waiting for hook to trigger..."

# 7. Wait for notification with polling
ELAPSED=0
FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  LOG_AFTER=$(wc -l < "$LOG_FILE")
  if [ "$LOG_AFTER" -gt "$LOG_BEFORE" ]; then
    if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep -q "Notification sent"; then
      FOUND=true
      break
    fi
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting... ${ELAPSED}s / ${TIMEOUT}s"
done

# 8. Capture Claude pane output for debugging (always, not just on failure)
echo ""
echo "Claude pane output:"
tmux capture-pane -t "$CLAUDE_SESSION" -p -S -50

# 9. Exit Claude
tmux send-keys -t "$CLAUDE_SESSION" -l "/exit"
sleep 1
tmux send-keys -t "$CLAUDE_SESSION" Enter
sleep 2

# 10. Report result
if [ "$FOUND" = true ]; then
  echo ""
  echo "E2E test PASSED — notification was sent to Telegram"
  echo "Relevant log entries:"
  tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "Notification"
  exit 0
else
  echo ""
  echo "E2E test FAILED — no notification detected within ${TIMEOUT}s"
  echo "Bot log tail:"
  tail -20 "$LOG_FILE"
  exit 1
fi
