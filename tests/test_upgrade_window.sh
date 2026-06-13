#!/bin/bash
# test_upgrade_window.sh — proves no event loss across an upgrade window for a non-blocking event.
# Design:
#   1. Build the binary.
#   2. Start a stub bot HTTP server that records POSTs to /hook/Stop (the non-blocking target).
#   3. EXPERIMENTAL path (with flag): stub DOWN + write upgrade flag → fire Stop event →
#      stub comes back up → assert stub eventually received the event.
#   4. CONTROL path (no flag): stub DOWN + no flag → fire Stop event →
#      assert stub does NOT receive it (hook fails fast).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BINARY="/tmp/tgcli-test-upgrade"
DEBUG_LOG="/tmp/tgcli-test-upgrade-debug.log"
RECEIVED_FILE="/tmp/tgcli-test-upgrade-received.txt"
STUB_PID=""

cleanup() {
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
  rm -f /tmp/tgcli-upgrade-stub.py
}
trap cleanup EXIT

echo "=== test_upgrade_window ==="

# Step 1: build binary
echo "Building binary..."
cd "$REPO_ROOT"
go build -o "$BINARY" . 2>&1
if [ ! -x "$BINARY" ]; then
  echo "FAIL: binary build failed"
  exit 1
fi
echo "Binary: $BINARY"

# pick a free port
FREE_PORT=$(python3 -c "import socket; s=socket.socket(); s.bind(('',0)); print(s.getsockname()[1]); s.close()")
echo "Stub port: $FREE_PORT"

# Write the stub server script
cat > /tmp/tgcli-upgrade-stub.py << PYEOF
import sys, os, json, time, threading
from http.server import BaseHTTPRequestHandler, HTTPServer

port = int(sys.argv[1])
received_file = sys.argv[2]

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        clen = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(clen)
        self.send_response(200)
        self.send_header('Content-Type','application/json')
        self.end_headers()
        self.wfile.write(b'{}')
        with open(received_file, 'a') as f:
            f.write(f"received: path={self.path} body={body.decode()}\n")
    def log_message(self, fmt, *args):
        pass

server = HTTPServer(('127.0.0.1', port), Handler)
server.daemon_threads = True
print(f"stub listening on {port}", flush=True)
server.serve_forever()
PYEOF

# Set up temp config dir
TEST_CONFIG="$(mktemp -d)"
cat > "$TEST_CONFIG/credentials.json" << CREDSEOF
{"botToken":"test","pairingAllow":{"ids":["test"],"defaultChatId":"123456"},"port":$FREE_PORT}
CREDSEOF

# Derive the upgrade flag path the same way config.UpgradeFlagPath() does
UPGRADE_FLAG="$TEST_CONFIG/upgrading"

# Minimal Stop payload
STOP_PAYLOAD='{"hook_event_name":"Stop","session_id":"test-sess","cwd":"/tmp","transcript_path":"/dev/null"}'

# Helper: start stub server
start_stub() {
  > "$RECEIVED_FILE"
  local stub_stdout="/tmp/tgcli-upgrade-stub-out.log"
  python3 /tmp/tgcli-upgrade-stub.py "$FREE_PORT" "$RECEIVED_FILE" > "$stub_stdout" 2>&1 &
  STUB_PID=$!
  # Wait for "stub listening" in stdout, then verify port is accepting
  local elapsed=0
  while [ $elapsed -lt 20 ]; do
    if grep -q "stub listening" "$stub_stdout" 2>/dev/null; then
      # Give it a brief moment to actually bind
      sleep 0.3
      break
    fi
    sleep 0.2
    elapsed=$((elapsed + 1))
  done
  if [ $elapsed -ge 20 ]; then
    echo "FAIL: stub did not start (no 'stub listening' output)"
    cat "$stub_stdout" || true
    exit 1
  fi
}

stop_stub() {
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
  STUB_PID=""
  sleep 0.5
}

# ============================================================
# Path A (EXPERIMENTAL): upgrade flag present + server restart
# Expected: hook retries and event is received after restart
# ============================================================
echo ""
echo "--- Path A: upgrade flag + server starts late (event must be received) ---"

# Start and immediately stop the stub (simulate restart window)
start_stub
stop_stub

# Write fresh upgrade flag
echo "$(date +%s)" > "$UPGRADE_FLAG"
echo "Upgrade flag written: $UPGRADE_FLAG"

# Fire Stop event in background (will retry due to flag)
echo "$STOP_PAYLOAD" | "$BINARY" --config-dir "$TEST_CONFIG" hook > /dev/null 2>> "$DEBUG_LOG" &
HOOK_PID=$!

# Start stub back up within ~3s (inside 25s cap)
sleep 3
start_stub
echo "Stub restarted (PID=$STUB_PID)"

# Wait for hook to complete
wait "$HOOK_PID" 2>/dev/null || true

# Assert stub received the event
ELAPSED=0
RECEIVED=false
while [ $ELAPSED -lt 10 ]; do
  if grep -q "received: path=/hook/Stop" "$RECEIVED_FILE" 2>/dev/null; then
    RECEIVED=true
    break
  fi
  sleep 0.5
  ELAPSED=$((ELAPSED + 1))
done

if [ "$RECEIVED" = "true" ]; then
  echo "PASS (Path A): stub received Stop event across upgrade window"
else
  echo "FAIL (Path A): stub did not receive Stop event within expected time"
  echo "--- received file ---"
  cat "$RECEIVED_FILE" || true
  echo "--- debug log ---"
  cat "$DEBUG_LOG" || true
  exit 1
fi

# Clean up
stop_stub
rm -f "$UPGRADE_FLAG"
> "$DEBUG_LOG"

# ============================================================
# Path B (CONTROL): no flag → event dropped (fast fail)
# Expected: hook exits fast with error; stub does NOT receive
# ============================================================
echo ""
echo "--- Path B: no upgrade flag (event must be dropped) ---"

# Stub is down (stop_stub already called above)
# Ensure no flag
rm -f "$UPGRADE_FLAG"
> "$RECEIVED_FILE"

# Fire Stop event (should fail fast, no retry)
HOOK_START=$(date +%s)
echo "$STOP_PAYLOAD" | "$BINARY" --config-dir "$TEST_CONFIG" hook > /dev/null 2>> "$DEBUG_LOG" || true
HOOK_END=$(date +%s)
HOOK_DURATION=$((HOOK_END - HOOK_START))

# Should fail fast (well under 10s)
if [ "$HOOK_DURATION" -gt 10 ]; then
  echo "FAIL (Path B): hook took ${HOOK_DURATION}s — expected fast failure without flag"
  exit 1
fi
echo "Hook exited in ${HOOK_DURATION}s (fast fail as expected)"

# Stub still down — nothing should have been received
sleep 1
if grep -q "received: path=/hook/Stop" "$RECEIVED_FILE" 2>/dev/null; then
  echo "FAIL (Path B): stub received an event but should not have (no flag, no server)"
  cat "$RECEIVED_FILE" || true
  exit 1
fi

echo "PASS (Path B): no event received without flag (correct fast-fail behavior)"

echo ""
echo "PASS: test_upgrade_window completed successfully"
exit 0
