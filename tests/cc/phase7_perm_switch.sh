#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Permission mode switching test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-cc-7" "--allow-dangerously-skip-permissions"

# E2E_PANE is the tmux pane ID (e.g. %0)
TARGET="$E2E_PANE"
ENCODED_TARGET=$(printf '%s' "$TARGET" | jq -sRr @uri)

# Test each mode: switch to it, then verify via /perm/status
MODES_TO_TEST="plan acceptEdits auto bypass default"

for MODE in $MODES_TO_TEST; do
    LOG_BEFORE=$(wc -l < "$LOG_FILE")
    pane_log "[perm_switch] BEFORE perm/switch to $MODE"

    SWITCH_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/perm/switch?target=$ENCODED_TARGET&mode=$MODE")
    echo "  DEBUG: SWITCH_RESP (${#SWITCH_RESP} chars): $SWITCH_RESP"
    SWITCH_STATUS=$(echo "$SWITCH_RESP" | jq -r '.status // empty' 2>/dev/null)
    SWITCH_MODE=$(echo "$SWITCH_RESP" | jq -r '.mode // empty' 2>/dev/null)

    if [ "$SWITCH_STATUS" = "ok" ] && [ "$SWITCH_MODE" = "$MODE" ]; then
        pass "/perm/switch to $MODE returned ok"
    else
        fail "/perm/switch to $MODE failed: $SWITCH_RESP"
    fi

    sleep 2
    pane_log "[perm_switch] AFTER perm/switch to $MODE"

    # Verify via /perm/status
    STATUS_RESP=$(curl -s "http://127.0.0.1:$TEST_PORT/perm/status?target=$ENCODED_TARGET")
    echo "  DEBUG: STATUS_RESP (${#STATUS_RESP} chars): $STATUS_RESP"
    STATUS_MODE=$(echo "$STATUS_RESP" | jq -r '.mode // empty' 2>/dev/null)

    if [ "$STATUS_MODE" = "$MODE" ]; then
        pass "/perm/status confirms mode=$MODE"
    else
        fail "/perm/status expected $MODE, got $STATUS_MODE"
    fi

    # Verify bot log contains Perm switch API entry for this mode
    if awk -v start="$((LOG_BEFORE + 1))" -v pat="Perm switch API.*mode=$MODE" 'NR>=start && $0~pat{found=1; exit} END{exit !found}' "$LOG_FILE"; then
        pass "Perm switch API log found for $MODE"
    else
        fail "Perm switch API log not found for $MODE"
    fi
done

# Restore bypass mode before cleanup so stop_claude /exit works without permission prompts
curl -s "http://127.0.0.1:$TEST_PORT/perm/switch?target=$ENCODED_TARGET&mode=bypass" > /dev/null 2>&1 || true
sleep 1
