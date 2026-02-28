#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- Phase 12: MCP send_file tool test ---"

ensure_infrastructure

# Create test file
TEST_FILE="/tmp/tg-cli-e2e-mcp-test-file.txt"
echo "MCP send_file test content - $(date)" > "$TEST_FILE"

LOG_BEFORE_MCP=$(wc -l < "$LOG_FILE")
pane_log "[Phase 12] BEFORE MCP send_file"

# Inject prompt to trigger send_file MCP tool
inject_prompt "Use the send_file MCP tool to send the file at $TEST_FILE to telegram with caption 'E2E test file'"

# Wait for MCP send confirmation in bot log (polling loop)
ELAPSED=0
MCP_FOUND=false
while [ $ELAPSED -lt $TIMEOUT ]; do
  if tail -n +"$((LOG_BEFORE_MCP + 1))" "$LOG_FILE" | grep "\[MCP\] File sent" > /dev/null 2>&1; then
    MCP_FOUND=true
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo "  Waiting for MCP send confirmation... ${ELAPSED}s / ${TIMEOUT}s"
done

pane_log "[Phase 12] AFTER MCP send_file"

if [ "$MCP_FOUND" = true ]; then
  pass "MCP send_file tool"
else
  fail "MCP send_file tool - no send confirmation in bot log"
fi

# Cleanup
rm -f "$TEST_FILE"
