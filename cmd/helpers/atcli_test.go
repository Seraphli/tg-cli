package helpers

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtReplyCommandDefault(t *testing.T) {
	home, _ := os.UserHomeDir()
	want := `tg-cli session at reply e2e-at-b e2e-cli --text "your message"`
	if got := AtReplyCommand(filepath.Join(home, ".tg-cli"), 12500, "e2e-at-b", "e2e-cli"); got != want {
		t.Fatalf("default reply: got %q want %q", got, want)
	}
	if got := AtReplyCommand("", 12500, "e2e-at-b", "e2e-cli"); got != want {
		t.Fatalf("empty reply: got %q want %q", got, want)
	}
}

func TestAtEndCommandDefault(t *testing.T) {
	home, _ := os.UserHomeDir()
	want := `tg-cli session at end e2e-at-b e2e-cli`
	if got := AtEndCommand(filepath.Join(home, ".tg-cli"), 12500, "e2e-at-b", "e2e-cli"); got != want {
		t.Fatalf("default end: got %q want %q", got, want)
	}
	if got := AtEndCommand("", 12500, "e2e-at-b", "e2e-cli"); got != want {
		t.Fatalf("empty end: got %q want %q", got, want)
	}
}

func TestAtReplyCommandNonDefault(t *testing.T) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".tg-cli-test")
	want := "tg-cli --config-dir " + dir + ` session at reply e2e-at-b e2e-cli --port 12501 --text "your message"`
	if got := AtReplyCommand(dir, 12501, "e2e-at-b", "e2e-cli"); got != want {
		t.Fatalf("non-default reply: got %q want %q", got, want)
	}
}

func TestAtEndCommandNonDefault(t *testing.T) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".tg-cli-test")
	want := "tg-cli --config-dir " + dir + ` session at end e2e-at-b e2e-cli --port 12501`
	if got := AtEndCommand(dir, 12501, "e2e-at-b", "e2e-cli"); got != want {
		t.Fatalf("non-default end: got %q want %q", got, want)
	}
}
