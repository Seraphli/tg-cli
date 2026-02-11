#!/bin/bash
set -euo pipefail

PASS_COUNT=0
FAIL_COUNT=0

pass() { echo "  PASS: $1"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "  FAIL: $1"; FAIL_COUNT=$((FAIL_COUNT + 1)); }

TEST_CONFIG_DIR=$(mktemp -d)
MOCK_BIN_DIR=$(mktemp -d)

cleanup() {
  rm -rf "$TEST_CONFIG_DIR" "$MOCK_BIN_DIR"
}
trap cleanup EXIT

echo "=== Voice Setup Test ==="
echo "Test config dir: $TEST_CONFIG_DIR"
echo "Mock bin dir: $MOCK_BIN_DIR"

# Create mock whisper-cli binary
cat > "$MOCK_BIN_DIR/whisper-cli" << 'MOCK'
#!/bin/bash
echo "mock whisper"
MOCK
chmod +x "$MOCK_BIN_DIR/whisper-cli"

# Pre-create model file to skip download
mkdir -p "$TEST_CONFIG_DIR/models"
echo "mock model" > "$TEST_CONFIG_DIR/models/ggml-tiny.bin"

# Build first
echo ""
echo "--- Building ---"
go build -o tg-cli 2>&1
pass "go build"

# Run voice setup with piped input and mock whisper in PATH
# Inputs: model selection "1" (tiny), language "en"
echo ""
echo "--- Running voice setup ---"
export PATH="$MOCK_BIN_DIR:$PATH"
OUTPUT=$(echo -e "1\nen" | ./tg-cli --config-dir "$TEST_CONFIG_DIR" voice 2>&1) || true
echo "$OUTPUT"

# Verify ffmpeg detected
if echo "$OUTPUT" | grep -q "ffmpeg found"; then
  pass "ffmpeg auto-detected"
else
  fail "ffmpeg not detected"
fi

# Verify whisper-cli detected
if echo "$OUTPUT" | grep -q "whisper.cpp found"; then
  pass "whisper.cpp auto-detected"
else
  fail "whisper.cpp not detected"
fi

# Verify setup complete message
if echo "$OUTPUT" | grep -q "Voice transcription setup complete"; then
  pass "setup completed"
else
  fail "setup did not complete"
fi

# Verify config.json exists
CONFIG="$TEST_CONFIG_DIR/config.json"
if [ -f "$CONFIG" ]; then
  pass "config.json created"
else
  fail "config.json not created"
  echo "=== Voice Setup Test FAILED ==="
  exit 1
fi

# Verify whisperPath
WHISPER_PATH=$(jq -r '.whisperPath' "$CONFIG")
if [[ "$WHISPER_PATH" == *"whisper-cli"* ]]; then
  pass "whisperPath auto-detected correctly"
else
  fail "whisperPath incorrect: $WHISPER_PATH"
fi

# Verify modelPath
MODEL_PATH=$(jq -r '.modelPath' "$CONFIG")
if [[ "$MODEL_PATH" == *"ggml-tiny.bin"* ]]; then
  pass "modelPath correct"
else
  fail "modelPath incorrect: $MODEL_PATH"
fi

# Verify ffmpegPath
FFMPEG_PATH=$(jq -r '.ffmpegPath' "$CONFIG")
if [ -n "$FFMPEG_PATH" ] && [ "$FFMPEG_PATH" != "null" ]; then
  pass "ffmpegPath set: $FFMPEG_PATH"
else
  fail "ffmpegPath not set"
fi

# Verify language
LANGUAGE=$(jq -r '.language' "$CONFIG")
if [ "$LANGUAGE" = "en" ]; then
  pass "language set to en"
else
  fail "language incorrect: $LANGUAGE"
fi

# Test 2: Keep current model (empty input)
echo ""
echo "--- Test 2: Keep current model ---"
OUTPUT2=$(echo -e "\nen" | ./tg-cli --config-dir "$TEST_CONFIG_DIR" voice 2>&1) || true
echo "$OUTPUT2"

# Verify "Keeping current model" message
if echo "$OUTPUT2" | grep -q "Keeping current model"; then
  pass "keep current model message shown"
else
  fail "keep current model message not shown"
fi

# Verify modelPath still has ggml-tiny.bin
MODEL_PATH2=$(jq -r '.modelPath' "$CONFIG")
if [[ "$MODEL_PATH2" == *"ggml-tiny.bin"* ]]; then
  pass "modelPath preserved after empty selection"
else
  fail "modelPath changed unexpectedly: $MODEL_PATH2"
fi

# Report
echo ""
echo "=== Voice Setup Test Report ==="
echo "  Passed: $PASS_COUNT"
echo "  Failed: $FAIL_COUNT"

if [ "$FAIL_COUNT" -gt 0 ]; then
  echo "Voice setup test FAILED"
  exit 1
else
  echo "Voice setup test PASSED"
  exit 0
fi
