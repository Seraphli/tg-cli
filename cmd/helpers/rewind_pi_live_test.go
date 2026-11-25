package helpers

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// piSessionHeaders returns the first line of every session file (*.jsonl) under the private config dir of the live pi:
// the session header record, which carries the session id.
func piSessionHeaders(t *testing.T) []string {
	t.Helper()
	var heads []string
	err := filepath.WalkDir(filepath.Join(liveCfgDir, "sessions"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		head, _, _ := strings.Cut(string(b), "\n")
		heads = append(heads, head)
		return nil
	})
	if err != nil {
		t.Fatalf("read the pi session files: %v", err)
	}
	return heads
}

// TestRewindLivePi (TC-B4) runs the real pi through three turns, lists the /tree user messages newest first, rewinds to
// the middle one (a two-line prompt) with "No summary" and checks that both lines are back in the editor, that the
// editor is cleared afterwards and that the rewind kept the same single session file and session id (/tree, not /fork).
func TestRewindLivePi(t *testing.T) {
	target := liveStart(t, "pi", 50, "")
	// r3's second line quotes the pi tree and dialog titles and r1's the spinner words: they stay on screen as conversation text (r1 stays on the kept path after the rewind to r2) and must not be taken for a menu or for the progress line
	r1, r2, r3 := "Reply with exactly: alpha\nSummarizing branch", "Reply with exactly: beta\nThis is the second line of the prompt.", "Reply with exactly: gamma\nSession Tree Summarize branch?"
	liveTurn(t, target, r1, "alpha")
	liveTurn(t, target, r2, "beta")
	liveTurn(t, target, r3, "gamma")
	before := piSessionHeaders(t)
	entries, _, err := RewindList(target, "pi", 10, noRewindCheck)
	if err != nil {
		liveFail(t, target, "RewindList: %v", err)
	}
	// the /tree rows show a newline as a space
	labels := []string{strings.ReplaceAll(r3, "\n", " "), strings.ReplaceAll(r2, "\n", " "), strings.ReplaceAll(r1, "\n", " ")}
	if len(entries) != len(labels) {
		liveFail(t, target, "RewindList returned %d entries %q, want %d", len(entries), entries, len(labels))
	}
	for i, label := range labels {
		if entries[i].Label != label {
			liveFail(t, target, "entry %d has the label %q, want %q", i, entries[i].Label, label)
		}
	}
	res, err := RewindDrive(target, "pi", 1, entries[1], "No summary", noRewindCheck)
	if err != nil {
		liveFail(t, target, "RewindDrive: %v", err)
	}
	if after := piSessionHeaders(t); len(before) != 1 || len(after) != 1 || before[0] != after[0] {
		liveFail(t, target, "the rewind changed the session file or id: headers before %q, after %q", before, after)
	}
	if res.Restored != r2 {
		liveFail(t, target, "restored text is %q, want %q", res.Restored, r2)
	}
	v := viewportNow(target)
	if !strings.Contains(v, "Navigated to selected point") {
		liveFail(t, target, "the viewport has no \"Navigated to selected point\"")
	}
	if text, _ := piBoxText(v); text != "" {
		liveFail(t, target, "the editor holds %q, want it empty", text)
	}
	if piMenuOpen(v) {
		liveFail(t, target, "the tree or the summary dialog is still on screen")
	}
}
