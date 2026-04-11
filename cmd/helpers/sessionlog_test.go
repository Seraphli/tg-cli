package helpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadLastAssistantText_NoTruncation verifies that ReadLastAssistantText
// returns the full assistant text without a fixed rune cap. Before the fix it
// took a maxLen parameter (hard-coded to 500 in cmd/hooks/register.go); after
// the refactor the signature is path-only and returns the complete text so
// SessionStart resume notifications can display the full recovered content.
func TestReadLastAssistantText_NoTruncation(t *testing.T) {
	// Build a >500-rune assistant message with a marker at the tail we can grep.
	longBody := strings.Repeat("abcdefghij", 80) + "|TAIL_MARKER|" // 800 + 13 = 813 runes
	entry := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + longBody + `"}]}}` + "\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(entry), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := ReadLastAssistantText(path)
	if n := len([]rune(got)); n < 800 {
		t.Errorf("expected full text (>=800 runes), got %d runes", n)
	}
	if !strings.Contains(got, "|TAIL_MARKER|") {
		t.Error("returned text missing |TAIL_MARKER|, indicating truncation")
	}
	if strings.HasSuffix(got, "...") {
		t.Error("returned text ends with '...', indicating TruncateStr was applied")
	}
}
