package helpers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tele "gopkg.in/telebot.v3"
)

// richFallbackServer returns a fake Telegram that 400s any rich request (rich_message present) and, for
// the legacy fallback, captures the "text" field. *legacyText is the text the legacy path sent.
func richFallbackServer(t *testing.T, legacyText *string, okMsgID int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		json.Unmarshal(body, &payload)
		w.Header().Set("Content-Type", "application/json")
		if _, isRich := payload["rich_message"]; isRich {
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse rich message: unexpected end tag"}`)
			return
		}
		if raw, ok := payload["text"]; ok {
			json.Unmarshal(raw, legacyText)
		}
		fmt.Fprint(w, okResponse(okMsgID))
	}))
}

// notificationHeader is a rich header that CollapseSessionMeta wraps in <details> and RichifyNewlines
// turns into <br> — so the transformed rich html carries <details>/<br>, but originalHTML must not.
const notificationHeader = "✅ Task Completed\n📂 /path\n📟 %1@s\n🖥 claude"

// E1b (EDIT — the reachable path): RetryFreezeEditRich delegates to RetryEditRich with NO LegacyHTML.
// On a rich 400 the legacy edit must use the PRE-transform html (no <details>, no <br>), else
// parse_mode=HTML would reject it and the fallback 400s too.
func TestRetryEditRich_EmptyLegacy_FallbackUsesOriginalHTML(t *testing.T) {
	var legacyText string
	srv := richFallbackServer(t, &legacyText, 99)
	defer srv.Close()
	bot := makeFakeBot(t, srv)
	msg := &tele.Message{ID: 10, Chat: &tele.Chat{ID: 1}}
	markup := &tele.ReplyMarkup{InlineKeyboard: [][]tele.InlineButton{{{Text: "OK", Data: "ok"}}}}

	if _, err := RetryFreezeEditRich(bot, msg, notificationHeader, markup); err != nil {
		t.Fatalf("RetryFreezeEditRich returned error: %v", err)
	}
	if strings.Contains(legacyText, "<details>") || strings.Contains(legacyText, "<br>") {
		t.Errorf("legacy edit fallback contains <details>/<br> (invalid for parse_mode=HTML): %q", legacyText)
	}
	if legacyText != notificationHeader {
		t.Errorf("legacy edit fallback should be the original pre-transform html; got %q want %q", legacyText, notificationHeader)
	}
}

// E1b (SEND): same guarantee for RetrySendRich with empty LegacyHTML.
func TestRetrySendRich_EmptyLegacy_FallbackUsesOriginalHTML(t *testing.T) {
	var legacyText string
	srv := richFallbackServer(t, &legacyText, 88)
	defer srv.Close()
	bot := makeFakeBot(t, srv)

	if _, err := RetrySendRich(bot, &tele.Chat{ID: 1}, notificationHeader, RichSendOpts{}); err != nil {
		t.Fatalf("RetrySendRich returned error: %v", err)
	}
	if strings.Contains(legacyText, "<details>") || strings.Contains(legacyText, "<br>") {
		t.Errorf("legacy send fallback contains <details>/<br>: %q", legacyText)
	}
	if legacyText != notificationHeader {
		t.Errorf("legacy send fallback should be the original pre-transform html; got %q want %q", legacyText, notificationHeader)
	}
}
