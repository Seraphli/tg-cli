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
		{"codex", "   ", false},        // whitespace-only
		{"codex", "/", false},          // bare "/" names no command (parse_slash_name → None)
		{"codex", "//foo", false},      // "/" inside the name
		{"codex", "/word/arg", false},  // "/" inside the name
		{"codex", "  /compact", false}, // space-led input is ordinary text in codex — no trimming
		{"codex", "/tmp/x.jpg", false}, // absolute file path: "/" inside the token (attempt-5 phase14 shape)
		{"codex", "hello", false},      // not a slash command
		{"codex", "not/a/command", false},
		{"cc", "/compact", false}, // not a codex backend
		{"", "/compact", false},
	}
	for _, c := range cases {
		if got := isLocalSlash(c.backend, c.text); got != c.want {
			t.Errorf("isLocalSlash(%q, %q) = %v, want %v", c.backend, c.text, got, c.want)
		}
	}
}

// TestCodexSlashGate covers the gate-level AltSnippet image bypass: an image inject (AltSnippet is set
// ONLY by the image-inject path, messages.go "[Image") must take the legacy paste path even when its
// pasted text is a single-slash root path that passes the text predicate — codex renders image pastes as
// an attachment chip, so the compose-confirm could never match and the transaction would always veto
// (the attempt-5 codex phase14 regression).
func TestCodexSlashGate(t *testing.T) {
	if localSlashGate("codex", "/image.jpg", "[Image") {
		t.Error("image inject (AltSnippet set) with a single-slash root path must bypass the codex-slash transaction")
	}
	if localSlashGate("codex", "/tmp/x.jpg", "[Image") {
		t.Error("image inject with a multi-slash path must bypass (predicate and AltSnippet both exclude it)")
	}
	if !localSlashGate("codex", "/compact", "") {
		t.Error("a real slash command without AltSnippet must run the transaction")
	}
	if localSlashGate("cc", "/compact", "") {
		t.Error("a non-codex backend must not run the transaction")
	}
}

// TestIsLocalSlashPi covers the pi admission added to the generalised predicate: pi built-in slash commands
// are TRUE (same text grammar as codex — pi intercepts them before prompt(), so no UserPromptSubmit), while
// cc stays FALSE. The codex cases live in TestIsCodexSlash and are byte-identical to before the rename.
func TestIsLocalSlashPi(t *testing.T) {
	cases := []struct {
		backend, text string
		want          bool
	}{
		{"pi", "/compact", true},
		{"pi", "/hotkeys", true},
		{"pi", "/model gpt", true},  // "/" only in the command token matters; an arg is fine
		{"pi", "/", false},          // bare "/" names no command
		{"pi", "//foo", false},      // "/" inside the name
		{"pi", "/tmp/x.jpg", false}, // absolute file path: "/" inside the token
		{"pi", "  /compact", false}, // space-led input is ordinary text
		{"pi", "hello", false},      // not a slash command
		{"cc", "/compact", false},   // cc is not a local-slash backend
	}
	for _, c := range cases {
		if got := isLocalSlash(c.backend, c.text); got != c.want {
			t.Errorf("isLocalSlash(%q, %q) = %v, want %v", c.backend, c.text, got, c.want)
		}
	}
	// The gate admits pi too (no AltSnippet), and still bypasses image injects.
	if !localSlashGate("pi", "/compact", "") {
		t.Error("a pi built-in slash without AltSnippet must run the transaction")
	}
	if localSlashGate("pi", "/image.jpg", "[Image") {
		t.Error("a pi image inject (AltSnippet set) must bypass the local-slash transaction")
	}
}

// TestPiComposerHasText covers pi's glyph-less composer predicate: the snippet must appear between the LAST
// TWO full-width "─" rule lines (bottom-most bordered region), matched bottom-up. Crucially it must pick the
// COMPOSER rules even when a DECOY rule pair (e.g. a prior /hotkeys box or a bash-execution box) sits above,
// and must VETO (false) when fewer than two rules exist.
func TestPiComposerHasText(t *testing.T) {
	rule := "────────────────────"
	// composer holds the snippet, no decoy.
	if !piComposerHasText("assistant text\n"+rule+"\n/compact\n"+rule+"\n~/proj (main)\n↑1k ↓2k", "/compact") {
		t.Error("must match a snippet in the composer between the last two rules")
	}
	// DECOY rule pair ABOVE the composer (a prior /hotkeys box); the composer (last two rules) holds the
	// snippet — must still match, proving bottom-anchoring (last-two, not any-two).
	decoy := rule + "\nHotkeys: ctrl-c quit\n" + rule
	if !piComposerHasText("output\n"+decoy+"\nmore output\n"+rule+"\n/hotkeys\n"+rule+"\n~/proj (main)\n↑1k", "/hotkeys") {
		t.Error("must bottom-anchor to the composer's rule pair, not a decoy pair above it")
	}
	// The snippet is in the DECOY (above), NOT in the composer → must NOT match (any-two-rules would wrongly match).
	if piComposerHasText("output\n"+rule+"\n/hotkeys\n"+rule+"\nmore\n"+rule+"\n\n"+rule+"\n~/proj (main)", "/hotkeys") {
		t.Error("must NOT match a snippet that is above the composer (in a decoy pair)")
	}
	// Fewer than two rules → veto.
	if piComposerHasText("just one rule\n"+rule+"\n/compact", "/compact") {
		t.Error("fewer than two rules must veto (return false)")
	}
	if piComposerHasText("no rules at all\n/compact somewhere", "/compact") {
		t.Error("no rules must veto (return false)")
	}
	// Two rules but the composer between them lacks the snippet → false.
	if piComposerHasText("out\n"+rule+"\nsomething else\n"+rule+"\n~/proj", "/compact") {
		t.Error("must NOT match when the composer region lacks the snippet")
	}
}

// TestPiCaptureConfirms covers the pi general-path compose-confirm predicate: a pi inject confirms when
// EITHER the literal snippet (the pasted PATH — pi shows the path itself, e.g. the boss's ToAPI-reg.zip)
// OR the altSnippet ("[Image" chip, a CC rendering) appears in the composer (between the last two rules).
// The literal-path case is exactly why the helper ORs the snippet leg and does NOT rely on altSnippet alone
// (the MUTATION-RED demo: reducing the helper to altSnippet-only fails ONLY the literal-path case).
func TestPiCaptureConfirms(t *testing.T) {
	rule := "────────────────────"
	path := "/tmp/tg-cli/uploads/ToAPI-reg.zip"
	// The boss's exact case: pi renders the LITERAL PATH in the composer; altSnippet is the CC-ism "[Image".
	// Must confirm via the snippet leg (altSnippet-only would miss the path — that is the mutation-RED signal).
	if !piCaptureConfirms("assistant text\n"+rule+"\n"+path+"\n"+rule+"\n~/proj (main)\n↑1k", path, "[Image") {
		t.Error("must confirm when the composer holds the literal pasted path (snippet leg)")
	}
	// CC-style rendering: the composer holds an "[Image …]" chip; the path snippet is absent but altSnippet matches.
	if !piCaptureConfirms("out\n"+rule+"\n[Image #1 attached]\n"+rule+"\n~/proj", path, "[Image") {
		t.Error("must confirm when the composer holds the [Image chip (altSnippet leg)")
	}
	// Composer holds NEITHER the path nor the chip → must NOT confirm.
	if piCaptureConfirms("out\n"+rule+"\nsomething else entirely\n"+rule+"\n~/proj", path, "[Image") {
		t.Error("must NOT confirm when the composer holds neither snippet nor altSnippet")
	}
	// Empty altSnippet must not spuriously confirm (piComposerHasText(pane, "") matches every line — the
	// altSnippet != "" guard must hold); path is absent → false.
	if piCaptureConfirms("out\n"+rule+"\nsomething else\n"+rule+"\n~/proj", path, "") {
		t.Error("empty altSnippet must not spuriously confirm")
	}
	// Fewer than two rules → veto (inherits piComposerHasText).
	if piCaptureConfirms("just one rule\n"+rule+"\n"+path, path, "[Image") {
		t.Error("fewer than two rules must veto (return false)")
	}
	if piCaptureConfirms("no rules at all\n"+path, path, "[Image") {
		t.Error("no rules must veto (return false)")
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
