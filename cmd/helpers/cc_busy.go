package helpers

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
)

// Pinned cc 2.1.261 spinner/done glyph set: · ✢ * ✻ ✽ ✶ . The inter-token separator after the glyph is
// an explicit [ \x{00A0}]+ class (ASCII space OR NBSP) — NEVER bare \s (Go RE2 \s is ASCII-only).
// spinnerGlyphLine finds a glyph line; ONLY the first one above the input box decides the state, in this order:
//   - b. the done line (doneLine: "✻ Cooked for 1m 43s · done 8:34 AM · 2 shells still running"): idle at once.
//   - w. the background-agent wait line (agentWaitLine: "✻ Waiting for 2 background agents to finish"): cc shows it
//     after the Stop-hook phase of a turn that leaves background agents running, until their reports start the next
//     turn. Idle whatever CCActive says (boss ruling, fix round 3): a trailing MessageDisplay re-stamps CCActive
//     after the Stop, and rule e would read the line working for the rest of the 4 s stamp.
//   - k. the compaction line (compactingLine: "Compacting conversation…", with or without a parenthetical: 2.1.261
//     prints none for about 14 s): compacting. A manual /compact and an automatic compaction print the same line.
//     The hook-phase lines ("Running PreCompact hooks…", "Running PostCompact hooks…") are NOT this rule: they read
//     working through rule d, because the PreCompact line also appears when cc compacts nothing.
//   - c. the stall banner (stallBanner): when CC's stream watchdog (timeout kt=20000ms) marks the in-flight
//     response kind=stalled on a mid-turn chunk gap of >= 20s, it renders "Waiting for API response · will
//     retry in <t> · check your network" in the spinner slot: waiting, with the countdown after "will retry in ".
//   - r. the retry banner (retryBanner: "✻ Connection dropped (ECONNRESET) · Retrying in 27s · attempt 7/10"): cc
//     retries an API error itself, up to about 3 minutes, before it ends the turn. The banner has no …, so rule d
//     does not take it: waiting, with the countdown after "Retrying in " (up to a "(reset)" parenthetical or the
//     next " · "). The hang case (the server never answers) shows "Retrying in 0s" for as long as the pane does.
//   - d. any other glyph line with a … (U+2026): working. Whatever stands between glyph and … does not matter
//     (a task title in Chinese, lowercase or accented).
//   - e. none of these (no input box, no glyph line, a glyph line without …): working while the 4 s hook signal
//     CCActive is fresh, else idle.
var (
	spinnerGlyphLine = regexp.MustCompile(`^[·✢*✻✽✶][ \x{00A0}]+`)
	doneLine         = regexp.MustCompile(`^✻ .+ for .+ · done \d{1,2}:\d{2}( [AP]M)?( · \d+ shells? still running)?$`)
	compactingLine   = regexp.MustCompile(`^[·✢*✻✽✶][ \x{00A0}]+Compacting conversation…`)
	stallBanner      = regexp.MustCompile(`^[·✢*✻✽✶][ \x{00A0}]+Waiting for API response`)
	stallCountdown   = regexp.MustCompile(`will retry in (.+?)(?: · |$)`)
	retryBanner      = regexp.MustCompile(`^[·✢*✻✽✶][ \x{00A0}]+.* · Retrying in (.+?)(?: \([^)]*\))? · attempt \d+/\d+`)
	agentWaitLine    = regexp.MustCompile(`^[·✢*✻✽✶][ \x{00A0}]+Waiting for \d+ background agents? to finish$`)
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

// ccBusyFromViewport is the pure classifier: box-finding + the first glyph line above the box (see the
// rules above). The countdown is set only for the waiting state.
func ccBusyFromViewport(viewport string, ccActive bool) (stores.BusyState, string) {
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
	// (2) The first glyph line above the anchor decides; nothing above it is looked at. No box (a dialog)
	// means no decision from the pane.
	for i := anchor - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if !spinnerGlyphLine.MatchString(t) {
			continue
		}
		switch {
		case doneLine.MatchString(strings.TrimRight(lines[i], " \t\r")):
			return stores.BusyIdle, ""
		case agentWaitLine.MatchString(t):
			return stores.BusyIdle, ""
		case compactingLine.MatchString(t):
			return stores.BusyCompacting, ""
		case stallBanner.MatchString(t):
			if m := stallCountdown.FindStringSubmatch(t); m != nil {
				return stores.BusyWaiting, m[1]
			}
			return stores.BusyWaiting, ""
		case retryBanner.MatchString(t):
			return stores.BusyWaiting, retryBanner.FindStringSubmatch(t)[1]
		case strings.Contains(t, "…"):
			return stores.BusyWorking, ""
		}
		break
	}
	if ccActive {
		return stores.BusyWorking, ""
	}
	return stores.BusyIdle, ""
}

// captureViewport is the helper-local seam (#4) so unit tests can stub the capture.
var captureViewport = injector.CaptureViewport

// capturePane captures the pane viewport (500ms bound). A bad target or a capture error gives no viewport, so
// the pane makes no decision.
func capturePane(ctx context.Context, tmuxTarget string) (string, error) {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return "", err
	}
	ctx2, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return captureViewport(ctx2, target)
}

// ccBusyFromContent captures the cc pane and classifies it. A bad target or a capture error gives no viewport,
// so the pane makes no decision (ccActive or idle). An idle reading while the hook store says a turn runs
// is written to bot.log (see diagnoseIdle).
func ccBusyFromContent(ctx context.Context, hookRunning *stores.HookRunningStateStore, tmuxTarget string) (stores.BusyState, string) {
	viewport, capErr := capturePane(ctx, tmuxTarget)
	state, countdown := ccBusyFromViewport(viewport, hookRunning.CCActive(tmuxTarget, stores.CCBusyTTL))
	if state == stores.BusyIdle {
		diagnoseIdle(hookRunning, tmuxTarget, viewport, capErr)
	}
	return state, countdown
}

// diagnoseIdle writes ONE bot.log line when a cc session turns idle (its stored state was busy) while the hook
// store says a turn runs: the capture error (swallowed otherwise) and the whole viewport. A session that stays
// idle writes nothing more: the stored state is idle from the next tick on. This keeps the pane of the idle
// reading in the middle of a turn that no run could reproduce (2026-11-16 23:40:18). A question or permission
// dialog and a turn interrupted after a tool call write the line too: the hook store says running, the pane
// reads idle.
func diagnoseIdle(hookRunning *stores.HookRunningStateStore, tmuxTarget, viewport string, capErr error) {
	if prev, _ := hookRunning.GetBusyState(tmuxTarget); prev == stores.BusyIdle {
		return
	}
	if running, known := hookRunning.IsRunning(tmuxTarget); !known || !running {
		return
	}
	capture := "ok"
	if capErr != nil {
		capture = "error " + capErr.Error()
	}
	logger.Warn(fmt.Sprintf("busy diagnostic: cc session %s turned idle while the hook store says a turn runs; capture %s; viewport:\n%s", tmuxTarget, capture, viewport))
}
