#!/bin/bash
# Hook Timing Test — logs hook events + transcript state for manual observation
# Usage: bash tests/hook_timing_test.sh
# Then interact with Claude manually in the tmux session (session name: hook-timing-test)
# Press Ctrl+C here when done to see the log

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
TEST_DIR=$(mktemp -d /tmp/hook-timing-test.XXXXXX)
LOG_FILE="$TEST_DIR/hook_events.log"
HOOK_SCRIPT="$TEST_DIR/log_hook.sh"
TEST_SETTINGS="$TEST_DIR/settings.json"
SESSION_NAME="hook-timing-test"

echo "=== Hook Timing Test ==="
echo "  Test dir:  $TEST_DIR"
echo "  Log file:  $LOG_FILE"
echo ""

# Create the hook script that logs everything
cat > "$HOOK_SCRIPT" << 'HOOKEOF'
#!/bin/bash
# Read hook payload from stdin
PAYLOAD=$(cat)
EVENT=$(echo "$PAYLOAD" | jq -r '.hook_event_name // .event // "unknown"')
TRANSCRIPT=$(echo "$PAYLOAD" | jq -r '.transcript_path // ""')
TIMESTAMP=$(date '+%H:%M:%S.%3N')
LOG_FILE="$HOOK_TIMING_LOG"

{
  echo "================================================================"
  echo "[$TIMESTAMP] EVENT: $EVENT"
  echo "----------------------------------------------------------------"
  echo "PAYLOAD (key fields):"
  echo "$PAYLOAD" | jq '{hook_event_name, session_id, tool_name, tool_input: (.tool_input | if . then (. | tostring | if length > 200 then .[0:200] + "..." else . end) else null end), stop_hook_active}' 2>/dev/null || echo "$PAYLOAD" | head -5
  echo ""

  if [ -n "$TRANSCRIPT" ] && [ -f "$TRANSCRIPT" ]; then
    TOTAL_LINES=$(wc -l < "$TRANSCRIPT")
    ASSISTANT_COUNT=$(grep -c '"type":"assistant"' "$TRANSCRIPT" 2>/dev/null || echo 0)
    echo "TRANSCRIPT STATE: $TOTAL_LINES total lines, $ASSISTANT_COUNT assistant entries"
    echo "TRANSCRIPT FILE: $TRANSCRIPT"
    echo ""

    # Show the last 3 entries (type + first 150 chars of content)
    echo "LAST 3 TRANSCRIPT ENTRIES:"
    tail -3 "$TRANSCRIPT" | while IFS= read -r line; do
      TYPE=$(echo "$line" | jq -r '.type // "?"' 2>/dev/null)
      if [ "$TYPE" = "assistant" ]; then
        # Extract text content blocks
        TEXT=$(echo "$line" | jq -r '[.message.content[]? | select(.type=="text") | .text] | join(" ")' 2>/dev/null | head -c 150)
        TOOL_USES=$(echo "$line" | jq '[.message.content[]? | select(.type=="tool_use") | .name] | join(", ")' 2>/dev/null)
        echo "  [$TYPE] text: ${TEXT}..."
        [ -n "$TOOL_USES" ] && [ "$TOOL_USES" != "null" ] && echo "         tool_use: $TOOL_USES"
      elif [ "$TYPE" = "tool_result" ] || [ "$TYPE" = "tool_response" ]; then
        echo "  [$TYPE] (tool result entry)"
      else
        PREVIEW=$(echo "$line" | head -c 120)
        echo "  [$TYPE] $PREVIEW..."
      fi
    done
  else
    echo "TRANSCRIPT: not available or file not found"
  fi
  echo ""
} >> "$LOG_FILE"

exit 0
HOOKEOF
chmod +x "$HOOK_SCRIPT"

# Create settings.json with hooks for all relevant events
cat > "$TEST_SETTINGS" << SETEOF
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "HOOK_TIMING_LOG='$LOG_FILE' $HOOK_SCRIPT",
            "timeout": 5
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "HOOK_TIMING_LOG='$LOG_FILE' $HOOK_SCRIPT",
            "timeout": 5
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "HOOK_TIMING_LOG='$LOG_FILE' $HOOK_SCRIPT",
            "timeout": 5
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "HOOK_TIMING_LOG='$LOG_FILE' $HOOK_SCRIPT",
            "timeout": 5
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "HOOK_TIMING_LOG='$LOG_FILE' $HOOK_SCRIPT",
            "timeout": 5
          }
        ]
      }
    ]
  }
}
SETEOF

echo "Settings written to: $TEST_SETTINGS"
echo ""
echo "Starting Claude in tmux session '$SESSION_NAME'..."
echo "  - Only test hooks are active (--setting-sources local)"
echo "  - All hook events logged to: $LOG_FILE"
echo ""
echo "Instructions:"
echo "  1. Interact with Claude in the tmux pane"
echo "  2. Try: ask it to read a file, then ask a question"
echo "  3. Press Ctrl+C here when done"
echo "  4. Log will be displayed automatically"
echo ""

# Kill existing session if any
tmux kill-session -t "$SESSION_NAME" 2>/dev/null || true

# Start Claude in a tmux session with isolated settings
tmux new-session -d -s "$SESSION_NAME" -x 200 -y 50
tmux send-keys -t "$SESSION_NAME" "cd $PROJECT_DIR && claude --settings '$TEST_SETTINGS' --setting-sources local --allow-dangerously-skip-permissions" Enter

echo "Claude is starting in tmux session '$SESSION_NAME'"
echo "Attach with: tmux attach -t $SESSION_NAME"
echo ""
echo "Tailing log (Ctrl+C to stop and show full log)..."
echo ""

# Tail the log file
touch "$LOG_FILE"
tail -f "$LOG_FILE" &
TAIL_PID=$!

# Wait for user to press Ctrl+C
trap "kill $TAIL_PID 2>/dev/null; echo ''; echo '=== Full Log ==='; cat '$LOG_FILE'; echo ''; echo 'Log file: $LOG_FILE'; echo 'Test dir: $TEST_DIR'; tmux kill-session -t $SESSION_NAME 2>/dev/null || true" EXIT

wait $TAIL_PID 2>/dev/null || true
