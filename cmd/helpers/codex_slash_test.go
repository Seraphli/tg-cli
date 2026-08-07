package helpers

import "testing"

// TestIsCodexSlash covers the f29 C scope predicate, source-faithful to the codex 0.144.x slash grammar
// (prompt_args.rs parse_slash_name + slash_input.rs validate_submission): literal leading "/" (no trim),
// a non-empty command name, and no "/" inside the name. /image.jpg is predicate-TRUE (a legal
// single-slash root path); image injects are excluded at the GATE by the AltSnippet bypass — see
// TestCodexSlashGate.
func TestIsCodexSlash(t *testing.T) {
	cases := []struct {
		backend, text string
		want          bool
	}{
		{"codex", "/compact", true},
		{"codex", "/review src/main.go", true}, // "/" in an ARG is fine — only the command token matters
		{"codex", "/prompts:draftpr FILE=src/main.go", true},
		{"codex", "/image.jpg", true}, // single-slash root path passes the TEXT predicate; the gate excludes image injects
		{"codex", "", false},
		{"codex", "   ", false},         // whitespace-only
		{"codex", "/", false},           // bare "/" names no command (parse_slash_name → None)
		{"codex", "//foo", false},       // "/" inside the name
		{"codex", "/word/arg", false},   // "/" inside the name
		{"codex", "  /compact", false},  // space-led input is ordinary text in codex — no trimming
		{"codex", "/tmp/x.jpg", false},  // absolute file path: "/" inside the token (attempt-5 phase14 shape)
		{"codex", "hello", false},       // not a slash command
		{"codex", "not/a/command", false},
		{"cc", "/compact", false}, // not a codex backend
		{"", "/compact", false},
	}
	for _, c := range cases {
		if got := isCodexSlash(c.backend, c.text); got != c.want {
			t.Errorf("isCodexSlash(%q, %q) = %v, want %v", c.backend, c.text, got, c.want)
		}
	}
}

// TestCodexSlashGate covers the gate-level AltSnippet image bypass: an image inject (AltSnippet is set
// ONLY by the image-inject path, messages.go "[Image") must take the legacy paste path even when its
// pasted text is a single-slash root path that passes the text predicate — codex renders image pastes as
// an attachment chip, so the compose-confirm could never match and the transaction would always veto
// (the attempt-5 codex phase14 regression).
func TestCodexSlashGate(t *testing.T) {
	if codexSlashGate("codex", "/image.jpg", "[Image") {
		t.Error("image inject (AltSnippet set) with a single-slash root path must bypass the codex-slash transaction")
	}
	if codexSlashGate("codex", "/tmp/x.jpg", "[Image") {
		t.Error("image inject with a multi-slash path must bypass (predicate and AltSnippet both exclude it)")
	}
	if !codexSlashGate("codex", "/compact", "") {
		t.Error("a real slash command without AltSnippet must run the transaction")
	}
	if codexSlashGate("cc", "/compact", "") {
		t.Error("a non-codex backend must not run the transaction")
	}
}

// TestCodexComposerHasText covers the compose-confirm predicate: the snippet must appear AFTER a "›"
// codex composer prompt char, matched bottom-up (the live composer line nearest the bottom).
func TestCodexComposerHasText(t *testing.T) {
	if !codexComposerHasText("some output\n› /compact", "/compact") {
		t.Error("must match a snippet after the › composer prompt")
	}
	if !codexComposerHasText("› \nmore\n› /compact now", "/compact") {
		t.Error("must scan bottom-up and match the live composer line")
	}
	if codexComposerHasText("just plain output\n/compact somewhere", "/compact") {
		t.Error("must NOT match without a › composer prompt line")
	}
	if codexComposerHasText("› something else", "/compact") {
		t.Error("must NOT match when the composer lacks the snippet")
	}
	if codexComposerHasText("›   ", "/compact") {
		t.Error("an empty composer must NOT match")
	}
}
