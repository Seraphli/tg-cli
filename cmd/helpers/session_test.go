package helpers

import "testing"

func TestIsCodexBusyTitle(t *testing.T) {
	// Codex busy: title starts with a braille spinner frame (U+2800–U+28FF).
	busy := []string{
		"⠋ project", "⠙ project", "⠹ project", "⠸ project", "⠼ project",
		"⠴ project", "⠦ project", "⠧ project", "⠇ project", "⠏ project",
		"⠙ ca-fix-codex-i...", // truncated busy title
	}
	for _, title := range busy {
		if !IsCodexBusyTitle(title) {
			t.Errorf("IsCodexBusyTitle(%q) = false, want true (busy)", title)
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
		if IsCodexBusyTitle(title) {
			t.Errorf("IsCodexBusyTitle(%q) = true, want false (idle)", title)
		}
	}
}
