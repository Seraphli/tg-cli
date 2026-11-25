package helpers

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/internal/injector"
)

// ccRewindRows is the height of the cc pane: E1 (a) showed "↑ 2 more" at the start position with the 3 messages of this
// test at 20 rows (the window holds 2 entries), so the list read has to scroll and no other height is needed.
const ccRewindRows = 20

// ccWorkDir is the working directory of the pane (liveStart does not return it); cc's Write tool creates note.txt there.
func ccWorkDir(t *testing.T, target injector.TmuxTarget) string {
	t.Helper()
	out, err := exec.Command("tmux", "-L", injector.ServerName, "display-message", "-p", "-t", target.PaneID, "#{pane_current_path}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// TestRewindLiveCC runs the real cc in acceptEdits mode (the Write tool runs without a dialog) through TC-B1: the list
// read with scrolling, Restore conversation of a multi-line message on a 4-option entry and Restore code on a 6-option
// entry, with TC-B2 (a stale list is refused and nothing is rewound) between steps 1 and 2; steps 2 and 3 also fail when
// cc summarized the conversation instead. Restore conversation runs first on the 4-option layout of the no-code P3 entry;
// after Restore code cc offers later entries the 6-option layout, so the order matters for the label lookup.
func TestRewindLiveCC(t *testing.T) {
	target := liveStart(t, "cc", ccRewindRows, "acceptEdits")
	const (
		p2 = "Create a file named note.txt containing hello, using the Write tool. Then reply with exactly: beta"
		p3 = "Reply with exactly: gamma\nThis is the second line of the prompt."
		// p4 quotes the three cc menu texts: they stay on screen as conversation text and must not be taken for a menu
		p4 = "Reply with exactly: delta. Enter to continue · Esc to cancel · Confirm you want to restore to the point before you sent this message: · Restore the code and/or conversation to the point before"
	)
	note := filepath.Join(ccWorkDir(t, target), "note.txt")
	noteExists := func() bool { _, err := os.Stat(note); return err == nil }
	list := func(n int) []RewindEntry {
		t.Helper()
		entries, more, err := RewindList(target, "cc", 10, noRewindCheck)
		if err != nil || more || len(entries) != n {
			liveFail(t, target, "RewindList returned %q, more %v, error %v; want %d entries and no more", entries, more, err, n)
		}
		return entries
	}
	noMenu := func() {
		t.Helper()
		if ccMenuOpen(viewportNow(target)) {
			liveFail(t, target, "a rewind menu is on screen")
		}
	}
	emptyBox := func() {
		t.Helper()
		if text, ok := ccBoxNow(target); !ok || text != "" {
			liveFail(t, target, "the input box is %q (box found: %v), want it empty", text, ok)
		}
	}
	// cc prints "Summarized conversation" after any summarize action; a real restore leaves no such line
	noSummary := func() {
		t.Helper()
		if strings.Contains(viewportNow(target), "Summarized") {
			liveFail(t, target, "the conversation was summarized instead of restored")
		}
	}
	// on the fullscreen renderer (alternate screen, history 0) the viewport holds the conversation after a menu closes, so the assertions look at the replies in the viewport
	hasReplies := func(replies ...string) {
		t.Helper()
		pane := viewportNow(target)
		for _, r := range replies {
			if !hasReplyLine(pane, r) {
				liveFail(t, target, "no reply line %q in the checked viewport:\n%s", r, pane)
			}
		}
	}
	liveTurn(t, target, "Reply with exactly: alpha", "alpha")
	liveTurn(t, target, p2, "beta")
	liveTurn(t, target, p3, "gamma")
	// precondition (C5): the window at the start position must show "↑ N more", so the read in step 1 has to scroll
	var window ccWindow
	if err := injector.WithInjectLock(target, func() error {
		if err := (ccRewind{}).open(target); err != nil {
			return err
		}
		window = parseCCWindow(viewportNow(target))
		return ccRewind{}.close(target)
	}); err != nil {
		liveFail(t, target, "read the window at the start position: %v", err)
	}
	if !window.Above {
		liveFail(t, target, "window holds the whole list, lower ccRewindRows")
	}
	// step 1
	entries := list(3)
	want := []RewindEntry{{"Reply with exactly: gamma…", "No code changes"}, {p2, "note.txt +1"}, {"Reply with exactly: alpha", "No code changes"}}
	if !slices.Equal(entries, want) {
		liveFail(t, target, "entries %q, want %q", entries, want)
	}
	noMenu()
	// TC-B2: P4 makes the stored list stale; index 1 now holds P3, not P2
	liveTurn(t, target, p4, "delta")
	if _, err := RewindDrive(target, "cc", 1, entries[1], "Restore conversation", noRewindCheck); !errors.Is(err, ErrRewindListChanged) {
		liveFail(t, target, "RewindDrive on the stale list returned %v, want ErrRewindListChanged", err)
	}
	noMenu()
	emptyBox()
	if !noteExists() {
		liveFail(t, target, "note.txt is gone after the refused drive")
	}
	hasReplies("gamma", "delta")
	// step 2: Restore conversation of the two-line P3 (4-option layout, no code changes) puts both lines back into the box
	entries = list(4)
	if entries[1].Label != want[0].Label {
		liveFail(t, target, "entry 1 is %q, want label %q", entries[1], want[0].Label)
	}
	res, err := RewindDrive(target, "cc", 1, entries[1], "Restore conversation", noRewindCheck)
	if err != nil || res.Restored != p3 {
		liveFail(t, target, "Restore conversation: error %v, restored %q, want %q", err, res.Restored, p3)
	}
	if v := viewportNow(target); !hasReplyLine(v, "beta") || hasReplyLine(v, "gamma") || hasReplyLine(v, "delta") {
		liveFail(t, target, "the viewport must show the beta reply and neither the gamma nor the delta reply")
	}
	if !noteExists() {
		liveFail(t, target, "note.txt is gone after Restore conversation")
	}
	noSummary()
	emptyBox()
	// step 3: Restore code on the 6-option entry of P2
	entries = list(2)
	if entries[0] != want[1] || entries[1] != want[2] {
		liveFail(t, target, "entries %q, want %q and %q", entries, want[1], want[2])
	}
	res, err = RewindDrive(target, "cc", 0, entries[0], "Restore code", noRewindCheck)
	if err != nil || res.Restored != "" {
		liveFail(t, target, "Restore code: error %v, restored %q, want neither", err, res.Restored)
	}
	if noteExists() {
		liveFail(t, target, "note.txt still exists after Restore code")
	}
	noSummary()
	hasReplies("beta")
	emptyBox()
	noMenu()
}

// TestRewindLiveCCLongMessage: cc keeps a paste of one paragraph this long in one of two forms: a "[Pasted text #N]"
// placeholder in most runs (E6, round 2) and plain text in some (E8, round 8). Both are accepted, as cc shows the message.
// Placeholder: the drive must report InBox, return no text and leave the placeholder in the box. Plain text: the drive
// must return the message (the box wraps it over several rows and the reader joins the rows with a newline where the pane
// wrapped it, in place of the space) with InBox false and the box cleared.
func TestRewindLiveCCLongMessage(t *testing.T) {
	target := liveStart(t, "cc", 50, "default")
	msg := "Please reply with exactly the word ok and nothing else. " + strings.Repeat("This morning I reviewed the open pull requests and left comments on two of them. ", 18)
	liveSend(t, target, msg)
	entries, _, err := RewindList(target, "cc", 10, noRewindCheck)
	if err != nil || len(entries) != 1 {
		liveFail(t, target, "RewindList returned %q, error %v, want 1 entry", entries, err)
	}
	res, err := RewindDrive(target, "cc", 0, entries[0], "Restore conversation", noRewindCheck)
	text, ok := ccBoxNow(target)
	switch {
	case err == nil && res.InBox && res.Restored == "" && ok && strings.Contains(text, "[Pasted text #"):
		// placeholder form
	case err == nil && !res.InBox && ok && text == "" && strings.ReplaceAll(res.Restored, "\n", " ") == strings.TrimRight(msg, " "):
		// plain text form
	default:
		liveFail(t, target, "Restore conversation: error %v, InBox %v, restored %q, box %q (found %v); want no error and either the placeholder in the box with InBox and no text, or the plain message restored with an empty box and no InBox (both forms were wanted)", err, res.InBox, res.Restored, text, ok)
	}
}

// TestRewindLiveCCMenuLeftOpen: a user opened cc's Rewind menu (Escape twice on the idle box) and moved the cursor up to
// an older entry, then asked the bot for /rewind. read lists from the cursor on, so RewindList must refuse and close the
// menu instead of returning a list without the newer messages; the next RewindList lists from the newest message.
func TestRewindLiveCCMenuLeftOpen(t *testing.T) {
	target := liveStart(t, "cc", ccRewindRows, "default")
	for _, w := range []string{"alpha", "beta", "gamma"} {
		liveTurn(t, target, "Reply with exactly: "+w, w)
	}
	injector.SendKeys(target, "Escape", "Escape")
	v := liveWait(t, target, 5*time.Second, func(v string) bool { return strings.Contains(ccMenuBlock(v), ccMenuFooter) })
	for i := 0; i < 2; i++ {
		var ok bool
		if v, ok = press(target, v, "Up"); !ok {
			liveFail(t, target, "Up %d did not move the cursor", i+1)
		}
	}
	if row, _, found := parseCCWindow(v).selected(); !found || row.Entry.Label != "Reply with exactly: beta" {
		liveFail(t, target, "the cursor is on %q (found %v), want the entry beta", row.Entry.Label, found)
	}
	entries, _, err := RewindList(target, "cc", 10, noRewindCheck)
	if !errors.Is(err, ErrRewindNoMenu) || len(entries) != 0 {
		liveFail(t, target, "RewindList with the menu left open returned %q, error %v, want ErrRewindNoMenu and no list", entries, err)
	}
	if ccMenuOpen(viewportNow(target)) {
		liveFail(t, target, "the menu that was left open is still on screen")
	}
	if text, ok := ccBoxNow(target); !ok || text != "" {
		liveFail(t, target, "the input box is %q (box found: %v), want it back and empty", text, ok)
	}
	entries, more, err := RewindList(target, "cc", 10, noRewindCheck)
	want := []RewindEntry{{"Reply with exactly: gamma", "No code changes"}, {"Reply with exactly: beta", "No code changes"}, {"Reply with exactly: alpha", "No code changes"}}
	if err != nil || more || !slices.Equal(entries, want) {
		liveFail(t, target, "the second RewindList returned %q, more %v, error %v, want %q", entries, more, err, want)
	}
}
