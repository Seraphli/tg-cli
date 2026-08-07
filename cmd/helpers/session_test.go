package helpers

import "testing"

// TestTitleIsBusyCodex is the retargeted TestIsCodexBusyTitle: the same codex spinner-frame /
// truncated-name cases, now routed through the single classifier TitleIsBusy("codex", ...).
func TestTitleIsBusyCodex(t *testing.T) {
	// Codex busy: title starts with a braille spinner frame (U+2800–U+28FF).
	busy := []string{
		"⠋ project", "⠙ project", "⠹ project", "⠸ project", "⠼ project",
		"⠴ project", "⠦ project", "⠧ project", "⠇ project", "⠏ project",
		"⠙ ca-fix-codex-i...", // truncated busy title
	}
	for _, title := range busy {
		if !TitleIsBusy("codex", title) {
			t.Errorf("TitleIsBusy(codex, %q) = false, want true (busy)", title)
		}
	}
	// Codex idle: no braille prefix (plain or truncated dir name, empty).
	idle := []string{
		"ca-fix-codex-idle-detection", // full dir name
		"ca-messagedisplay-not...",    // truncated idle title (the original bug case)
		"tg-cli",
		"OBNote",
		"",
	}
	for _, title := range idle {
		if TitleIsBusy("codex", title) {
			t.Errorf("TitleIsBusy(codex, %q) = true, want false (idle)", title)
		}
	}
}

// TestTitleIsBusy exercises the single busy classifier directly across cc/codex/unknown backends, empty
// and whitespace-only titles, and the leading/trailing-whitespace cases the trim exists for. Fails if
// TitleIsBusy stops trimming or a prefix rule regresses.
func TestTitleIsBusy(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		title   string
		want    bool
	}{
		{"cc idle", "cc", "✳ Job", false},
		{"cc busy", "cc", "· Working", true},
		{"codex busy braille", "codex", "⠙ project", true},
		{"codex idle name", "codex", "project", false},
		{"unknown backend never busy", "zsh", "· Working", false},
		{"empty string not busy", "cc", "", false},
		{"whitespace-only not busy (cc)", "cc", "   ", false},
		{"whitespace-only not busy (codex)", "codex", "   ", false},
		{"cc idle with surrounding whitespace", "cc", "  ✳ Job  ", false},
		{"codex busy with surrounding whitespace", "codex", "  ⠙ x  ", true},
	}
	for _, c := range cases {
		if got := TitleIsBusy(c.backend, c.title); got != c.want {
			t.Errorf("%s: TitleIsBusy(%q, %q) = %v, want %v", c.name, c.backend, c.title, got, c.want)
		}
	}
}
