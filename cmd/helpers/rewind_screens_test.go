package helpers

import (
	"os"
	"testing"
)

// A user who left a menu open at the terminal (pi /tree, pi "Summarize branch?" dialog, codex transcript overlay) and
// sends /rewind from Telegram must not get the keys of the open and clear sequences (C-u, BSpace, the pasted /tree,
// Escape Escape) typed into it: the box readers report no recognised box on those screens although their layout holds
// the rule lines or the composer row of an input box. The screens in testdata are the structure lines of real captures
// (pi 0.85.1, codex 0.160.1); the idle screens are the cases that must still be recognised.
func TestRewindMenuScreensAreNoBox(t *testing.T) {
	screen := func(name string) string {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, name := range []string{"pi_tree.txt", "pi_summary_dialog.txt"} {
		if text, ok := piBoxText(screen(name)); ok {
			t.Errorf("%s: piBoxText reports an editor with the text %q", name, text)
		}
	}
	if text, ok := piBoxText(screen("pi_idle.txt")); !ok || text != "" {
		t.Errorf("pi_idle.txt: piBoxText = %q, %v, want an empty recognised editor", text, ok)
	}
	if text, _, ok := codexComposer(screen("codex_overlay.txt")); ok {
		t.Errorf("codex_overlay.txt: codexComposer reports a composer with the text %q", text)
	}
	if _, placeholder, ok := codexComposer(screen("codex_idle.txt")); !ok || !placeholder {
		t.Errorf("codex_idle.txt: codexComposer ok=%v placeholder=%v, want a recognised composer showing the placeholder", ok, placeholder)
	}
}

// cc draws the Rewind menu under a ─ rule row instead of the ▔ border in some panes (testdata/cc_rewind_rule_row.txt: the last 20 lines of the pane that failed TestRewindLiveBot in round 3, list level, cursor on "(current)"): the menu must read as open and its window as read, or the open step refuses a menu that is on screen and the close step sends no Escape.
func TestRewindCCMenuUnderRuleRow(t *testing.T) {
	b, err := os.ReadFile("testdata/cc_rewind_rule_row.txt")
	if err != nil {
		t.Fatal(err)
	}
	v := string(b)
	w := parseCCWindow(v)
	sel, _, found := w.selected()
	if !ccMenuOpen(v) || !w.Above || w.Below || len(w.Rows) != 2 || w.Rows[0].Entry != (RewindEntry{Label: "Reply with exactly: gamma", Annotation: "No code changes"}) || w.Rows[0].Selected || !found || !sel.Current {
		t.Errorf("ccMenuOpen = %v, window %+v, want an open menu whose window has \"↑ more\", the entry gamma and the selected row (current)", ccMenuOpen(v), w)
	}
}
