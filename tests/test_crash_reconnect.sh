#!/bin/bash
# test_crash_reconnect.sh — proves a blocking PermissionRequest hook reconnects after
# the bot crashes and restarts, WITHOUT needing the upgrade flag.
# Design:
#   Phase 1: stub sends "registered" frame, then holds connection open indefinitely.
#   Crash:   kill the stub (simulates bot crash — EOF on the stream).
#   Phase 2: restart stub on same port; sends "registered" then "answer" and closes.
#   Assert:  hook stdout contains the answer output and exit code is 0.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BINARY="/tmp/tgcli-test-crash-reconnect"
STUB_SCRIPT="/tmp/tgcli-crash-reconnect-stub.py"
PHASE_FILE="/tmp/tgcli-crash-reconnect-phase"
STUB_LOG="/tmp/tgcli-crash-reconnect-stub.log"
HOOK_STDOUT="/tmp/tgcli-crash-reconnect-hook-stdout.log"
HOOK_STDERR="/tmp/tgcli-crash-reconnect-hook-stderr.log"
STUB_PID=""
HOOK_PID=""
TEST_CONFIG=""

cleanup() {
  [ -n "$STUB_PID" ] && kill -9 "$STUB_PID" 2>/dev/null || true
  [ -n "$HOOK_PID" ] && kill -9 "$HOOK_PID" 2>/dev/null || true
  [ -n "$TEST_CONFIG" ] && rm -rf "$TEST_CONFIG" || true
  rm -f "$PHASE_FILE" "$STUB_SCRIPT"
}
trap cleanup EXIT

echo "=== test_crash_reconnect ==="

# Step 1: build binary
echo "Building binary..."
cd "$REPO_ROOT"
go build -o "$BINARY" . 2>&1
if [ ! -x "$BINARY" ]; then
  echo "FAIL: binary build failed"
  exit 1
fi
echo "Binary: $BINARY"

# Step 2: pick a free port for the stub server
FREE_PORT=$(python3 -c "import socket; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(('',0)); print(s.getsockname()[1]); s.close()")
echo "Stub server port: $FREE_PORT"

# Step 3: write the Python stub script.
# Controlled by PHASE_FILE:
#   phase "1" (or missing): send registered, hold connection open
#   phase "2":              send registered, then answer, then close
cat > "$STUB_SCRIPT" << 'PYEOF'
import sys, json, time, socket
from http.server import BaseHTTPRequestHandler, HTTPServer

port = int(sys.argv[1])
phase_file = sys.argv[2]

registered = json.dumps({"type":"registered","msg_id":99999,"chat_id":123456,"topic_id":0}).encode() + b'\n'
answer_output = json.dumps({"hookSpecificOutput":{"hookEventName":"PermissionRequest","permissionDecision":"allow"}})
answer = json.dumps({"type":"answer","output":answer_output}).encode() + b'\n'

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path.startswith('/pending/connect'):
            clen = int(self.headers.get('Content-Length', 0))
            self.rfile.read(clen)
            self.send_response(200)
            self.send_header('Content-Type', 'application/x-ndjson')
            self.send_header('Cache-Control', 'no-cache')
            self.end_headers()
            self.wfile.write(registered)
            self.wfile.flush()
            # Check phase file to decide behavior
            phase = "1"
            try:
                with open(phase_file) as f:
                    phase = f.read().strip()
            except Exception:
                pass
            if phase == "2":
                # Send answer then close connection — hook should exit 0
                time.sleep(0.3)
                self.wfile.write(answer)
                self.wfile.flush()
                return
            else:
                # Hold connection open indefinitely (blocking phase)
                while True:
                    time.sleep(1)
        else:
            self.send_response(404)
            self.end_headers()
    def log_message(self, fmt, *args):
        pass  # suppress default access log

server = HTTPServer(('127.0.0.1', port), Handler)
server.allow_reuse_address = True
server.daemon_threads = True
print(f"stub listening on {port}", flush=True)
server.serve_forever()
PYEOF

# Step 4: set up temp config dir so the hook uses the stub via credentials
TEST_CONFIG="$(mktemp -d)"
cat > "$TEST_CONFIG/credentials.json" << CREDSEOF
{"botToken":"test","pairingAllow":{"ids":["test"],"defaultChatId":"123456"},"port":$FREE_PORT}
CREDSEOF

# Control check: no upgrading flag should exist in config dir during the test
UPGRADE_FLAG="$TEST_CONFIG/upgrading"

# Minimal PermissionRequest payload
PAYLOAD='{"hook_event_name":"PermissionRequest","session_id":"test-sess","tool_name":"Bash","tool_input":{"command":"echo test"},"cwd":"/tmp","tool_use_id":""}'

# Phase 1: start stub holding connections open
echo "1" > "$PHASE_FILE"
python3 "$STUB_SCRIPT" "$FREE_PORT" "$PHASE_FILE" > "$STUB_LOG" 2>&1 &
STUB_PID=$!

# Wait for stub to be ready
ELAPSED=0
while [ $ELAPSED -lt 20 ]; do
  if grep -q "stub listening" "$STUB_LOG" 2>/dev/null; then
    break
  fi
  sleep 0.2
  ELAPSED=$((ELAPSED + 1))
done
if [ $ELAPSED -ge 20 ]; then
  echo "FAIL: stub (phase 1) did not start within 4s"
  cat "$STUB_LOG" || true
  exit 1
fi
echo "Phase 1 stub started (PID=$STUB_PID)"

# Launch hook in background; capture stdout and stderr to files
echo "$PAYLOAD" | "$BINARY" --config-dir "$TEST_CONFIG" hook > "$HOOK_STDOUT" 2> "$HOOK_STDERR" &
HOOK_PID=$!
echo "Hook PID: $HOOK_PID"

# Wait for the hook to connect and register (up to 5s)
BOT_LOG="$TEST_CONFIG/bot.log"
ELAPSED=0
while [ $ELAPSED -lt 25 ]; do
  if grep -q "registered: uuid=" "$BOT_LOG" 2>/dev/null; then
    break
  fi
  sleep 0.2
  ELAPSED=$((ELAPSED + 1))
done
if [ $ELAPSED -ge 25 ]; then
  echo "FAIL: hook did not register within 5s"
  echo "--- hook stderr ---"
  cat "$HOOK_STDERR" || true
  echo "--- stub log ---"
  cat "$STUB_LOG" || true
  echo "--- bot log ---"
  cat "$BOT_LOG" || true
  kill -9 "$HOOK_PID" 2>/dev/null || true
  HOOK_PID=""
  exit 1
fi
echo "Hook registered — now simulating crash (killing stub)"

# Verify hook is still alive before crash
if ! kill -0 "$HOOK_PID" 2>/dev/null; then
  echo "FAIL: hook exited before stub crash"
  echo "--- hook stderr ---"
  cat "$HOOK_STDERR" || true
  exit 1
fi

# Crash: kill the stub with SIGKILL to force immediate EOF on the connection
kill -9 "$STUB_PID" 2>/dev/null || true
STUB_PID=""

echo "Stub killed — waiting 3s before restarting"
sleep 3

# Verify hook is still alive during the down window (reconnect pending)
if ! kill -0 "$HOOK_PID" 2>/dev/null; then
  echo "FAIL: hook exited while bot was down (should have waited to reconnect)"
  echo "--- hook stderr ---"
  cat "$HOOK_STDERR" || true
  echo "--- bot log ---"
  cat "$BOT_LOG" || true
  exit 1
fi
echo "Hook still alive during down window — good"

# Phase 2: restart stub with answer behavior
echo "2" > "$PHASE_FILE"
python3 "$STUB_SCRIPT" "$FREE_PORT" "$PHASE_FILE" >> "$STUB_LOG" 2>&1 &
STUB_PID=$!

# Wait for phase 2 stub to be ready
ELAPSED=0
while [ $ELAPSED -lt 20 ]; do
  if grep -c "stub listening" "$STUB_LOG" 2>/dev/null | grep -qv "^0$" && \
     [ "$(grep -c 'stub listening' "$STUB_LOG" 2>/dev/null || echo 0)" -ge 2 ]; then
    break
  fi
  sleep 0.2
  ELAPSED=$((ELAPSED + 1))
done
# Simpler readiness check: just wait a bit since STUB_LOG append is reliable
sleep 1
echo "Phase 2 stub started (PID=$STUB_PID)"

# Wait for hook to exit (up to 15s) with polling
HOOK_GONE=false
for i in $(seq 1 30); do
  sleep 0.5
  if ! kill -0 "$HOOK_PID" 2>/dev/null; then
    HOOK_GONE=true
    break
  fi
done

# Capture exit code safely (if wait won't block)
HOOK_EXIT=0
if [ "$HOOK_GONE" = "true" ]; then
  if wait "$HOOK_PID"; then
    HOOK_EXIT=0
  else
    HOOK_EXIT=$?
  fi
  HOOK_PID=""
fi

echo ""
echo "--- hook stdout ---"
cat "$HOOK_STDOUT" || true
echo "--- hook stderr ---"
cat "$HOOK_STDERR" || true
echo "--- bot log ---"
cat "$BOT_LOG" || true
echo "--- stub log ---"
cat "$STUB_LOG" || true
echo "---"

# Control check: no upgrade flag should have existed
if [ -f "$UPGRADE_FLAG" ]; then
  echo "FAIL: upgrade flag existed at $UPGRADE_FLAG (test precondition violated)"
  exit 1
fi

# Assert: hook must have exited
if [ "$HOOK_GONE" = "false" ]; then
  echo "FAIL: hook did not exit within 15s after bot restarted"
  kill -9 "$HOOK_PID" 2>/dev/null || true
  HOOK_PID=""
  exit 1
fi

# Assert: hook exit code must be 0
if [ "$HOOK_EXIT" -ne 0 ]; then
  echo "FAIL: hook exited with code $HOOK_EXIT (expected 0)"
  exit 1
fi

# Assert: hook stdout must contain the answer (permissionDecision)
if ! grep -q "permissionDecision" "$HOOK_STDOUT"; then
  echo "FAIL: hook stdout does not contain answer (permissionDecision not found)"
  echo "Hook stdout was: $(cat "$HOOK_STDOUT")"
  exit 1
fi

echo "PASS: hook reconnected after crash and received answer without upgrade flag"
exit 0
