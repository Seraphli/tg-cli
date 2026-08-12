#!/bin/bash
set -euo pipefail
source "${SCRIPT_DIR:=$(cd "$(dirname "$0")" && pwd)}/pi_common.sh"

# R5: pi image/file inject WITH A CAPTION must confirm and the caption must survive.
# Bug (production 2026-08-12): sending a file to a pi session WITH a caption reported "image inject failed:
# inject not confirmed" and DROPPED the caption. Root cause: ask.go's general "prompt" confirm path had no pi
# branch, so pi's glyph-less rule-bordered composer never matched the prompt-char scan; the image+caption path
# is submit=false so there was no UserPromptSubmit fallback → ErrInjectNotConfirmed → messages.go:208 returns →
# the InjectTextAppend caption at :215 never runs. A file/image sent with NO caption already worked (submit=true
# → UPS fallback). Fix: pi composer check gated on backend==pi && !res.shouldSubmit.
#
# FOUR cases, GUARDS-FIRST (guards are submit=true; they must be GREEN on BOTH builds — if a guard is RED on the
# pre-fix build the caption-only reading is wrong → STOP, tell note3):
#   A. file-only  (.bin, submit=true)  — no-regression GUARD (path submitted via UPS; does not touch the pi branch).
#   B. plain text (submit=true)        — no-regression GUARD; also asserts capturePane=false (prompt-char scan missed).
#   C. .bin + CAPTION (submit=false)   — RED case: reproduces the boss's exact incident (a .zip rendered as a
#                                        LITERAL PATH in pi's composer → the snippet leg).
#   D. .jpg + CAPTION (submit=false)   — RED case: the REAL tests/test_image.jpg (an actual image, the ordinary
#                                        user action). pi's rendering of a pasted .jpg path is not independently
#                                        evidenced; the helper covers literal-path OR "[Image" chip. If D is RED
#                                        AFTER the fix, STOP and mail note3 the composer pane (pane_log below) —
#                                        do NOT widen the helper (that is a design change, note3's/the boss's call).
# Assertions use record_fail (non-exit), NOT fail (exit 1): so EVERY case runs in a single pre-fix stash/pop
# run and every assertion is independently RED-validated (nothing "implied" from a case sharing the code path).
# The suite/single-phase final report still exits 1 when any FAIL is recorded (e2e.sh counts the results file).

echo ""
echo "--- pi image/file inject test (R5: caption survives; 4 cases, guards-first) ---"

ensure_infrastructure
start_pi "e2e-pi-19"
pane_log "[pi_image_inject] BEFORE test"

TEST_BIN="/tmp/tg-cli-pi-fileinject-$$.bin"
printf 'tg-cli pi file-inject e2e payload\n' > "$TEST_BIN"
TEST_JPG="/tmp/tg-cli-pi-testimg-$$.jpg"
cp "$SCRIPT_DIR/../test_image.jpg" "$TEST_JPG"
CAP_BIN="PIIMGCAP$$"      # caption token for the .bin case
CAP_JPG="PIJPGCAP$$"      # caption token for the .jpg case (distinct, so each UPS is attributable)

# has HAYSTACK NEEDLE — fixed-string membership test, here-string (no pipe → no pipefail SIGPIPE trap).
has() { grep -qF -- "$2" <<< "$1"; }

# Every negative inject-failure check below is PANE-ANCHORED to this run's $E2E_PANE, NOT a bare phrase. The pi E2E
# model is agentic (repo-read + bash, cwd=worktree): given a file path it may read this very script or grep the
# results file, and its tool-results are logged at INFO as "Raw hook payload [PostToolUse]" inside the slice we
# grep — so a bare "inject not confirmed" can be replayed into the log by the model (this false-positived case A
# once). The two REAL product failure lines both carry the pane id (verified in a RED-run bot.log): ask.go INFO
# "inject not confirmed, target=<pane>" (comma) and the wrapped error logged by injection.go ERROR "inject not
# confirmed for target=<pane>" (the word "for", no comma — from ask.go:789 fmt.Errorf("%w for target=%s") →
# messages.go:208 "image inject failed: %w"). Anchoring to target=$E2E_PANE defeats both false-match vectors: the
# model cannot reproduce a runtime pane id, and a different pane's failure (e.g. an out-of-scope flushInjectQueue
# drop on another pane) will not match. BOTH forms are checked on every negative assertion — neither is dropped
# (the second maps to the boss's user-visible "image inject failed ... caption dropped" abort at messages.go:208).

# ---- Case A: file-only (.bin, submit=true) — no-regression GUARD ----
LOG_BEFORE_A=$(wc -l < "$LOG_FILE")
pane_log "[pi_image_inject] BEFORE file-only inject (submit=true guard)"
inject_prompt "" "$TEST_BIN" || true
wait_for_idle "$TIMEOUT" "$E2E_PANE"
pane_log "[pi_image_inject] AFTER file-only inject"
SLICE_A=$(tail -n +$((LOG_BEFORE_A + 1)) "$LOG_FILE")
if has "$SLICE_A" "inject not confirmed, target=$E2E_PANE" || has "$SLICE_A" "inject not confirmed for target=$E2E_PANE"; then
  record_fail "pi file-only (submit=true) GUARD: 'inject not confirmed' for $E2E_PANE — if RED on the pre-fix build, the caption-only reading is WRONG (STOP, tell note3)"
else
  pass "pi file-only (submit=true) GUARD: confirmed (no 'inject not confirmed')"
fi
if has "$SLICE_A" "UserPromptSubmit"; then
  pass "pi file-only (submit=true) GUARD: UserPromptSubmit present (path submitted via UPS)"
else
  record_fail "pi file-only (submit=true) GUARD: no UserPromptSubmit — if RED on the pre-fix build, the caption-only reading is WRONG (STOP, tell note3)"
fi

# ---- Case B: plain text (submit=true) — no-regression GUARD ----
LOG_BEFORE_B=$(wc -l < "$LOG_FILE")
pane_log "[pi_plaintext] BEFORE plain-text inject (submit=true guard)"
inject_prompt "reply with exactly one word: ok" || true
wait_for_idle "$TIMEOUT" "$E2E_PANE"
pane_log "[pi_plaintext] AFTER plain-text inject"
SLICE_B=$(tail -n +$((LOG_BEFORE_B + 1)) "$LOG_FILE")
if has "$SLICE_B" "inject not confirmed, target=$E2E_PANE" || has "$SLICE_B" "inject not confirmed for target=$E2E_PANE"; then
  record_fail "pi plain-text (submit=true) GUARD: 'inject not confirmed' for $E2E_PANE — if RED on the pre-fix build, the caption-only reading is WRONG (STOP, tell note3)"
else
  pass "pi plain-text (submit=true) GUARD: confirmed (no 'inject not confirmed')"
fi
# capturePane=false on the confirmed line: pi has no prompt glyph so the prompt-char scan always misses; the
# inject confirms via the submit=true UPS fallback. Per fablenote3 Q3.3 this is a NO-REGRESSION guard, NOT a
# gate proof (with the !res.shouldSubmit gate dropped, this success path still passes).
CONF_B=$(grep -F "inject confirmed" <<< "$SLICE_B" || true)
if has "$CONF_B" "capturePane=false"; then
  pass "pi plain-text (submit=true) GUARD: confirmed line carries capturePane=false (prompt-char scan missed, UPS fallback confirmed)"
else
  record_fail "pi plain-text (submit=true) GUARD: confirmed line missing capturePane=false"
fi

# ---- Case C: .bin + CAPTION (submit=false) — RED case (the boss's exact incident: literal-path snippet leg) ----
LOG_BEFORE_C=$(wc -l < "$LOG_FILE")
pane_log "[pi_bin_caption] BEFORE .bin+caption inject (submit=false, RED case)"
inject_prompt "reply with exactly one word: $CAP_BIN" "$TEST_BIN" || true
wait_for_idle "$TIMEOUT" "$E2E_PANE"
pane_log "[pi_bin_caption] AFTER .bin+caption inject"
SLICE_C=$(tail -n +$((LOG_BEFORE_C + 1)) "$LOG_FILE")
if has "$SLICE_C" "inject not confirmed, target=$E2E_PANE" || has "$SLICE_C" "inject not confirmed for target=$E2E_PANE"; then
  record_fail "pi .bin+caption (submit=false): inject FAILED (pane-anchored 'inject not confirmed ... target=$E2E_PANE' — comma INFO or 'for' ERROR / caption dropped) [BUILD-RED on the pre-fix build]"
else
  pass "pi .bin+caption (submit=false): inject confirmed (no 'inject not confirmed' / 'image inject failed')"
fi
UPS_C=$(grep -F "UserPromptSubmit" <<< "$SLICE_C" || true)
if has "$UPS_C" "$CAP_BIN"; then
  pass "pi .bin+caption (submit=false): UserPromptSubmit carries the caption ($CAP_BIN)"
else
  record_fail "pi .bin+caption (submit=false): no UserPromptSubmit carrying the caption ($CAP_BIN) — caption dropped [BUILD-RED on the pre-fix build]"
fi

# ---- Case D: .jpg + CAPTION (submit=false) — RED case (the REAL image; unverified .jpg composer rendering) ----
LOG_BEFORE_D=$(wc -l < "$LOG_FILE")
pane_log "[pi_jpg_caption] BEFORE .jpg+caption inject (submit=false, RED case, real image)"
inject_prompt "reply with exactly one word: $CAP_JPG" "$TEST_JPG" || true
wait_for_idle "$TIMEOUT" "$E2E_PANE"
pane_log "[pi_jpg_caption] AFTER .jpg+caption inject (if RED post-fix: THIS pane shows how pi rendered the .jpg — mail note3)"
SLICE_D=$(tail -n +$((LOG_BEFORE_D + 1)) "$LOG_FILE")
if has "$SLICE_D" "inject not confirmed, target=$E2E_PANE" || has "$SLICE_D" "inject not confirmed for target=$E2E_PANE"; then
  record_fail "pi .jpg+caption (submit=false): inject FAILED (pane-anchored 'inject not confirmed ... target=$E2E_PANE') — RED on pre-fix is expected; RED AFTER the fix means pi renders the .jpg as NEITHER a literal path NOR '[Image' → STOP, mail note3 the composer pane (do NOT widen the helper)"
else
  pass "pi .jpg+caption (submit=false): inject confirmed (real image; no 'inject not confirmed' / 'image inject failed')"
fi
UPS_D=$(grep -F "UserPromptSubmit" <<< "$SLICE_D" || true)
if has "$UPS_D" "$CAP_JPG"; then
  pass "pi .jpg+caption (submit=false): UserPromptSubmit carries the caption ($CAP_JPG)"
else
  record_fail "pi .jpg+caption (submit=false): no UserPromptSubmit carrying the caption ($CAP_JPG) — RED on pre-fix is expected; RED AFTER the fix → STOP, mail note3 the composer pane"
fi

rm -f "$TEST_BIN" "$TEST_JPG"
echo "  pi image/file inject tests complete."
