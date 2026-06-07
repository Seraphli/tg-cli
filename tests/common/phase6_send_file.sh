#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- send-file: sync / async / failure test (phase6) ---"

ensure_infrastructure
pane_log "[send_file] BEFORE"

SMALL=/tmp/tg-cli-e2e-sendfile-small.txt
BIG=/tmp/tg-cli-e2e-sendfile-big.bin
EMPTY=/tmp/tg-cli-e2e-sendfile-empty.bin
ERRF=/tmp/tg-cli-e2e-sendfile-stderr.txt
echo "phase6 send-file e2e content - $(date)" > "$SMALL"
truncate -s 51M "$BIG"
: > "$EMPTY"
SMALL_BASE="$(basename "$SMALL")"
EMPTY_BASE="$(basename "$EMPTY")"

# wait_log <pattern> <max_seconds> <start_line>  — poll bot log for a pattern appearing after start_line
wait_log() {
  local pat="$1" max="$2" start="$3" i=0
  while [ "$i" -lt "$max" ]; do
    if tail -n +"$((start + 1))" "$LOG_FILE" | grep -q "$pat"; then return 0; fi
    sleep 1; i=$((i + 1))
  done
  return 1
}

# ---- TC6-1: sync success (regression) ----
LB=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
set +e
OUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" send-file --file "$SMALL" --caption "phase6 sync" --port "$TEST_PORT" 2>&1); RC=$?
set -e
echo "  DEBUG TC6-1: rc=$RC out=$OUT"
if [ "$RC" -eq 0 ] && echo "$OUT" | grep -q "File sent" && wait_log "\[File\] File sent: $SMALL_BASE" 10 "$LB"; then
  pass "TC6-1 sync success: exit0 + 'File sent' + bot log [File] File sent"
else
  fail "TC6-1 sync success: rc=$RC out=$OUT"
fi

# ---- TC6-2: async path taken + bg upload completes ----
# NOTE: cannot E2E-prove "non-blocking under slow net" (no bandwidth throttle for a Go binary).
# Reliable proof of async path = stdout 'queued' (NOT 'File sent'); elapsed echoed for info only.
LB=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
T0=$(date +%s.%N)
set +e
OUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" send-file --file "$SMALL" --caption "phase6 async" --async --port "$TEST_PORT" 2>&1); RC=$?
set -e
T1=$(date +%s.%N)
echo "  DEBUG TC6-2: rc=$RC elapsed=$(awk "BEGIN{printf \"%.2f\", $T1-$T0}")s out=$OUT"
if [ "$RC" -eq 0 ] && echo "$OUT" | grep -q "queued" && ! echo "$OUT" | grep -q "File sent"; then
  if wait_log "\[File\] File sent: $SMALL_BASE" 15 "$LB"; then
    pass "TC6-2 async: exit0 + 'queued' (not 'File sent') + bg upload logged [File] File sent"
  else
    fail "TC6-2 async: queued returned but bg upload never logged [File] File sent within 15s"
  fi
else
  fail "TC6-2 async: rc=$RC out=$OUT (expected exit0 + 'queued' + no 'File sent')"
fi

# ---- TC6-3: sync fail, file not found (CLI-side preflight) ----
set +e
./tg-cli --config-dir "$TEST_CONFIG_DIR" send-file --file /nonexistent-phase6-xxxxx --port "$TEST_PORT" 2>"$ERRF"; RC=$?
set -e
ERR=$(cat "$ERRF"); echo "  DEBUG TC6-3: rc=$RC stderr=$ERR"
if [ "$RC" -ne 0 ] && echo "$ERR" | grep -q "file not found"; then
  pass "TC6-3 sync fail (CLI-side): non-zero exit + stderr 'file not found'"
else
  fail "TC6-3 sync fail (CLI-side): rc=$RC stderr=$ERR"
fi

# ---- TC6-4: sync fail, >50MB (bot-side validation reject + log) ----
LB=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
set +e
./tg-cli --config-dir "$TEST_CONFIG_DIR" send-file --file "$BIG" --port "$TEST_PORT" 2>"$ERRF"; RC=$?
set -e
ERR=$(cat "$ERRF"); echo "  DEBUG TC6-4: rc=$RC stderr=$ERR"
if [ "$RC" -ne 0 ] && echo "$ERR" | grep -qi "too large" && wait_log "\[File\] rejected: .*too large" 5 "$LB"; then
  pass "TC6-4 sync fail (>50MB): non-zero exit + stderr 'too large' + bot log [File] rejected"
else
  fail "TC6-4 sync fail (>50MB): rc=$RC stderr=$ERR"
fi

# ---- TC6-5: async fail path (0-byte → Telegram rejects → [File] send failed logged) ----
LB=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
set +e
OUT=$(./tg-cli --config-dir "$TEST_CONFIG_DIR" send-file --file "$EMPTY" --caption "phase6 async-fail" --async --port "$TEST_PORT" 2>&1); RC=$?
set -e
echo "  DEBUG TC6-5: rc=$RC out=$OUT"
if [ "$RC" -eq 0 ] && echo "$OUT" | grep -q "queued" && wait_log "\[File\] send failed: $EMPTY_BASE" 20 "$LB"; then
  pass "TC6-5 async fail: exit0 + 'queued' + bg failure logged [File] send failed"
else
  fail "TC6-5 async fail: rc=$RC out=$OUT (expected exit0 + 'queued' + bg [File] send failed)"
fi

# ---- TC6-6: sync fail path (0-byte → non-zero exit + stderr + [File] send failed logged) ----
LB=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
set +e
./tg-cli --config-dir "$TEST_CONFIG_DIR" send-file --file "$EMPTY" --port "$TEST_PORT" 2>"$ERRF"; RC=$?
set -e
ERR=$(cat "$ERRF"); echo "  DEBUG TC6-6: rc=$RC stderr=$ERR"
if [ "$RC" -ne 0 ] && echo "$ERR" | grep -q "telegram send failed" && wait_log "\[File\] send failed: $EMPTY_BASE" 5 "$LB"; then
  pass "TC6-6 sync fail (bot-side): non-zero exit + stderr 'telegram send failed' + bot log [File] send failed"
else
  fail "TC6-6 sync fail (bot-side): rc=$RC stderr=$ERR"
fi

rm -f "$SMALL" "$BIG" "$EMPTY" "$ERRF"
pane_log "[send_file] AFTER"
