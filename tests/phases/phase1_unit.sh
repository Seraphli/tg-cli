#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Phase 1: Go tests ---"

# Phase 1 does NOT call ensure_infrastructure - it only runs tests and build
# In orchestrated mode, skip the build (already done). In standalone mode, always build.

echo "Running injector tests..."
if go test ./internal/injector/ -v -count=1 2>&1 | tee /tmp/tg-cli-injector-test.log | tail -1 | grep "^ok" > /dev/null 2>&1; then
  pass "Go injector tests (unit + tmux injection)"
else
  fail "Go injector tests"
  echo "  See /tmp/tg-cli-injector-test.log for details"
fi

# Only build in standalone mode
if [ "${E2E_ORCHESTRATED:-}" != "1" ]; then
  echo "Running build check..."
  if go build -o tg-cli 2>&1; then
    pass "go build ./..."
  else
    fail "go build ./..."
    exit 1
  fi
fi
