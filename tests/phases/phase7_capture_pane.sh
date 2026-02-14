#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Phase 7: CapturePane test ---"

ensure_infrastructure

# CapturePane is the core function behind /bot_capture.
# Test it directly against the running Claude pane.
CAPTURE_OUTPUT=$(tmux capture-pane -t "$CLAUDE_SESSION" -p -S - 2>&1)
if [ -n "$CAPTURE_OUTPUT" ]; then
  pass "CapturePane: tmux capture-pane returns content from Claude session"
else
  fail "CapturePane: tmux capture-pane returned empty"
fi

# Verify the binary's CapturePane function via Go test (already in injector_test.go)
if go test ./internal/injector/ -run TestCapturePane -v -count=1 2>&1 | tail -1 | grep "^ok" > /dev/null 2>&1; then
  pass "CapturePane: Go integration test passes"
else
  fail "CapturePane: Go integration test failed"
fi
