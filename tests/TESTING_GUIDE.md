# Test Case Writing Guide

This guide explains how to write new E2E phase test files for tg-cli.

---

## Phase Numbering Rules

- Each phase file gets a **unique integer number**: `phase1_unit.sh`, `phase2_bot_hook.sh`, ..., `phase19_bot_new.sh`
- **NEVER reuse a number** — duplicate numbers cause silent overwrites or undefined ordering
- **Current highest phase: 19** (`phase19_bot_new.sh`)
- Interactive tests (that require CC running in tmux) must come **BEFORE** any `session_end` phase
- Non-interactive tests (unit tests, build checks, config checks) can go anywhere but conventionally go early (e.g., phase1)
- The `e2e.sh` orchestrator discovers phases by globbing `phases/phase*.sh` in numeric order

---

## File Template

```bash
#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- <Phase description> ---"

ensure_infrastructure

LOG_BEFORE_PHASE=$(wc -l < "$LOG_FILE")

# --- Step 1: describe what you do ---
pane_log "[myphase] BEFORE step 1: <action>" "$E2E_PANE"
# ... do the action ...
pane_log "[myphase] AFTER step 1: <action>" "$E2E_PANE"

# --- Verify log output ---
if tail -n +"$((LOG_BEFORE_PHASE + 1))" "$LOG_FILE" | grep "expected pattern" > /dev/null 2>&1; then
  pass "Expected behavior occurred"
else
  fail "Expected behavior did not occur"
fi
```

**Notes:**
- Always `source "${SCRIPT_DIR}/../e2e_common.sh"` — provides all helpers and shared variables
- Always call `ensure_infrastructure` at the top for interactive tests (skipped automatically in orchestrated mode)
- Capture `LOG_BEFORE_PHASE` immediately after `ensure_infrastructure` — use it for all log grep patterns in this phase
- Each step should have a `pane_log` BEFORE and AFTER to record TUI state changes

---

## Available Helper Functions

All helpers come from `tests/e2e_common.sh`.

### Result Reporting

```bash
pass "Description of what passed"
fail "Description of what failed"
```

Results are written to `$E2E_RESULTS_FILE` (shared across phases when orchestrated).

### Injecting Prompts into Claude Code

```bash
inject_prompt "Your prompt text here"
```

Sends text to Claude Code's TUI via the `/inject` HTTP API. Returns non-zero on failure. Always wait for CC to be idle before injecting.

### Waiting for CC to Be Idle

```bash
wait_for_idle [timeout] [target]
```

Polls `/session/idle` until CC reports idle. Adds an extra 5s sleep after idle is confirmed.
- `timeout`: seconds to wait (default: `$TIMEOUT` = 60)
- `target`: tmux pane ID (default: empty, uses the registered CC pane)

### Waiting for Bot HTTP Server

```bash
wait_for_bot_ready [timeout]
```

Polls `/session/idle` until the bot HTTP server responds. Used internally by `start_bot`.

### Capturing Pane Content to Log

```bash
pane_log "label" [target]
```

Calls `/capture` API and appends the pane content to `$LOG_FILE` with surrounding markers.
- `target`: tmux pane ID (default: `$E2E_PANE`)

Use `pane_log` before and after every significant action so failures can be analyzed post-run.

### Infrastructure Setup

```bash
ensure_infrastructure
```

In standalone mode: builds binary, starts bot, installs hooks, starts Claude. In orchestrated mode (when `E2E_ORCHESTRATED=1`): no-op (infrastructure already running).

### Waiting for Pane Content

```bash
wait_for_pane_content "pattern" [timeout] [target]
```

Polls `/capture` until the pane content matches the given grep pattern.

---

## Log Grep Pattern

All log verification must use `tail -n +` with a captured line offset. **Never grep the full log file** — earlier test phases leave stale entries that cause false positives.

```bash
# Capture baseline BEFORE triggering the action
LOG_BEFORE=$(wc -l < "$LOG_FILE")

# ... trigger the action ...

# Grep only lines added AFTER the baseline
if tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE" | grep "pattern" > /dev/null 2>&1; then
  pass "Pattern found in log"
else
  fail "Pattern not found in log"
fi
```

For multi-step phases, capture a new `LOG_BEFORE_*` variable before each step:

```bash
LOG_BEFORE_PHASE=$(wc -l < "$LOG_FILE")

# ... step 1 ...

LOG_BEFORE_STEP2=$(wc -l < "$LOG_FILE")

# ... step 2 ...
tail -n +"$((LOG_BEFORE_STEP2 + 1))" "$LOG_FILE" | grep "step2 pattern"
```

---

## HTTP API Endpoints

The bot exposes these endpoints on `http://127.0.0.1:$TEST_PORT`:

| Endpoint | Purpose |
|---|---|
| `/inject` | Inject text into a tmux pane (CC input) |
| `/capture` | Capture tmux pane content |
| `/escape` | Send Escape key to a tmux pane |
| `/session/idle` | Query whether CC is idle |
| `/session/name` | Get or set the current session name |
| `/route/bind` | Bind a chat route |
| `/route/unbind` | Unbind a chat route |
| `/route/list` | List current routes |
| `/perm/switch` | Switch permission mode |
| `/perm/status` | Get current permission mode |
| `/permission/decide` | Respond to a PreToolUse permission decision |
| `/tool/respond` | Respond to an AskUserQuestion |
| `/mcp/send-file` | Send a file via MCP |
| `/merge/start` | Start a merge session |
| `/merge/add` | Add content to merge buffer |
| `/merge/submit` | Submit merged content |
| `/resume/list` | List resumable CC sessions |
| `/resume/select` | Select a session to resume |
| `/bot_new` | Trigger interactive CC session launch flow |
| `/bot_new/callback` | Submit a step response in the /bot_new flow |

All endpoints return JSON. Check `ok` field for success:

```bash
RESP=$(curl -s "http://127.0.0.1:${TEST_PORT}/session/idle")
OK=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('idle',False))" 2>/dev/null)
```

---

## Key Variables

These are set by `e2e_common.sh` and available in all phase files:

| Variable | Value | Description |
|---|---|---|
| `$TEST_PORT` | `12501` | Bot HTTP server port (test instance) |
| `$LOG_FILE` | `~/.tg-cli-test/bot.log` | Bot log file path |
| `$E2E_PANE` | tmux pane ID | The active CC pane (set by `start_claude`) |
| `$DEFAULT_CHAT_ID` | from credentials | Paired Telegram chat ID |
| `$TIMEOUT` | `60` | Default wait timeout in seconds |
| `$LOG_BEFORE` | line count | Set by `ensure_infrastructure` (use as baseline for full-run log greps) |
| `$BOT_SESSION` | `tg-cli-e2e-bot` | tmux session name for bot |
| `$E2E_SESSION` | `tg-cli-e2e-claude` | tmux session name for CC |
| `$TEST_CONFIG_DIR` | `~/.tg-cli-test` | Config directory for test bot |

---

## How to Add a New Phase

1. **Pick the next available integer** (currently: 20 for the next new phase)
2. Create `tests/phases/phase20_myfeature.sh`
3. Make it executable: `chmod +x tests/phases/phase20_myfeature.sh`
4. Use the file template above
5. If your phase must run **before** `phase17_session_end.sh` (interactive test), insert it before phase 17 and renumber:
   - Rename from the **highest number down** to avoid collisions
   - Example: to insert before phase 17, rename phase19 → phase20, phase18 → phase19, phase17 → phase18, then create your new phase17
6. Run standalone to verify: `bash tests/phases/phase20_myfeature.sh`
7. Run full suite to verify integration: `bash tests/e2e.sh 2>&1 | tee /tmp/e2e-output.txt`

---

## Common Patterns

### Wait for a log message with timeout

```bash
ELAPSED=0
FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_PHASE + 1))" "$LOG_FILE" | grep "expected log pattern" > /dev/null 2>&1; then
    FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting... ${ELAPSED}s / ${TIMEOUT}s"
done
if [ "$FOUND" = true ]; then
  pass "Event occurred"
else
  fail "Event did not occur within ${TIMEOUT}s"
  exit 1
fi
```

### Make an HTTP POST with JSON body

```bash
PAYLOAD=$(jq -n --arg key "value" '{key: $key}')
RESP=$(curl -s -X POST \
  -H "Content-Type: application/json" \
  -d "$PAYLOAD" \
  "http://127.0.0.1:${TEST_PORT}/endpoint")
OK=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('ok',''))" 2>/dev/null || echo "false")
if [ "$OK" != "True" ]; then
  fail "API call failed"
  exit 1
fi
```

### Inject a prompt and wait for CC to finish

```bash
wait_for_idle
pane_log "[myphase] BEFORE inject" "$E2E_PANE"
inject_prompt "Do something and reply with DONE"
pane_log "[myphase] AFTER inject" "$E2E_PANE"
wait_for_idle 120
pane_log "[myphase] AFTER cc_idle" "$E2E_PANE"
```

### Non-interactive phase (unit test, config check)

```bash
#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Unit tests ---"

# No ensure_infrastructure call needed

if go test ./internal/mypkg/ -v -count=1 2>&1 | tail -1 | grep "^ok" > /dev/null 2>&1; then
  pass "Go unit tests for mypkg"
else
  fail "Go unit tests for mypkg"
fi
```
