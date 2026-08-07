package helpers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	tele "gopkg.in/telebot.v3"
)

// makeFakeBot creates a *tele.Bot in Offline mode pointing at srv.URL.
// bot.Raw calls: srv.URL + "/bot" + token + "/" + method.
// We set URL=srv.URL and Token="test", so the full path is srv.URL + "/bottest/" + method.
func makeFakeBot(t *testing.T, srv *httptest.Server) *tele.Bot {
	t.Helper()
	bot, err := tele.NewBot(tele.Settings{
		Token:   "test",
		URL:     srv.URL,
		Offline: true,
		Client:  srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return bot
}

// extractMethodFromPath extracts the Telegram method name from the request path.
// Path format: /botTOKEN/METHOD
func extractMethodFromPath(path string) string {
	// Remove the leading /botTOKEN/ prefix
	parts := strings.SplitN(path, "/", 3)
	if len(parts) == 3 {
		return parts[2]
	}
	return path
}

// okResponse returns a JSON body simulating a successful Telegram message response.
func okResponse(msgID int) string {
	return fmt.Sprintf(`{"ok":true,"result":{"message_id":%d,"chat":{"id":1}}}`, msgID)
}

// TestRetrySendRich_Payload verifies TC2:
// - request path segment is "sendRichMessage"
// - payload JSON has rich_message.html == html
// - payload has chat_id
// - payload has reply_markup when markup is passed
// - returned *tele.Message has ID == 4242
func TestRetrySendRich_Payload(t *testing.T) {
	var capturedBody []byte
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, okResponse(4242))
	}))
	defer srv.Close()

	bot := makeFakeBot(t, srv)
	chat := &tele.Chat{ID: 999}
	markup := &tele.ReplyMarkup{
		InlineKeyboard: [][]tele.InlineButton{
			{{Text: "OK", Data: "ok"}},
		},
	}

	msg, err := RetrySendRich(bot, chat, "<b>hi</b>", RichSendOpts{Markup: markup})
	if err != nil {
		t.Fatalf("RetrySendRich returned error: %v", err)
	}

	// Assert method segment
	method := extractMethodFromPath(capturedPath)
	if method != "sendRichMessage" {
		t.Errorf("expected method sendRichMessage, got %q", method)
	}

	// Assert payload fields
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v (body=%q)", err, capturedBody)
	}
	if _, ok := payload["chat_id"]; !ok {
		t.Error("payload missing chat_id")
	}
	if _, ok := payload["reply_markup"]; !ok {
		t.Error("payload missing reply_markup")
	}

	// Assert rich_message.html
	var rm map[string]json.RawMessage
	if err := json.Unmarshal(payload["rich_message"], &rm); err != nil {
		t.Fatalf("unmarshal rich_message: %v", err)
	}
	var html string
	if err := json.Unmarshal(rm["html"], &html); err != nil {
		t.Fatalf("unmarshal rich_message.html: %v", err)
	}
	if html != "<b>hi</b>" {
		t.Errorf("rich_message.html = %q, want %q", html, "<b>hi</b>")
	}

	// Assert decoded message ID
	if msg == nil {
		t.Fatal("RetrySendRich returned nil message")
	}
	if msg.ID != 4242 {
		t.Errorf("msg.ID = %d, want 4242", msg.ID)
	}
}

// TestRetryEditRich_Payload verifies TC2 for the edit path:
// - request path segment is "editMessageText"
// - payload JSON has rich_message
func TestRetryEditRich_Payload(t *testing.T) {
	var capturedPath string
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, okResponse(100))
	}))
	defer srv.Close()

	bot := makeFakeBot(t, srv)
	editMsg := tele.StoredMessage{MessageID: "77", ChatID: 1}

	msg, err := RetryEditRich(bot, editMsg, "<i>edit</i>", RichSendOpts{})
	if err != nil {
		t.Fatalf("RetryEditRich returned error: %v", err)
	}

	method := extractMethodFromPath(capturedPath)
	if method != "editMessageText" {
		t.Errorf("expected method editMessageText, got %q", method)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := payload["rich_message"]; !ok {
		t.Error("editMessageText payload missing rich_message")
	}

	if msg == nil {
		t.Fatal("RetryEditRich returned nil message")
	}
	if msg.ID != 100 {
		t.Errorf("msg.ID = %d, want 100", msg.ID)
	}
}

// TestRetrySendRich_NoMarkup verifies that when no markup is passed, reply_markup is absent.
func TestRetrySendRich_NoMarkup(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, okResponse(1))
	}))
	defer srv.Close()

	bot := makeFakeBot(t, srv)
	chat := &tele.Chat{ID: 1}
	_, err := RetrySendRich(bot, chat, "<b>x</b>", RichSendOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := payload["reply_markup"]; ok {
		t.Error("reply_markup should be absent when no markup is passed")
	}
}

// TestRetrySendRich_G2Fallback verifies TC3:
// - server returns 400 for sendRichMessage, 200 for sendMessage
// - RetrySendRich retries exactly ONCE via legacy sendMessage (not in a loop)
// - returns the legacy message (ID 7)
// - RICH_FALLBACK marker appears in logger output (stderr)
func TestRetrySendRich_G2Fallback(t *testing.T) {
	var requestCount atomic.Int32
	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		paths = append(paths, r.URL.Path)
		method := extractMethodFromPath(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if method == "sendRichMessage" {
			// Return a known 400 error that produces a *tele.Error (ErrTooLongMessage)
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`)
		} else {
			// sendMessage succeeds
			fmt.Fprint(w, okResponse(7))
		}
	}))
	defer srv.Close()

	bot := makeFakeBot(t, srv)
	chat := &tele.Chat{ID: 1}

	// Capture stderr to check for RICH_FALLBACK marker
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	msg, err := RetrySendRich(bot, chat, "<b>hi</b>", RichSendOpts{LegacyHTML: "<b>hi</b>"})

	w.Close()
	os.Stderr = origStderr
	stderrBytes, _ := io.ReadAll(r)
	stderrOutput := string(stderrBytes)

	if err != nil {
		t.Fatalf("RetrySendRich G2 fallback returned error: %v", err)
	}

	// Assert: exactly 2 requests total (1 sendRichMessage + 1 sendMessage)
	count := int(requestCount.Load())
	if count != 2 {
		t.Errorf("expected exactly 2 requests (sendRichMessage + sendMessage), got %d; paths=%v", count, paths)
	}

	// Assert first request was sendRichMessage, second was sendMessage
	if len(paths) >= 2 {
		m0 := extractMethodFromPath(paths[0])
		m1 := extractMethodFromPath(paths[1])
		if m0 != "sendRichMessage" {
			t.Errorf("first request must be sendRichMessage, got %q", m0)
		}
		if m1 != "sendMessage" {
			t.Errorf("second request (fallback) must be sendMessage, got %q", m1)
		}
	}

	// Assert returned message ID is from the legacy path
	if msg == nil {
		t.Fatal("G2 fallback returned nil message")
	}
	if msg.ID != 7 {
		t.Errorf("G2 fallback msg.ID = %d, want 7", msg.ID)
	}

	// Assert RICH_FALLBACK marker was logged
	if !strings.Contains(stderrOutput, "RICH_FALLBACK") {
		t.Errorf("expected RICH_FALLBACK in stderr output, got: %q", stderrOutput)
	}
}

// TestRetrySendRich_G2Fallback_UnknownDescription verifies TC3 for the realistic rich-rejection case:
// telebot wraps novel (unknown) error descriptions as fmt.Errorf("telegram: %s (%d)") — NOT *tele.Error —
// so isTelegram400 must also match by the "(400)" suffix to trigger the G2 fallback.
func TestRetrySendRich_G2Fallback_UnknownDescription(t *testing.T) {
	var requestCount atomic.Int32
	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		paths = append(paths, r.URL.Path)
		method := extractMethodFromPath(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if method == "sendRichMessage" {
			// Unknown description — telebot does NOT have this in its errors map, so it returns
			// fmt.Errorf("telegram: %s (%d)", description, code) rather than a *tele.Error.
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse rich message: unexpected end tag"}`)
		} else {
			// Legacy sendMessage succeeds
			fmt.Fprint(w, okResponse(77))
		}
	}))
	defer srv.Close()

	bot := makeFakeBot(t, srv)
	chat := &tele.Chat{ID: 1}

	// Capture stderr to check for RICH_FALLBACK marker
	origStderr := os.Stderr
	pr, pw, _ := os.Pipe()
	os.Stderr = pw

	msg, err := RetrySendRich(bot, chat, "<b>x</b>", RichSendOpts{LegacyHTML: "<b>x</b>"})

	pw.Close()
	os.Stderr = origStderr
	stderrBytes, _ := io.ReadAll(pr)
	stderrOutput := string(stderrBytes)

	if err != nil {
		t.Fatalf("RetrySendRich G2 fallback (unknown desc) returned error: %v", err)
	}

	// Assert: exactly 2 requests total (1 sendRichMessage + 1 sendMessage, no loop)
	count := int(requestCount.Load())
	if count != 2 {
		t.Errorf("expected exactly 2 requests (sendRichMessage + sendMessage), got %d; paths=%v", count, paths)
	}

	// Assert order: sendRichMessage first, sendMessage second
	if len(paths) >= 2 {
		m0 := extractMethodFromPath(paths[0])
		m1 := extractMethodFromPath(paths[1])
		if m0 != "sendRichMessage" {
			t.Errorf("first request must be sendRichMessage, got %q", m0)
		}
		if m1 != "sendMessage" {
			t.Errorf("second request (fallback) must be sendMessage, got %q", m1)
		}
	}

	// Assert returned message is from the legacy path (ID 77)
	if msg == nil {
		t.Fatal("G2 fallback (unknown desc) returned nil message")
	}
	if msg.ID != 77 {
		t.Errorf("G2 fallback (unknown desc) msg.ID = %d, want 77", msg.ID)
	}

	// Assert RICH_FALLBACK marker was logged
	if !strings.Contains(stderrOutput, "RICH_FALLBACK") {
		t.Errorf("expected RICH_FALLBACK in stderr output, got: %q", stderrOutput)
	}
}

// TestRetrySendRich_NewlineToBr_LegacyUntouched verifies the multi-line rich fix: the rich payload's
// html has bare "\n" converted to <br> (rich HTML would otherwise collapse it to a space), while a
// G2 legacy fallback keeps the original "\n" byte-for-byte (legacy parse_mode=HTML treats \n as a
// line break — G3).
func TestRetrySendRich_NewlineToBr_LegacyUntouched(t *testing.T) {
	var richHTML, legacyText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := extractMethodFromPath(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if method == "sendRichMessage" {
			var p struct {
				RichMessage struct {
					HTML string `json:"html"`
				} `json:"rich_message"`
			}
			json.Unmarshal(body, &p)
			richHTML = p.RichMessage.HTML
			// Force the G2 legacy fallback.
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse rich message"}`)
			return
		}
		// Legacy sendMessage fallback — capture its text field.
		var p struct {
			Text string `json:"text"`
		}
		json.Unmarshal(body, &p)
		legacyText = p.Text
		fmt.Fprint(w, okResponse(9))
	}))
	defer srv.Close()

	// Silence the RICH_FALLBACK stderr marker.
	origStderr := os.Stderr
	_, pw, _ := os.Pipe()
	os.Stderr = pw

	bot := makeFakeBot(t, srv)
	_, err := RetrySendRich(bot, &tele.Chat{ID: 1}, "alpha\nbeta\n<pre>code\nline2</pre>", RichSendOpts{LegacyHTML: "alpha\nbeta"})

	pw.Close()
	os.Stderr = origStderr
	if err != nil {
		t.Fatalf("RetrySendRich: %v", err)
	}

	// Rich payload: \n outside <pre> -> <br>; \n inside <pre> preserved.
	if !strings.Contains(richHTML, "alpha<br>beta<br>") {
		t.Errorf("rich html did not convert outside-<pre> newlines to <br>: %q", richHTML)
	}
	if !strings.Contains(richHTML, "<pre>code\nline2</pre>") {
		t.Errorf("rich html corrupted <pre> newlines: %q", richHTML)
	}
	// Legacy fallback text: byte-identical to the LegacyHTML arg (\n preserved, no <br>).
	if legacyText != "alpha\nbeta" {
		t.Errorf("legacy fallback text was altered (G3 violation): got %q want %q", legacyText, "alpha\nbeta")
	}
}

// TestRetryFreezeEditRich_ConvertsNewlines confirms advisor condition 1: the third wrapper
// (RetryFreezeEditRich) also applies the newline->  <br> conversion (it delegates to RetryEditRich
// for non-empty html), so no rich path can bypass it.
func TestRetryFreezeEditRich_ConvertsNewlines(t *testing.T) {
	var richHTML string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			RichMessage struct {
				HTML string `json:"html"`
			} `json:"rich_message"`
		}
		json.Unmarshal(body, &p)
		richHTML = p.RichMessage.HTML
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, okResponse(3))
	}))
	defer srv.Close()

	bot := makeFakeBot(t, srv)
	editMsg := tele.StoredMessage{MessageID: "3", ChatID: 1}
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.Data("OK", "u", "d")))
	if _, err := RetryFreezeEditRich(bot, editMsg, "one\ntwo", markup); err != nil {
		t.Fatalf("RetryFreezeEditRich: %v", err)
	}
	if !strings.Contains(richHTML, "one<br>two") {
		t.Errorf("RetryFreezeEditRich did not convert newlines to <br>: %q", richHTML)
	}
}

// TestRetrySendRich_CallbackDataEncoding guards against a regression where bot.Raw (unlike
// bot.Send) does NOT run telebot's processButtons, so a rich inline button's callback_data would
// omit the "\f<unique>|<data>" prefix and clicks would never route to their handler. The rich
// wrapper must apply the encoding itself; here the serialized payload must carry "\fce|c".
func TestRetrySendRich_CallbackDataEncoding(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, okResponse(1))
	}))
	defer srv.Close()

	bot := makeFakeBot(t, srv)
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.Data("📗 Collapse", "ce", "c")))
	if _, err := RetrySendRich(bot, &tele.Chat{ID: 1}, "<b>x</b>", RichSendOpts{Markup: markup}); err != nil {
		t.Fatalf("RetrySendRich error: %v", err)
	}
	var payload struct {
		ReplyMarkup struct {
			InlineKeyboard [][]struct {
				CallbackData string `json:"callback_data"`
			} `json:"inline_keyboard"`
		} `json:"reply_markup"`
	}
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v (body=%q)", err, capturedBody)
	}
	if len(payload.ReplyMarkup.InlineKeyboard) == 0 || len(payload.ReplyMarkup.InlineKeyboard[0]) == 0 {
		t.Fatalf("no inline buttons in payload (body=%q)", capturedBody)
	}
	got := payload.ReplyMarkup.InlineKeyboard[0][0].CallbackData
	if got != "\fce|c" {
		t.Errorf("callback_data = %q, want %q (\\f-encoding missing → clicks would not route)", got, "\fce|c")
	}
}

// TestRetryFreezeEditAuto_MixedEra verifies TC10(c) — the G1 mixed-era edit dispatch:
//   - rich=true  → the edit is an editMessageText carrying rich_message (post-upgrade rich message)
//   - rich=false → the edit is an editMessageText with parse_mode=HTML and NO rich_message
//     (a pre-upgrade legacy message stays legacy when edited by the post-upgrade binary)
// A single message's format is thus preserved across an upgrade regardless of the running binary.
func TestRetryFreezeEditAuto_MixedEra(t *testing.T) {
	markup := &tele.ReplyMarkup{InlineKeyboard: [][]tele.InlineButton{{{Text: "OK", Data: "ok"}}}}

	cases := []struct {
		name        string
		rich        bool
		wantRichMsg bool // body must contain "rich_message"
		wantHTML    bool // body must contain parse_mode HTML
	}{
		{name: "rich message stays rich", rich: true, wantRichMsg: true, wantHTML: false},
		{name: "legacy message stays legacy", rich: false, wantRichMsg: false, wantHTML: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var capturedBody, capturedPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				capturedBody = string(b)
				capturedPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, okResponse(55))
			}))
			defer srv.Close()

			bot := makeFakeBot(t, srv)
			editMsg := tele.StoredMessage{MessageID: "55", ChatID: 1}
			if _, err := RetryFreezeEditAuto(bot, editMsg, tc.rich, "<b>frozen</b>", markup); err != nil {
				t.Fatalf("RetryFreezeEditAuto(rich=%v) error: %v", tc.rich, err)
			}
			if method := extractMethodFromPath(capturedPath); method != "editMessageText" {
				t.Errorf("expected editMessageText, got %q", method)
			}
			hasRichMsg := strings.Contains(capturedBody, "rich_message")
			if hasRichMsg != tc.wantRichMsg {
				t.Errorf("rich=%v: rich_message present=%v, want %v (body=%q)", tc.rich, hasRichMsg, tc.wantRichMsg, capturedBody)
			}
			hasHTML := strings.Contains(capturedBody, "HTML")
			if hasHTML != tc.wantHTML {
				t.Errorf("rich=%v: parse_mode HTML present=%v, want %v (body=%q)", tc.rich, hasHTML, tc.wantHTML, capturedBody)
			}
		})
	}
}

// TestRetryEditRich_G2Fallback_UnknownDescription verifies G2 fallback symmetry for the edit path
// with an unknown error description (plain fmt.Errorf from telebot, not *tele.Error).
// The stub distinguishes rich vs legacy by checking whether the request body contains "rich_message".
func TestRetryEditRich_G2Fallback_UnknownDescription(t *testing.T) {
	var requestCount atomic.Int32
	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		paths = append(paths, r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Rich editMessageText carries "rich_message" in its JSON body; the legacy
		// RetryEdit call does NOT — use that to distinguish the two code paths.
		if strings.Contains(string(body), "rich_message") {
			// Unknown description — same plain-error path as send
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse rich message: unexpected end tag"}`)
		} else {
			// Legacy editMessageText (without rich_message, called by RetryEdit) succeeds
			fmt.Fprint(w, okResponse(88))
		}
	}))
	defer srv.Close()

	bot := makeFakeBot(t, srv)
	editMsg := tele.StoredMessage{MessageID: "88", ChatID: 1}

	// Capture stderr to check for RICH_FALLBACK marker
	origStderr := os.Stderr
	pr, pw, _ := os.Pipe()
	os.Stderr = pw

	msg, err := RetryEditRich(bot, editMsg, "<b>x</b>", RichSendOpts{LegacyHTML: "<b>x</b>"})

	pw.Close()
	os.Stderr = origStderr
	stderrBytes, _ := io.ReadAll(pr)
	stderrOutput := string(stderrBytes)

	if err != nil {
		t.Fatalf("RetryEditRich G2 fallback (unknown desc) returned error: %v", err)
	}

	// Assert: exactly 2 requests (1 editMessageText rich + 1 editMessageText legacy)
	count := int(requestCount.Load())
	if count != 2 {
		t.Errorf("expected exactly 2 requests, got %d; paths=%v", count, paths)
	}

	// Assert returned message is from the legacy path (ID 88)
	if msg == nil {
		t.Fatal("RetryEditRich G2 fallback (unknown desc) returned nil message")
	}
	if msg.ID != 88 {
		t.Errorf("RetryEditRich G2 fallback (unknown desc) msg.ID = %d, want 88", msg.ID)
	}

	// Assert RICH_FALLBACK marker was logged
	if !strings.Contains(stderrOutput, "RICH_FALLBACK") {
		t.Errorf("expected RICH_FALLBACK in stderr output, got: %q", stderrOutput)
	}
}
