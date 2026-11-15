package handlers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	tele "gopkg.in/telebot.v3"
)

// TestExtractReplyTarget_TextPath: a notification's text carries a 📟 line, so the target is parsed from it
// (legacy notifications as delivered by Telegram, rich ones through RichReplyPoller).
func TestExtractReplyTarget_TextPath(t *testing.T) {
	replyTo := &tele.Message{ID: 100, Text: "🔴 Idle\n📟 %5\n📊 Context"}
	target, err := extractReplyTarget(replyTo)
	if err != nil {
		t.Fatalf("expected target from text, got err: %v", err)
	}
	if target == nil || target.PaneID != "%5" {
		t.Fatalf("expected PaneID %%5, got %+v", target)
	}
}

// TestExtractReplyTarget_EmptyText: a reply to a message without text (no 📟 line) has no target.
func TestExtractReplyTarget_EmptyText(t *testing.T) {
	if _, err := extractReplyTarget(&tele.Message{ID: 300}); err == nil {
		t.Fatal("expected error when the text is empty")
	}
}

// TestExtractReplyTarget_NilReply: a nil reply message yields an error.
func TestExtractReplyTarget_NilReply(t *testing.T) {
	if _, err := extractReplyTarget(nil); err == nil {
		t.Fatal("expected error for nil reply message")
	}
}

// quoteFile matches the name withQuote gives the file of a long quoted message: quote_, the label of the session
// the reply goes to and the local time to the second as YYYYMMDD-HHMMSS.
func quoteFile(label string) *regexp.Regexp {
	return regexp.MustCompile("^quote_" + regexp.QuoteMeta(label) + `_\d{8}-\d{6}\.txt$`)
}

// savedQuote checks that got is the quote line of a message written to a file in dir plus the user's text, that
// the file is named for label and the second of the write, and that the file holds the whole message; it returns
// the path of the file.
func savedQuote(t *testing.T, dir, label, got, quoted, text string) string {
	t.Helper()
	line, rest, _ := strings.Cut(got, "\n\n")
	path, ok := strings.CutPrefix(line, fmt.Sprintf("> [quoted message, %d characters: ", utf8.RuneCountInString(quoted)))
	path, ok2 := strings.CutSuffix(path, "]")
	if !ok || !ok2 || rest != text || strings.Contains(line, "\n") || filepath.Dir(path) != dir || !quoteFile(label).MatchString(filepath.Base(path)) {
		t.Fatalf("injected text = %.100q, want one quote line naming a quote_%s_<YYYYMMDD-HHMMSS>.txt file in %s, a blank line and %q", got, label, dir, text)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != quoted {
		t.Errorf("the quote file holds %d bytes (err %v), want the whole message of %d bytes", len(data), err, len(quoted))
	}
	return path
}

// TestWithQuote: what a group reply puts in front of the user's text. A short message is quoted inline; a longer
// one (a rich message holds up to 30000 runes) is written to a file named for the session and the second, and the
// single quote line names that file.
func TestWithQuote(t *testing.T) {
	dir := t.TempDir()
	if got, want := withQuote("hi", "line one\nline two", dir, "alpha"), "> line one\n> line two\n\nhi"; got != want {
		t.Errorf("short message quoted as %q, want %q", got, want)
	}
	long := strings.Repeat(strings.Repeat("é", 99)+"\n", 300)
	savedQuote(t, dir, "alpha", withQuote("hi", long, dir, "alpha"), long, "hi")
}
