#!/bin/bash
# Deterministic test for the wait_for_idle busy-extend (fix round 3). NO network / grok / real bot.
# Stands up a tiny python http.server that serves /session/idle from a test-controlled state file, and
# drives the real wait_for_idle (+ check_session_idle) from e2e_common.sh against it, asserting:
#   1 idle-immediately        -> rc0, NO "BUSY" line
#   2 busy-then-idle          -> rc0, output has "round 1/3" (extended, then recovered)
#   3 always-busy (exhaust)   -> rc1, rounds 1/3,2/3,3/3 logged + total-budget fail line
#   4 unknown / API down(500) -> rc1, NO "BUSY" line (fails after ONE window, no extend)
#   5 boundary idle diagnosis -> rc0, NO "BUSY" line (busy for inner polls, idle on the post-window
#                                diagnostic; request-count controlled) — the CA-vs-tg-cli adaptation branch
#   6 stall / curl --max-time -> rc1, completes in bounded time (server accepts but never responds)
set -uo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

WORK="$(mktemp -d)"
STATE="$WORK/state"; export STATE
SRV_PID=""

# Pick a free port BEFORE sourcing e2e_common.sh so it (and wait_for_idle) bind to it.
export TEST_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
export E2E_RUN_ID="waitidletest-$$"

# Pull in wait_for_idle + check_session_idle. e2e_common.sh installs set -E + ERR/EXIT traps and its own
# pass/fail (which exit) — clear those and use our own result helpers so assertions don't abort the run.
source "$DIR/e2e_common.sh"
set +E
trap - ERR

cleanup() { [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null || true; rm -rf "$WORK" "${TEST_CONFIG_DIR:-}" 2>/dev/null || true; }
trap cleanup EXIT

cat > "$WORK/server.py" <<'PY'
import http.server, json, os, sys, time
STATE = os.environ["STATE"]
class H(http.server.BaseHTTPRequestHandler):
    count = 0  # counts /session/idle requests only (not /ping) — used by idle_after:N
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path.startswith("/ping"):  # readiness probe, mode-independent, not counted
            self.send_response(200); self.end_headers(); self.wfile.write(b"ok"); return
        if not self.path.startswith("/session/idle"):
            self.send_response(404); self.end_headers(); return
        H.count += 1
        try:
            mode = open(STATE).read().strip()
        except OSError:
            mode = "busy"
        if mode == "down":
            self.send_response(500); self.end_headers(); return
        if mode == "stall":
            time.sleep(30)  # curl --max-time must abort long before this returns
            idle = True
        elif mode.startswith("idle_after:"):
            idle = H.count > int(mode.split(":", 1)[1])
        elif mode == "idle":
            idle = True
        else:  # busy
            idle = False
        body = json.dumps({"idle": idle, "sessions": {"e2e": {"target": "t", "idle": idle}}}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers()
        self.wfile.write(body)
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY

# start_server — fresh process (fresh request counter). Probes /ping (mode-independent) until ready.
start_server() {
  [ -n "$SRV_PID" ] && { kill "$SRV_PID" 2>/dev/null || true; wait "$SRV_PID" 2>/dev/null || true; }
  SRV_PID=""
  python3 "$WORK/server.py" "$TEST_PORT" & SRV_PID=$!
  local i
  for i in $(seq 1 50); do
    curl -sf --max-time 2 "http://127.0.0.1:$TEST_PORT/ping" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  echo "FATAL: fake idle server did not start on port $TEST_PORT"; return 1
}

pass=0; fail=0
t_ok()  { echo "  PASS: $1"; pass=$((pass + 1)); }
t_bad() { echo "  FAIL: $1"; fail=$((fail + 1)); }
has()   { case "$2" in *"$3"*) t_ok "$1";; *) t_bad "$1 (missing [$3])";; esac; }
hasnt() { case "$2" in *"$3"*) t_bad "$1 (found [$3])";; *) t_ok "$1";; esac; }
eqrc()  { [ "$2" = "$3" ] && t_ok "$1" || t_bad "$1 (rc=$2 want $3)"; }

# run_wait <timeout> — foreground; sets RC + OUT.
run_wait() {
  ( wait_for_idle "$1" ) > "$WORK/out" 2>&1
  RC=$?
  OUT="$(cat "$WORK/out")"
}

echo "== Case 1: idle immediately -> rc0, no BUSY =="
echo idle > "$STATE"; start_server
run_wait 2
eqrc  "idle-immediate rc" "$RC" 0
hasnt "idle-immediate: no BUSY extend line" "$OUT" "BUSY: LLM still processing"

echo "== Case 2: busy -> idle (extends, then recovers) -> rc0, round 1/3 =="
echo busy > "$STATE"; start_server
{ ( wait_for_idle 3 ) > "$WORK/out" 2>&1; echo $? > "$WORK/rc"; } &
BGPID=$!
sleep 4          # let the first window time out and one extend begin, then flip to idle
echo idle > "$STATE"
wait "$BGPID"
RC="$(cat "$WORK/rc")"; OUT="$(cat "$WORK/out")"
eqrc "busy-then-idle rc" "$RC" 0
has  "busy-then-idle: extended at least once" "$OUT" "extending wait (round 1/3"

echo "== Case 3: always busy -> exhaust 3 rounds -> rc1 =="
echo busy > "$STATE"; start_server
run_wait 2
eqrc "always-busy rc" "$RC" 1
has  "always-busy: round 1/3" "$OUT" "round 1/3"
has  "always-busy: round 2/3" "$OUT" "round 2/3"
has  "always-busy: round 3/3" "$OUT" "round 3/3"
has  "always-busy: total-budget fail line" "$OUT" "3/3 busy-extends"

echo "== Case 4: unknown / API down (500) -> rc1, NO extend =="
echo down > "$STATE"; start_server
run_wait 2
eqrc  "unknown rc" "$RC" 1
hasnt "unknown: no BUSY extend line (fails after one window)" "$OUT" "BUSY: LLM still processing"

echo "== Case 5: boundary idle diagnosis (busy inner polls, idle on diagnostic) -> rc0, no BUSY =="
start_server                       # fresh process => request counter starts at 0
echo "idle_after:2" > "$STATE"     # 2 inner polls busy, the 3rd request (diagnostic) idle
run_wait 2
eqrc  "boundary-diagnosis rc" "$RC" 0
hasnt "boundary-diagnosis: no BUSY extend line" "$OUT" "BUSY: LLM still processing"

echo "== Case 6: stall (accept but no response) -> rc1, bounded by curl --max-time =="
echo busy > "$STATE"; start_server   # readiness via /ping first
echo stall > "$STATE"
_s=$SECONDS
run_wait 1
_dur=$((SECONDS - _s))
eqrc "stall rc" "$RC" 1
if [ "$_dur" -lt 30 ]; then t_ok "stall bounded (${_dur}s < 30s — curl --max-time capped it)"; else t_bad "stall NOT bounded (${_dur}s >= 30s)"; fi

echo ""
echo "RESULT: $pass PASS / $fail FAIL"
[ "$fail" -eq 0 ]
