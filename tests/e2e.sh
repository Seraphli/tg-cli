#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/e2e_common.sh"

ensure_credentials

# Parse args
PHASE_NUM=""
SESSION_PREFIX=""
while [[ $# -gt 0 ]]; do
  case $1 in
    --phase) PHASE_NUM="$2"; shift 2;;
    --session) SESSION_PREFIX="$2"; shift 2;;
    *) shift;;
  esac
done

# Override session names if specified
if [ -n "$SESSION_PREFIX" ]; then
  BOT_SESSION="${SESSION_PREFIX}-bot"
  CLAUDE_SESSION="${SESSION_PREFIX}-claude"
  export BOT_SESSION CLAUDE_SESSION
fi

# Init results file
> "$E2E_RESULTS_FILE"
export E2E_ORCHESTRATED=1

echo "=== tg-cli E2E Test ==="
echo "Log file: $LOG_FILE"
echo "Results file: $E2E_RESULTS_FILE"

run_phase() {
  local script="$1"
  echo ""
  bash "$script" || true
}

if [ -n "$PHASE_NUM" ]; then
  # Single phase
  MATCHED=$(ls "$SCRIPT_DIR"/phases/phase${PHASE_NUM}_*.sh 2>/dev/null | head -1)
  if [ -z "$MATCHED" ]; then
    echo "ERROR: Phase $PHASE_NUM not found"
    exit 1
  fi
  if [ "$PHASE_NUM" != "1" ]; then
    echo "Building binary..."
    go build -o tg-cli 2>&1 || { echo "Build failed"; exit 1; }
    start_bot
    export LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
    setup_hooks
    start_claude
    trap cleanup_sessions EXIT
  fi
  run_phase "$MATCHED"
else
  # Run all phases
  run_phase "$SCRIPT_DIR/phases/phase1_unit.sh"

  echo "Building binary..."
  go build -o tg-cli 2>&1 || { echo "Build failed"; exit 1; }
  start_bot
  export LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
  setup_hooks
  start_claude
  trap cleanup_sessions EXIT

  for phase in "$SCRIPT_DIR"/phases/phase[2-9]_*.sh; do
    run_phase "$phase"
  done
fi

# Final report
echo ""
echo "=== E2E Test Report ==="
TOTAL_PASS=$(grep "^PASS|" "$E2E_RESULTS_FILE" 2>/dev/null | wc -l)
TOTAL_FAIL=$(grep "^FAIL|" "$E2E_RESULTS_FILE" 2>/dev/null | wc -l)
echo "  Passed: $TOTAL_PASS"
echo "  Failed: $TOTAL_FAIL"
echo ""
if [ "$TOTAL_FAIL" -gt 0 ]; then
  echo "Failed tests:"
  grep "^FAIL|" "$E2E_RESULTS_FILE" | sed 's/^FAIL|/  - /'
  echo ""
  echo "E2E test FAILED"
  exit 1
else
  echo "E2E test PASSED"
  exit 0
fi
