#!/bin/bash
set -euo pipefail
source "${SCRIPT_DIR:=$(cd "$(dirname "$0")" && pwd)}/codex_common.sh"

echo ""
echo "--- Image injection test ---"

ensure_infrastructure
pane_log "[image_inject] BEFORE test"

TEST_IMG="/tmp/tg-cli-test-image-$$.jpg"
cp "$SCRIPT_DIR/../test_image.jpg" "$TEST_IMG"

LOG_BEFORE=$(wc -l < "$LOG_FILE")

# Test 1: Image with caption
pane_log "[image_inject] BEFORE image+caption inject"
inject_prompt "describe this test image" "$TEST_IMG"
wait_for_idle $TIMEOUT
pane_log "[image_inject] AFTER image+caption inject"

PANE_AFTER=$($TMUX_TEST capture-pane -t "${E2E_PANE%@*}" -p -S - 2>/dev/null || true)
echo "  DEBUG: PANE_AFTER (${#PANE_AFTER} chars): $PANE_AFTER"
set +eo pipefail
echo "$PANE_AFTER" | grep -q "\[Image\|tg-cli-test-image"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep '[Image|tg-cli-test-image' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Image injection: Codex shows image in pane (image + caption)"
else
  fail "Image injection: Codex does not show image — image was lost"
fi

set +eo pipefail
tail -n +$((LOG_BEFORE + 1)) "$LOG_FILE" | grep -q "UserPromptSubmit"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'UserPromptSubmit' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Image injection: UserPromptSubmit confirmed (image + caption)"
else
  fail "Image injection: no UserPromptSubmit after image inject"
fi

# Test 2: Image only (no caption)
LOG_BEFORE2=$(wc -l < "$LOG_FILE")
pane_log "[image_inject] BEFORE image-only inject"
inject_prompt "" "$TEST_IMG"
wait_for_idle $TIMEOUT
pane_log "[image_inject] AFTER image-only inject"

PANE_AFTER2=$($TMUX_TEST capture-pane -t "${E2E_PANE%@*}" -p -S - 2>/dev/null || true)
set +eo pipefail
echo "$PANE_AFTER2" | grep -q "\[Image\|tg-cli-test-image"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep '[Image|tg-cli-test-image' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Image injection: Codex shows image in pane (image only)"
else
  fail "Image injection: Codex does not show image — image was lost"
fi

set +eo pipefail
tail -n +$((LOG_BEFORE2 + 1)) "$LOG_FILE" | grep -q "UserPromptSubmit"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'UserPromptSubmit' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Image injection: UserPromptSubmit confirmed (image only)"
else
  fail "Image injection: no UserPromptSubmit after image-only inject"
fi

# Test 3: Multi-line text injection (no image)
LOG_BEFORE3=$(wc -l < "$LOG_FILE")
MULTI_TEXT="line1 test
line2 test
line3 test"
pane_log "[multiline] BEFORE multi-line inject"
inject_prompt "$MULTI_TEXT"
wait_for_idle $TIMEOUT
pane_log "[multiline] AFTER multi-line inject"

set +eo pipefail
tail -n +$((LOG_BEFORE3 + 1)) "$LOG_FILE" | grep -q "UserPromptSubmit"
_ps=("${PIPESTATUS[@]}")
set -eo pipefail
echo "  DEBUG: grep 'UserPromptSubmit' PIPESTATUS=${_ps[*]}"
if [ "${_ps[1]}" -eq 0 ]; then
  pass "Multi-line injection: UserPromptSubmit confirmed"
else
  fail "Multi-line injection: no UserPromptSubmit after multi-line inject"
fi

rm -f "$TEST_IMG"
echo "  Image injection tests complete."
