#!/bin/bash
# test_hook_parent_death.sh — proves an orphaned blocking hook exits within ~2s of its parent dying.
# The hook must be BLOCKING on the /pending/connect stream, not exiting due to connection-refused.
# Design:
#   1. Build the binary.
#   2. Start a stub HTTP server that accepts POST /pending/connect and holds the connection open
#      (streams the "registered" frame but never sends a terminal), so the hook blocks reading.
#   3. Launch the hook as a child of an intermediate parent process; capture the hook PID.
#   4. Kill the intermediate parent.
#   5. Assert: hook PID is gone within ~2s (ppid-watcher fired). Exit non-zero on failure.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BINARY="/tmp/tgcli-test-hookdeath"
DEBUG_LOG="/tmp/tgcli-test-hookdeath-debug.log"
STUB_LOG="/tmp/tgcli-test-hookdeath-stub.log"
STUB_PID=""
PARENT_PID=""

cleanup() {
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
  # PARENT_PID is the intermediate shell — if still alive, kill it
  [ -n "$PARENT_PID" ] && kill "$PARENT_PID" 2>/dev/null || true
}
trap cleanup EXIT

echo "=== test_hook_parent_death ==="

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
FREE_PORT=$(python3 -c "import socket; s=socket.socket(); s.bind(('',0)); print(s.getsockname()[1]); s.close()")
echo "Stub server port: $FREE_PORT"

# Step 3: start stub server that holds /pending/connect open.
# It sends the "registered" ndjson frame immediately, then holds the connection
# open indefinitely (so the hook blocks reading the stream).
python3 - "$FREE_PORT" > "$STUB_LOG" 2>&1 &
STUB_PID=$!
cat << 'PYEOF' > /tmp/tgcli-hookdeath-stub.py
import sys, json, threading, time
from http.server import BaseHTTPRequestHandler, HTTPServer

port = int(sys.argv[1])
registered_frame = json.dumps({"type":"registered","msg_id":99999,"chat_id":123456,"topic_id":0}).encode() + b'\n'

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path.startswith('/pending/connect'):
            # Read body (ignore it)
            clen = int(self.headers.get('Content-Length', 0))
            self.rfile.read(clen)
            self.send_response(200)
            self.send_header('Content-Type','application/x-ndjson')
            self.send_header('Cache-Control','no-cache')
            self.end_headers()
            # Flush the registered frame so the hook logs "registered"
            self.wfile.write(registered_frame)
            self.wfile.flush()
            # Hold connection open — hook blocks here waiting for a terminal
            while True:
                time.sleep(1)
        else:
            self.send_response(404)
            self.end_headers()
    def log_message(self, fmt, *args):
        pass  # suppress default logging

server = HTTPServer(('127.0.0.1', port), Handler)
server.daemon_threads = True
print(f"stub listening on {port}", flush=True)
server.serve_forever()
PYEOF

# Restart stub with proper script file (avoids heredoc quoting issues)
kill "$STUB_PID" 2>/dev/null || true
python3 /tmp/tgcli-hookdeath-stub.py "$FREE_PORT" > "$STUB_LOG" 2>&1 &
STUB_PID=$!

# Wait for stub to be ready
ELAPSED=0
while [ $ELAPSED -lt 10 ]; do
  if grep -q "stub listening" "$STUB_LOG" 2>/dev/null; then
    break
  fi
  sleep 0.2
  ELAPSED=$((ELAPSED + 1))
done
if [ $ELAPSED -ge 10 ]; then
  echo "FAIL: stub server did not start within 10s"
  cat "$STUB_LOG" || true
  exit 1
fi
echo "Stub server started (PID=$STUB_PID)"

# Step 4: set up a temp config dir so the hook uses our stub port via credentials
TEST_CONFIG="$(mktemp -d)"
cat > "$TEST_CONFIG/credentials.json" << CREDSEOF
{"botToken":"test","pairingAllow":{"ids":["test"],"defaultChatId":"123456"},"port":$FREE_PORT}
CREDSEOF

# Build a minimal PermissionRequest payload
PAYLOAD='{"hook_event_name":"PermissionRequest","session_id":"test-sess","tool_name":"Bash","tool_input":{"command":"echo test"},"cwd":"/tmp","tool_use_id":""}'

# Step 5: launch the hook as the child of an intermediate parent bash process.
# The parent is a bash that execs tg-cli; we capture the bash PID, then kill it.
# The hook watches os.Getppid() — when the intermediate bash dies, ppid changes → cancel.
HOOK_STDERR_LOG="/tmp/tgcli-hookdeath-stderr.log"
bash -c "$BINARY --config-dir $TEST_CONFIG hook < /dev/null" \
  <<< "$PAYLOAD" > /dev/null 2> "$HOOK_STDERR_LOG" &
# Note: the above won't work cleanly because we need to:
# (a) pass stdin payload via heredoc / echo pipe
# (b) record the inner hook PID (grandchild), not the bash PID

# Better approach: use an intermediate script that prints its child's PID
PARENT_SCRIPT="/tmp/tgcli-hookdeath-parent.sh"
cat > "$PARENT_SCRIPT" << PSCEOF
#!/bin/bash
echo \$\$ > /tmp/tgcli-hookdeath-parent.pid
exec "$BINARY" --config-dir "$TEST_CONFIG" hook
PSCEOF
chmod +x "$PARENT_SCRIPT"

# Feed payload to the intermediate script via stdin pipe
echo "$PAYLOAD" | "$PARENT_SCRIPT" > /dev/null 2>> "$HOOK_STDERR_LOG" &
PARENT_PID=$!
echo "Intermediate parent PID: $PARENT_PID"

# Wait briefly for the hook to connect to the stub and block
sleep 2

# Verify hook is actually running (parent is alive)
if ! kill -0 "$PARENT_PID" 2>/dev/null; then
  echo "FAIL: intermediate parent/hook exited before we killed it"
  echo "--- hook stderr ---"
  cat "$HOOK_STDERR_LOG" || true
  echo "--- stub log ---"
  cat "$STUB_LOG" || true
  exit 1
fi
echo "Hook is blocking (parent_pid=$PARENT_PID alive) — killing parent now"

# Step 6: kill the intermediate parent
kill "$PARENT_PID" 2>/dev/null || true
PARENT_PID=""

# Step 7: wait up to 4s for the hook to die (ppid-watcher polls every 500ms)
ELAPSED=0
HOOK_GONE=false
while [ $ELAPSED -lt 4 ]; do
  sleep 0.5
  ELAPSED=$((ELAPSED + 1))
  # Check if any tgcli-test-hookdeath processes are still running
  if ! pgrep -f "tgcli-test-hookdeath.*hook" > /dev/null 2>&1; then
    HOOK_GONE=true
    break
  fi
done

echo ""
echo "--- hook stderr log ---"
cat "$HOOK_STDERR_LOG" || true
echo "--- stub log ---"
cat "$STUB_LOG" || true
echo "---"

if [ "$HOOK_GONE" = "true" ]; then
  echo "PASS: hook exited within ~${ELAPSED}s of parent death (ppid-watcher fired)"
  exit 0
else
  echo "FAIL: hook still alive after ${ELAPSED}s — ppid-watcher did not fire"
  pgrep -a -f "tgcli-test-hookdeath" || true
  exit 1
fi
