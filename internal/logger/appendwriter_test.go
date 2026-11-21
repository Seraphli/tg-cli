package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookAppend appends the way cmd/hook.go hookLog does: a hook process opens bot.log with O_APPEND for one line.
func hookAppend(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAppendWriterKeepsHookLines: bot.log is written by the bot and by every hook process. A hook line must
// survive the bot's next write, and hook lines written while the bot is down count toward the rotation size.
func TestAppendWriterKeepsHookLines(t *testing.T) {
	t.Run("hook line between two bot writes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bot.log")
		w := newAppendWriter(path, 1<<20, 5)
		w.Write([]byte("bot-1\n"))
		hookAppend(t, path, "hook-1\n")
		w.Write([]byte("bot-2\n"))
		if got := readLog(t, path); got != "bot-1\nhook-1\nbot-2\n" {
			t.Fatalf("bot.log = %q, want the three lines in order", got)
		}
	})
	t.Run("hook lines fill the file while the bot is down", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bot.log")
		w := newAppendWriter(path, 64, 5)
		hooks := strings.Repeat("h", 59) + "\n"
		bot := strings.Repeat("b", 49) + "\n"
		hookAppend(t, path, hooks)
		w.Write([]byte(bot))
		backups, _ := filepath.Glob(filepath.Join(dir, "bot-*.log"))
		if len(backups) != 1 || readLog(t, backups[0]) != hooks {
			t.Fatalf("want one backup holding the hook lines, got %v", backups)
		}
		hookAppend(t, path, "hook-2\n")
		w.Write([]byte("bot-3\n"))
		if got := readLog(t, path); got != bot+"hook-2\nbot-3\n" {
			t.Fatalf("bot.log after the rotation = %q", got)
		}
	})
}
