#!/bin/bash
# Phase 12 = Round-2 Item 3: a pi built-in slash command inject must CONFIRM (not falsely report
# ErrInjectNotConfirmed). pi intercepts its 25 built-ins before prompt() so NO UserPromptSubmit is emitted;
# the pre-fix code falls to the CapturePane+UPS path (pi has no ❯/› prompt glyph and emits no UPS) and times
# out into ErrInjectNotConfirmed even though the command ran. Fix: the local-slash transaction (generalised
# from codex) OWNs the Enter and compose-confirms via pi's last-two-rules composer, skipping the Working poll
# (a pi local slash starts no agent run).
#
# note3 CHANGE 3 — this MUST exercise the BOTTOM-anchoring, not just "find rules". So it runs /hotkeys ONCE
# first (handleHotkeysCommand wraps its output in DynamicBorder at both ends -> a PRIOR rule pair above the
# composer), THEN injects /hotkeys as the slash-under-test. With a prior rule pair present, "last two rules"
# (correct) finds the composer and confirms, whereas "any two rules" would scan the stale /hotkeys box and
# veto — so the test discriminates the design.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi Item3: built-in slash inject confirms ---"

ensure_infrastructure
start_pi "e2e-pi-12"

# inject_prompt is made non-fatal here: on a PRE-FIX build the pi-slash inject fails confirmation, so
# SafeInjectText returns ErrInjectNotConfirmed and /inject/message answers HTTP 500 (inject_prompt -> 1). The
# command STILL executes (pi renders the box) and the bot log records the outcome, so the phase must not die
# at the inject under `set -e` — the log-based asserts below are the real discriminator.

# ---- Setup: run /hotkeys once to leave a PRIOR full-width rule pair ABOVE the composer ----
pane_log "[pi/item3] before setup /hotkeys"
inject_prompt "/hotkeys" || true
sleep 5   # /hotkeys is a local command (no agent run) — let the TUI render its bordered box.
pane_log "[pi/item3] after setup /hotkeys (a rule pair should now sit above the composer)"

# ---- Test: inject /hotkeys again; it must CONFIRM via the pi-slash transaction ----
LOG_T_BEFORE=$(wc -l < "$LOG_FILE")
inject_prompt "/hotkeys" || true

# Poll the bot log for the pi-slash confirmation outcome (confirmed or not-confirmed) for this inject.
OUTCOME=""
for i in $(seq 1 40); do
  SLICE=$(tail -n +"$((LOG_T_BEFORE + 1))" "$LOG_FILE")
  if printf '%s\n' "$SLICE" | grep -q "pi slash inject confirmed"; then OUTCOME="confirmed"; break; fi
  if printf '%s\n' "$SLICE" | grep -q "inject not confirmed"; then OUTCOME="not_confirmed"; break; fi
  sleep 1
done
SLICE=$(tail -n +"$((LOG_T_BEFORE + 1))" "$LOG_FILE")
pane_log "[pi/item3] after test /hotkeys inject (outcome=$OUTCOME)"
echo "  DEBUG: test-inject outcome = $OUTCOME"

set +eo pipefail
printf '%s\n' "$SLICE" | grep -q "pi slash inject confirmed"
_ps_ok=("${PIPESTATUS[@]}")
printf '%s\n' "$SLICE" | grep -qE "safeInjectText: inject not confirmed|pi slash compose not confirmed"
_ps_bad=("${PIPESTATUS[@]}")
set -eo pipefail
[ "${_ps_ok[1]}" -eq 0 ] && pass "pi Item3: built-in slash /hotkeys inject CONFIRMED (pi-slash transaction)" \
  || record_fail "pi Item3: /hotkeys inject NOT confirmed — pi built-in slash still reports ErrInjectNotConfirmed"
[ "${_ps_bad[1]}" -ne 0 ] && pass "pi Item3: no ErrInjectNotConfirmed for the /hotkeys inject" \
  || record_fail "pi Item3: ErrInjectNotConfirmed present for /hotkeys — the false inject failure was not fixed"

echo "  pi Item3 slash-inject test complete."
