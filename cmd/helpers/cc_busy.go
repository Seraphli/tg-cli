package helpers

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/injector"
)

// Pinned cc 2.1.261 spinner/done glyph set: · ✢ * ✻ ✽ ✶ . The inter-token separator after the glyph is
// an explicit [ \x{00A0}]+ class (ASCII space OR NBSP) — NEVER bare \s (Go RE2 \s is ASCII-only).
// spinnerLine is the STOP test (glyph + separator + an ASCII TitleCase verb start). spinnerBusyLine is the
// BUSY test: glyph + separator + any run + a trailing … (U+2026). The middle is `.*` (any run), NOT an ASCII
// verb class, so accented or multi-word status verbs (e.g. "Flambéing…", "Compacting conversation…") still
// classify BUSY — an ASCII [A-Z][a-z]+ verb class dropped the accented/second word before the ellipsis and
// misread a busy spinner as idle.
// spinnerWaitLine is a second BUSY test for the stall banner: when CC's stream watchdog (timeout kt=20000ms)
// marks the in-flight response kind=stalled on a mid-turn chunk gap of >= 20s, it renders "Waiting for API
// response · will retry in <t> · check your network" in the spinner slot. That banner has NO trailing … so
// spinnerBusyLine misses it, yet the turn is still open (the model has not stopped) — so it must classify BUSY.
var (
	spinnerLine     = regexp.MustCompile(`^[·✢*✻✽✶][ \x{00A0}]+[A-Z][a-z]`)               // STOP test
	spinnerBusyLine = regexp.MustCompile(`^[·✢*✻✽✶][ \x{00A0}]+.*…`)                      // BUSY test (trailing U+2026; any verb run)
	spinnerWaitLine = regexp.MustCompile(`^[·✢*✻✽✶][ \x{00A0}]+Waiting for API response`) // BUSY test (stall-watchdog banner; no …)
)

// isRuleLine reports whether the trimmed line is a full-width box rule: its LEADING run of ─ (U+2500) is
// >= 40 runes. The border may carry an embedded status/label text after the leading dashes.
func isRuleLine(t string) bool {
	n := 0
	for _, r := range t {
		if r != '─' {
			break
		}
		n++
	}
	return n >= 40
}

// ccBusyFromViewport is the pure classifier: box-finding + spinner-glyph scan-up. See CLASSIFIER-SPEC v3.3.
func ccBusyFromViewport(viewport string) bool {
	lines := strings.Split(viewport, "\n")
	// (1) Box-finding: the BOTTOM-MOST region between two rule lines whose FIRST enclosed line (trimmed)
	// starts with ❯ (empty OR carrying a wrapped/numbered draft; NO ❯\d+. exclusion). Anchor = top rule.
	var ruleIdx []int
	for i, ln := range lines {
		if isRuleLine(strings.TrimSpace(ln)) {
			ruleIdx = append(ruleIdx, i)
		}
	}
	anchor := -1
	for k := 0; k+1 < len(ruleIdx); k++ {
		a, b := ruleIdx[k], ruleIdx[k+1]
		if b <= a+1 {
			continue // no enclosed line
		}
		if strings.HasPrefix(strings.TrimSpace(lines[a+1]), "❯") {
			anchor = a // keep last -> bottom-most
		}
	}
	if anchor == -1 {
		return false // dialog / no input box -> IDLE
	}
	// (2) Spinner-glyph scan-up from the anchor: first spinner/done line -> STOP; BUSY iff it carries a
	// head-anchored trailing … (spinnerBusyLine) OR is the stall-watchdog "Waiting for API response" banner.
	for i := anchor - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if spinnerLine.MatchString(t) {
			return spinnerBusyLine.MatchString(t) || spinnerWaitLine.MatchString(t)
		}
	}
	return false
}

// captureViewport is the helper-local seam (#4) so unit tests can stub the capture.
var captureViewport = injector.CaptureViewport

// ccBusyFromContent captures the cc pane viewport (500ms bound) and classifies it. Capture error -> IDLE.
func ccBusyFromContent(ctx context.Context, tmuxTarget string) bool {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return false
	}
	ctx2, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	out, err := captureViewport(ctx2, target)
	if err != nil {
		return false
	}
	return ccBusyFromViewport(out)
}
