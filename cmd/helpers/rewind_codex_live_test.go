package helpers

import (
	"slices"
	"strings"
	"testing"
)

// TestRewindLiveCodex (TC-B3) runs the real codex through three turns, lists the prompts from the transcript overlay and
// rewinds to the two-line second one. The list is read from the reverse-video selection of the ANSI capture; after the
// rewind the two-line prompt is back in the composer and must be read in full and then cleared row by row (one C-u
// clears only the cursor row).
func TestRewindLiveCodex(t *testing.T) {
	const (
		c1 = "Reply with exactly: alpha"
		c2 = "Reply with exactly: beta\nThis is the second line of the prompt."
		// c3's second line quotes the overlay footer: it stays on screen as conversation text and must not be taken for the overlay
		c3 = "Reply with exactly: gamma\nBrowsing transcript · ↑↓/jk scroll · ←→/hl prompts · ctrl+t details · ↵ rewind · esc back"
	)
	target := liveStart(t, "codex", 50, "")
	liveTurn(t, target, c1, "alpha")
	liveTurn(t, target, c2, "beta")
	liveTurn(t, target, c3, "gamma")
	entries, _, err := RewindList(target, "codex", 10, noRewindCheck)
	if err != nil {
		liveFail(t, target, "RewindList: %v", err)
	}
	if want := []RewindEntry{{Label: c3}, {Label: c2}, {Label: c1}}; !slices.Equal(entries, want) {
		liveFail(t, target, "entries = %q, want %q", entries, want)
	}
	res, err := RewindDrive(target, "codex", 1, entries[1], "Rewind conversation to here", noRewindCheck)
	if err != nil {
		liveFail(t, target, "RewindDrive: %v", err)
	}
	v := viewportNow(target)
	if !strings.Contains(v, "Conversation reverted to this point. File changes are unchanged.") {
		liveFail(t, target, "the viewport does not show the reverted message")
	}
	if res.Restored != c2 {
		liveFail(t, target, "restored text = %q, want %q", res.Restored, c2)
	}
	if _, placeholder, _ := codexComposer(v); !placeholder {
		liveFail(t, target, "the composer does not show the placeholder %q", codexPlaceholder)
	}
	if codexOverlayOpen(v) {
		liveFail(t, target, "the transcript overlay is still open")
	}
	if hasReplyLine(v, "gamma") {
		liveFail(t, target, "the reply to the rewound prompt is still in the viewport")
	}
}
