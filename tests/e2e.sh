#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/e2e_common.sh"

E2E_LOG_FILE="/tmp/tg-cli-e2e-$(date +%Y%m%d-%H%M%S).log"
exec > >(tee "$E2E_LOG_FILE") 2>&1
echo "E2E log file: $E2E_LOG_FILE"

ensure_credentials

# Parse args
PHASE_NUM=""
PHASE_START=""
PHASE_END=""
SESSION_PREFIX=""
BACKEND="all"
while [[ $# -gt 0 ]]; do
  case $1 in
    --phase) PHASE_NUM="$2"; shift 2;;
    --start) PHASE_START="$2"; shift 2;;
    --end) PHASE_END="$2"; shift 2;;
    --session) SESSION_PREFIX="$2"; shift 2;;
    --backend) BACKEND="$2"; shift 2;;
    *) shift;;
  esac
done

# Override session names if specified
if [ -n "$SESSION_PREFIX" ]; then
  BOT_SESSION="${SESSION_PREFIX}-bot"
  E2E_SESSION="${SESSION_PREFIX}-claude"
  export BOT_SESSION E2E_SESSION
fi

# Init results file
> "$E2E_RESULTS_FILE"
export E2E_ORCHESTRATED=1
export TEST_CLAUDE_CONFIG_DIR
export E2E_BACKEND="$BACKEND"
case "$BACKEND" in
  cc) source "$SCRIPT_DIR/cc/cc_common.sh" ;;
  codex) source "$SCRIPT_DIR/codex/codex_common.sh" ;;
  all) source "$SCRIPT_DIR/cc/cc_common.sh"; source "$SCRIPT_DIR/codex/codex_common.sh" ;;
esac

echo "=== tg-cli E2E Test ==="
echo "Log file: $LOG_FILE"
echo "Results file: $E2E_RESULTS_FILE"
echo "Backend: $BACKEND"

run_phase() {
  local script="$1"
  echo ""
  local rc=0
  bash "$script" || rc=$?
  if [ $rc -ne 0 ]; then
    pane_log "[$(basename "$script")] CRASH capture"
    fail "Phase $(basename "$script") crashed with exit code $rc"
  else
    # Post-phase sanity checks
    local stale_sessions
    stale_sessions=$($TMUX_TEST list-sessions -F '#{session_name}' 2>/dev/null | grep -v "^${BOT_SESSION}$" || true)
    if [ -n "$stale_sessions" ]; then
      echo "  WARN: stale tmux sessions after $(basename "$script"): $stale_sessions"
      for s in $stale_sessions; do
        pane_log "[post-phase] stale session $s"
        $TMUX_TEST kill-session -t "=$s" 2>/dev/null || true
      done
      fail "Phase $(basename "$script") left stale tmux sessions: $stale_sessions"
    fi
    local queue_status
    queue_status=$(curl -s "http://127.0.0.1:$TEST_PORT/inject/queue-status" 2>/dev/null || echo '{"queues":{}}')
    local pending_count
    pending_count=$(echo "$queue_status" | python3 -c "import sys,json; d=json.load(sys.stdin); print(sum(d.get('queues',{}).values()))" 2>/dev/null || echo "0")
    if [ "$pending_count" -gt 0 ]; then
      echo "  WARN: inject queue not empty after $(basename "$script"): $queue_status"
      fail "Phase $(basename "$script") left $pending_count items in inject queue"
    fi
  fi
}

# Build phase list based on --backend
build_phase_list() {
  local phases=()
  case "$BACKEND" in
    cc)
      phases+=( $(ls "$SCRIPT_DIR/common"/phase*.sh 2>/dev/null | sort -V) )
      phases+=( $(ls "$SCRIPT_DIR/cc"/phase*.sh 2>/dev/null | sort -V) )
      ;;
    codex)
      phases+=( $(ls "$SCRIPT_DIR/common"/phase*.sh 2>/dev/null | sort -V) )
      phases+=( $(ls "$SCRIPT_DIR/codex"/phase*.sh 2>/dev/null | sort -V) )
      ;;
    all)
      phases+=( $(ls "$SCRIPT_DIR/common"/phase*.sh 2>/dev/null | sort -V) )
      phases+=( $(ls "$SCRIPT_DIR/cc"/phase*.sh 2>/dev/null | sort -V) )
      phases+=( $(ls "$SCRIPT_DIR/codex"/phase*.sh 2>/dev/null | sort -V) )
      ;;
    *)
      echo "ERROR: Unknown backend '$BACKEND'. Use cc, codex, or all."
      exit 1
      ;;
  esac
  echo "${phases[@]}"
}

# Find a specific phase across subdirectories (backend-specific dirs searched first)
find_phase() {
  local num="$1"
  local matched=""
  local dirs
  case "$BACKEND" in
    cc) dirs="cc common" ;;
    codex) dirs="codex common" ;;
    *) dirs="common cc codex" ;;
  esac
  for dir in $dirs; do
    matched=$(ls "$SCRIPT_DIR/$dir/phase${num}_"*.sh 2>/dev/null | head -1 || true)
    [ -n "$matched" ] && echo "$matched" && return
  done
}

if [ -n "$PHASE_NUM" ]; then
  # Single phase
  MATCHED=$(find_phase "$PHASE_NUM")
  if [ -z "$MATCHED" ]; then
    echo "ERROR: Phase $PHASE_NUM not found"
    exit 1
  fi
  build_test_binary
  start_bot
  export LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
  setup_hooks
  trap cleanup_sessions EXIT
  run_phase "$MATCHED"
elif [ -n "$PHASE_START" ]; then
  # Range: --start N [--end M]
  build_test_binary
  start_bot
  export LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
  setup_hooks
  trap cleanup_sessions EXIT
  ALL_PHASES=( $(build_phase_list) )
  for phase in "${ALL_PHASES[@]}"; do
    num=$(basename "$phase" | grep -oP 'phase\K[0-9]+')
    [ -z "$num" ] && continue
    [ "$num" -lt "$PHASE_START" ] && continue
    [ -n "$PHASE_END" ] && [ "$num" -gt "$PHASE_END" ] && continue
    run_phase "$phase"
  done
else
  # Run all phases for selected backend
  build_test_binary
  start_bot
  export LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
  setup_hooks
  trap cleanup_sessions EXIT

  # Run common phases first (API-only, no CC/Codex needed)
  for phase in $(ls "$SCRIPT_DIR/common"/phase*.sh 2>/dev/null | sort -V); do
    [ -f "$phase" ] && run_phase "$phase"
  done

  if [ "$BACKEND" = "all" ]; then
    # Run CC phases
    for phase in $(ls "$SCRIPT_DIR/cc"/phase*.sh 2>/dev/null | sort -V); do
      [ -f "$phase" ] && run_phase "$phase"
    done
    # Switch: CC -> Codex (each codex phase self-starts its own codex instance)
    echo ""
    echo "=== Switching backend: CC -> Codex ==="
    export LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
    for phase in $(ls "$SCRIPT_DIR/codex"/phase*.sh 2>/dev/null | sort -V); do
      [ -f "$phase" ] && run_phase "$phase"
    done
  elif [ "$BACKEND" = "codex" ]; then
    for phase in $(ls "$SCRIPT_DIR/codex"/phase*.sh 2>/dev/null | sort -V); do
      [ -f "$phase" ] && run_phase "$phase"
    done
  else
    for phase in $(ls "$SCRIPT_DIR/cc"/phase*.sh 2>/dev/null | sort -V); do
      [ -f "$phase" ] && run_phase "$phase"
    done
  fi
fi

# Final report
echo ""
echo "=== E2E Test Report ==="
TOTAL_PASS=$(grep "^PASS|" "$E2E_RESULTS_FILE" 2>/dev/null | wc -l || true)
TOTAL_FAIL=$(grep "^FAIL|" "$E2E_RESULTS_FILE" 2>/dev/null | wc -l || true)
TOTAL_OPT_PASS=$(grep "^OPT_PASS|" "$E2E_RESULTS_FILE" 2>/dev/null | wc -l || true)
TOTAL_WARN=$(grep "^WARN|" "$E2E_RESULTS_FILE" 2>/dev/null | wc -l || true)
echo "  Required: $TOTAL_PASS PASS / $TOTAL_FAIL FAIL"
echo "  Optional: $TOTAL_OPT_PASS PASS / $TOTAL_WARN WARN"
echo ""
if [ "$TOTAL_OPT_PASS" -gt 0 ] || [ "$TOTAL_WARN" -gt 0 ]; then
  echo "Optional tests:"
  grep "^OPT_PASS|" "$E2E_RESULTS_FILE" | sed 's/^OPT_PASS|/  ✓ /' || true
  grep "^WARN|" "$E2E_RESULTS_FILE" | sed 's/^WARN|/  ⚠ /' || true
  echo ""
fi
if [ "$TOTAL_FAIL" -gt 0 ]; then
  echo "Failed tests:"
  grep "^FAIL|" "$E2E_RESULTS_FILE" | sed 's/^FAIL|/  - /' || true
  echo ""
  echo "E2E test FAILED"
  exit 1
else
  echo "E2E test PASSED"
  exit 0
fi
