#!/bin/bash
# Stage-1 smoke test (round 8): verify "async": true on PreToolUse/PostToolUse works on the current CC
# version in the isolated test env — the hook still fires, CC does not block, events still arrive, and
# both PreToolUse AND MessageDisplay CC payloads carry prompt_id (needed for the round-8 ordering fix).
# Runs standalone via the isolated harness (setup_hooks isolates CODEX_HOME + CC settings + config dir).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/cc_common.sh"

echo ""
echo "--- Stage 1 async hooks smoke test ---"

ensure_infrastructure

LOG_BEFORE=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
start_claude "e2e-async-smoke"

pane_log "[async-smoke] before inject"
inject_prompt "Run exactly this one shell command and nothing else, then stop: echo async_smoke_marker_ok"
pane_log "[async-smoke] after inject"

wait_for_idle
pane_log "[async-smoke] after idle"

SLICE=$(tail -n +"$((LOG_BEFORE + 1))" "$LOG_FILE")

# S1: PreToolUse delivered to the bot even though its hook is async (async must not drop delivery)
if grep -q 'Raw hook payload \[PreToolUse\]:' <<< "$SLICE"; then
  pass "async-smoke: PreToolUse hook delivered to bot under async"
else
  fail "async-smoke: no PreToolUse raw payload in bot log (async broke delivery?)"
fi

# S2: PostToolUse delivered to the bot under async
if grep -q 'Raw hook payload \[PostToolUse\]:' <<< "$SLICE"; then
  pass "async-smoke: PostToolUse hook delivered to bot under async"
else
  fail "async-smoke: no PostToolUse raw payload in bot log (async broke delivery?)"
fi

# S3: prompt_id present on PreToolUse payload (round-8 B1/B2 correlation depends on it)
if grep 'Raw hook payload \[PreToolUse\]:' <<< "$SLICE" | grep -q '"prompt_id"'; then
  pass "async-smoke: PreToolUse payload carries prompt_id"
else
  fail "async-smoke: PreToolUse payload missing prompt_id on this CC version"
fi

# S4: prompt_id present on MessageDisplay payload (bot runs --debug so raw MD is logged)
if grep 'Raw hook payload \[MessageDisplay\]:' <<< "$SLICE" | grep -q '"prompt_id"'; then
  pass "async-smoke: MessageDisplay payload carries prompt_id"
else
  fail "async-smoke: MessageDisplay payload missing prompt_id on this CC version"
fi

echo "  [async-smoke] complete."
