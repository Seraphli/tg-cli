package helpers

import (
	"os"
	"strings"
	"testing"
)

// The 21:04 verify failure of round 7: cc closed the Rewind menu, the box was recognised and empty and nothing had been rewound, so a wait for "no menu and a recognised box" said done; ccEffectFor must not confirm a restore on those two panes (testdata/cc_effect_2104_*.txt) and must confirm the settled panes of real restores (the message back in the box as two lines, as "[Pasted text #N]" or as plain text). A second Summarize in a session that already shows an earlier "Summarized conversation" line must not be confirmed at the digit key either: testdata/cc_effect_summarize.txt holds the frames F45 to F52 of one real "Summarize up to here" in such a session, and only F52 may confirm.
func TestRewindCCEffect(t *testing.T) {
	screen := func(name string) string {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	gamma := RewindEntry{Label: "Reply with exactly: gamma…", Annotation: "No code changes"}
	long := RewindEntry{Label: "Please reply with exactly the word ok and nothing else. This morning I reviewed the open pull requests and left comments on two of them. This morning I reviewed the open pull requests and l…", Annotation: "No code changes"}
	for _, c := range []struct {
		name string
		want RewindEntry
	}{{"cc_effect_2104_cc.txt", gamma}, {"cc_effect_2104_long.txt", long}} {
		if ccEffectFor(c.want, "Restore conversation")(screen(c.name)) {
			t.Errorf("%s: ccEffectFor confirmed a restore, but nothing was rewound on this pane", c.name)
		}
	}
	for _, c := range []struct {
		name string
		want RewindEntry
	}{{"cc_effect_ok_restore.txt", gamma}, {"cc_effect_ok_placeholder.txt", long}, {"cc_effect_ok_plain.txt", long}} {
		if !ccEffectFor(c.want, "Restore conversation")(screen(c.name)) {
			t.Errorf("%s: ccEffectFor did not confirm the restored message in the box", c.name)
		}
	}
	// one wait for all eight frames: it keeps state (the "Summarizing…" line seen inside the menu)
	wait := ccEffectFor(RewindEntry{Label: "Reply with exactly: alpha", Annotation: "No code changes"}, "Summarize up to here")
	for _, f := range strings.Split(screen("cc_effect_summarize.txt"), "=== ")[1:] {
		frame, v, _ := strings.Cut(f, " ===\n")
		if got, want := wait(v), frame == "F52"; got != want {
			t.Errorf("cc_effect_summarize.txt %s: ccEffectFor = %v, want %v", frame, got, want)
		}
	}
}

// fixtureFrames returns the frame texts of testdata/name in file order: the file holds frames cut from a live run, each one starting after a line "=== F<n> ===".
func fixtureFrames(t *testing.T, name string) []string {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var frames []string
	for _, f := range strings.Split(string(b), "=== ")[1:] {
		_, v, _ := strings.Cut(f, " ===\n")
		frames = append(frames, v)
	}
	return frames
}

// A Summarize starts on a pane that already shows an earlier "Summarized conversation" line. The model call fails (cc: "Failed to summarize: Compaction blocked by PreCompact hook") and the block is closed by hand with an Escape: ccEffectFor must say not done on every frame, the first screen after the close included (testdata/cc_summarize_failed.txt, frames F1 to F3 of a live run: the "Summarizing…" line, the error screen, the pane after the Escape), and the error screen must count as an open menu so that close sends its Escape. The same start with a completed summarize (testdata/cc_summarize_done.txt: F1 the "Summarizing…" line, F2 the first screen after the menu) is done at F2.
func TestRewindCCSummarizeStopped(t *testing.T) {
	entry := RewindEntry{Label: "Reply with exactly: alpha", Annotation: "No code changes"}
	stopped := fixtureFrames(t, "cc_summarize_failed.txt")
	check := ccEffectFor(entry, "Summarize up to here")
	for i, v := range stopped {
		if check(v) {
			t.Errorf("cc_summarize_failed.txt F%d: the stopped summarize was confirmed", i+1)
		}
	}
	if !ccMenuOpen(stopped[1]) {
		t.Errorf("cc_summarize_failed.txt F2: the error screen must count as an open menu so that close sends its Escape")
	}
	done := ccEffectFor(entry, "Summarize up to here")
	for i, v := range fixtureFrames(t, "cc_summarize_done.txt") {
		if got := done(v); got != (i == 1) {
			t.Errorf("cc_summarize_done.txt F%d: ccEffectFor = %v, want %v", i+1, got, i == 1)
		}
	}
}
