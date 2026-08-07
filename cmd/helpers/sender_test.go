package helpers

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/internal/archive"
	tele "gopkg.in/telebot.v3"
)

// Fix 22: FinalizeRichHTML applies the exact send-time transforms (collapse header metadata into a
// <details> block, then convert body newlines to <br>) that the rich senders apply, so a logged body
// equals what was sent. Same function + same input on both the send and the log path → they match.
func TestFinalizeRichHTML(t *testing.T) {
	in := "💬 Update\n📂 /p\n📟 %1@s\n📊 Context: 9%\n\nStep 1\nStep 2"
	got := FinalizeRichHTML(in)
	if !strings.Contains(got, "<details><summary>📋 C:9%</summary>") {
		t.Errorf("header metadata should be collapsed into a <details> block; got=%q", got)
	}
	if !strings.Contains(got, "Step 1<br>Step 2") {
		t.Errorf("body newlines should be converted to <br>; got=%q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("no bare newlines should remain in the finalized rich HTML; got=%q", got)
	}
	if FinalizeRichHTML(in) != got {
		t.Errorf("FinalizeRichHTML must be deterministic for the same input")
	}
}

// Fix 21: network/transient errors must retry indefinitely; only Telegram 4xx business errors (other
// than 429) fail fast. isNonRetryable is the discriminator used by all four retry functions.
func TestIsNonRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"400 bad request", tele.NewError(400, "Bad Request"), true},
		{"403 forbidden", tele.NewError(403, "Forbidden"), true},
		{"404 not found", tele.NewError(404, "Not Found"), true},
		{"409 conflict", tele.NewError(409, "Conflict"), true},
		{"429 rate limit is retryable", tele.NewError(429, "Too Many Requests"), false},
		{"500 internal is retryable", tele.NewError(500, "Internal Server Error"), false},
		{"502 bad gateway is retryable", tele.NewError(502, "Bad Gateway"), false},
		{"503 unavailable is retryable", tele.NewError(503, "Service Unavailable"), false},
		{"wrapped 400 fmt error", fmt.Errorf("telegram: something bad (400)"), true},
		{"wrapped 502 fmt error is retryable", fmt.Errorf("telegram: Bad Gateway (502)"), false},
		{"transport error is retryable", fmt.Errorf("Post \"https://api.telegram.org\": dial tcp: connection refused"), false},
		{"json parse of gateway body is retryable", fmt.Errorf("telegram: invalid character '<' looking for beginning of value"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isNonRetryable(c.err); got != c.want {
				t.Errorf("isNonRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// Fix 21: exponential backoff (1,2,4,8,16s) capped at 30s, no attempt limit (plateaus at 30s).
func TestNetworkRetryBackoff(t *testing.T) {
	cases := map[int]time.Duration{
		1:   1 * time.Second,
		2:   2 * time.Second,
		3:   4 * time.Second,
		4:   8 * time.Second,
		5:   16 * time.Second,
		6:   30 * time.Second,
		10:  30 * time.Second,
		100: 30 * time.Second,
	}
	for attempt, want := range cases {
		if got := networkRetryBackoff(attempt); got != want {
			t.Errorf("networkRetryBackoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

// Fix 21: a nil error from bot.Raw does NOT mean success — telebot swallows non-JSON gateway bodies.
// responseOK must require an actual {"ok":true} body so 502 HTML pages trigger a retry, not a fake ok.
func TestResponseOK(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"genuine success message", `{"ok":true,"result":{"message_id":5}}`, true},
		{"genuine success true", `{"ok":true,"result":true}`, true},
		{"api error", `{"ok":false,"error_code":400,"description":"Bad Request"}`, false},
		{"html gateway 502 page", `<html><head><title>502 Bad Gateway</title></head></html>`, false},
		{"empty body", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := responseOK([]byte(c.data)); got != c.want {
				t.Errorf("responseOK(%q) = %v, want %v", c.data, got, c.want)
			}
		})
	}
}

func TestFreezeEditArgs(t *testing.T) {
	markup := &tele.ReplyMarkup{}
	t.Run("empty text returns markup-only", func(t *testing.T) {
		what, opts := freezeEditArgs("", markup)
		if _, ok := what.(*tele.ReplyMarkup); !ok {
			t.Fatalf("expected *tele.ReplyMarkup, got %T", what)
		}
		if what != markup {
			t.Fatal("expected the same markup pointer")
		}
		if opts != nil {
			t.Fatalf("expected nil opts for markup-only, got %v", opts)
		}
	})
	t.Run("non-empty text returns text with markup and HTML mode", func(t *testing.T) {
		what, opts := freezeEditArgs("hello", markup)
		s, ok := what.(string)
		if !ok {
			t.Fatalf("expected string, got %T", what)
		}
		if s != "hello" {
			t.Fatalf("expected hello, got %q", s)
		}
		if len(opts) != 2 {
			t.Fatalf("expected 2 opts, got %d", len(opts))
		}
		if opts[0] != markup {
			t.Fatal("expected markup as first opt")
		}
		if opts[1] != tele.ModeHTML {
			t.Fatalf("expected ModeHTML, got %v", opts[1])
		}
	})
}

func newTempArchive(t *testing.T) *archive.Archive {
	t.Helper()
	a, err := archive.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("archive.New: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// FinalizeRichHTMLWithID injects the 🆔 line for a positive id; id 0 (and the plain FinalizeRichHTML used
// by the logger call sites) must not.
func TestFinalizeRichHTMLWithID(t *testing.T) {
	in := "🟢 Idle\n📂 /p\n📊 Context: 5%\n\nbody"
	if got := FinalizeRichHTMLWithID(in, 12); !strings.Contains(got, "🆔 #12") {
		t.Errorf("expected 🆔 #12 in %q", got)
	}
	if got := FinalizeRichHTMLWithID(in, 0); strings.Contains(got, "🆔") {
		t.Errorf("id 0 should have no 🆔 line: %q", got)
	}
	if got := FinalizeRichHTML(in); strings.Contains(got, "🆔") {
		t.Errorf("FinalizeRichHTML should have no 🆔 line: %q", got)
	}
}

// FinalizeRichHTMLWithID fills the ArchiveIDPlaceholder with 🆔 #<id> (id>0), and removes it plus a
// leading newline when there is no id — for non-session headers (e.g. mailbox) to carry the archive ID.
func TestFinalizeRichHTMLWithID_Placeholder(t *testing.T) {
	in := "<details><summary>📬 From: a → b</summary>\n🔖 hex123\n" + ArchiveIDPlaceholder + "\n</details>\nSubject: x"
	withID := FinalizeRichHTMLWithID(in, 7)
	if !strings.Contains(withID, "🆔 #7") {
		t.Errorf("expected 🆔 #7 from placeholder, got %q", withID)
	}
	if strings.Contains(withID, ArchiveIDPlaceholder) {
		t.Errorf("placeholder must be replaced, got %q", withID)
	}
	noID := FinalizeRichHTMLWithID(in, 0)
	if strings.Contains(noID, ArchiveIDPlaceholder) || strings.Contains(noID, "🆔") {
		t.Errorf("id 0 must drop the placeholder line and add no 🆔, got %q", noID)
	}
	// The dropped placeholder must not leave a stray blank line inside the details block.
	if strings.Contains(noID, "🔖 hex123<br><br>") {
		t.Errorf("id 0 left a blank line where the placeholder was, got %q", noID)
	}
}

// resolveReservation: a non-zero reservedID is returned unchanged (never re-reserved, M1); a nil Archive
// or a skip condition (chatID==0, non-text) returns 0; and none of these leak a message row.
func TestResolveReservation(t *testing.T) {
	Archive = nil
	if got := resolveReservation(0, 123, true); got != 0 {
		t.Errorf("nil Archive resolveReservation = %d, want 0", got)
	}
	a := newTempArchive(t)
	Archive = a
	defer func() { Archive = nil }()
	if got := resolveReservation(7, 123, true); got != 7 {
		t.Errorf("resolveReservation(7,...) = %d, want 7 (unchanged, no re-reserve)", got)
	}
	if got := resolveReservation(0, 0, true); got != 0 {
		t.Errorf("chatID==0 should skip, got %d", got)
	}
	if got := resolveReservation(0, 123, false); got != 0 {
		t.Errorf("non-text payload should skip, got %d", got)
	}
	// None of the above reserved a row, so the first genuine reserve returns id 1.
	if id, err := a.ReserveSend(123); err != nil || id != 1 {
		t.Errorf("first genuine ReserveSend = (%d,%v), want (1,nil) — a skip/reuse leaked a row", id, err)
	}
}

// floodLogMsg: send form omits msg_id; edit form includes it; both carry FloodError and attempt.
func TestFloodLogMsg(t *testing.T) {
	wait := 3 * time.Second
	sendMsg := floodLogMsg("sendRichMessage", "123456", "", wait, 2)
	if !strings.Contains(sendMsg, "FloodError") {
		t.Errorf("send form missing FloodError: %q", sendMsg)
	}
	if !strings.Contains(sendMsg, "chat=") {
		t.Errorf("send form missing chat=: %q", sendMsg)
	}
	if strings.Contains(sendMsg, "msg_id=") {
		t.Errorf("send form must not contain msg_id=: %q", sendMsg)
	}
	if !strings.Contains(sendMsg, "attempt") {
		t.Errorf("send form missing attempt: %q", sendMsg)
	}
	editMsg := floodLogMsg("edit", "123456", "789", wait, 1)
	if !strings.Contains(editMsg, "FloodError") {
		t.Errorf("edit form missing FloodError: %q", editMsg)
	}
	if !strings.Contains(editMsg, "chat=") {
		t.Errorf("edit form missing chat=: %q", editMsg)
	}
	if !strings.Contains(editMsg, "msg_id=") {
		t.Errorf("edit form missing msg_id=: %q", editMsg)
	}
	if !strings.Contains(editMsg, "attempt") {
		t.Errorf("edit form missing attempt: %q", editMsg)
	}
}

// editArchiveID skips (returns 0, creates no row) for a nonnumeric message id, chatID==0, a nonpositive
// tg msg id, a markup-only payload, or a nil Archive; a valid edit mints the FIRST id (proving the skips
// created nothing).
func TestEditArchiveIDSkip(t *testing.T) {
	a := newTempArchive(t)
	Archive = a
	defer func() { Archive = nil }()
	cases := []struct {
		name   string
		msgID  string
		chatID int64
		isText bool
	}{
		{"nonnumeric msg id", "inline123", 55, true},
		{"zero chat id", "10", 0, true},
		{"zero tg msg id", "0", 55, true},
		{"negative tg msg id", "-4", 55, true},
		{"markup-only (non-text)", "10", 55, false},
	}
	for _, tc := range cases {
		if got := editArchiveID(tc.msgID, tc.chatID, tc.isText); got != 0 {
			t.Errorf("%s: editArchiveID = %d, want 0", tc.name, got)
		}
	}
	Archive = nil
	if got := editArchiveID("10", 55, true); got != 0 {
		t.Errorf("nil Archive editArchiveID = %d, want 0", got)
	}
	Archive = a
	if got := editArchiveID("999", 55, true); got != 1 {
		t.Errorf("valid editArchiveID = %d, want 1 — a skip leaked a row", got)
	}
}
