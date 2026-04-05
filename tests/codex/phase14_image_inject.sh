#!/bin/bash
set -euo pipefail
source "${SCRIPT_DIR:=$(cd "$(dirname "$0")" && pwd)}/codex_common.sh"

echo ""
echo "--- Image injection test ---"

ensure_infrastructure

# Create a minimal test JPEG file
TEST_IMG="/tmp/tg-cli-test-image-$$.jpg"
python3 -c "
import struct
# Minimal valid JPEG: SOI + APP0 + EOI
data = b'\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00\xff\xd9'
open('$TEST_IMG', 'wb').write(data)
"

LOG_BEFORE=$(wc -l < "$LOG_FILE")

# Test 1: Image with caption
inject_prompt "describe this test image" "$TEST_IMG"
wait_for_idle $TIMEOUT

if tail -n +$((LOG_BEFORE + 1)) "$LOG_FILE" | grep -q "UserPromptSubmit"; then
  pass "Image injection: UserPromptSubmit confirmed (image + caption)"
else
  fail "Image injection: no UserPromptSubmit after image inject"
fi

# Test 2: Image only (no caption)
LOG_BEFORE2=$(wc -l < "$LOG_FILE")
inject_prompt "" "$TEST_IMG"
wait_for_idle $TIMEOUT

if tail -n +$((LOG_BEFORE2 + 1)) "$LOG_FILE" | grep -q "UserPromptSubmit"; then
  pass "Image injection: UserPromptSubmit confirmed (image only)"
else
  fail "Image injection: no UserPromptSubmit after image-only inject"
fi

rm -f "$TEST_IMG"
echo "  Image injection tests complete."
