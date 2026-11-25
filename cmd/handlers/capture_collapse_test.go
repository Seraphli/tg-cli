package handlers

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/tgfake"
	tele "gopkg.in/telebot.v3"
)

// TestCaptureCollapse sends a pane capture of 30 lines whose last rows are blank expanded the way /p sends it, taps Collapse and
// Expand, and then sends a capture the way the rewind does: it arrives collapsed (the last 12 lines, blank rows dropped, escaped).
func TestCaptureCollapse(t *testing.T) {
	setupTestConfig(t, config.AppConfig{})
	chat := &tele.Chat{ID: -1001234567890, Type: tele.ChatSuperGroup}
	allowChat(t, chat.ID)
	s := tgfake.New(t)
	bot := s.Bot(t)
	bs := &types.BotState{Bot: bot, Pages: stores.NewPageCacheStore(), SessionState: stores.NewSessionStateStore(t.TempDir()), CommandStats: stores.NewCommandStatsStore(t.TempDir())}
	Register(bs)
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i+1)
	}
	lines[27] = "<b>build</b> & test"
	content := strings.Join(lines, "\n") + "\n   \n\n  "
	collapsed := strings.ReplaceAll(strings.Join([]string{"📺 Pane Capture · last 12 lines",
		"line 19", "line 20", "line 21", "line 22", "line 23", "line 24", "line 25", "line 26", "line 27",
		"&lt;b&gt;build&lt;/b&gt; &amp; test", "line 29", "line 30"}, "\n"), "\n", "<br>")
	// check compares the html (htmlOK, described by wantHTML) and the button texts of a recorded rich send or edit
	check := func(step string, c tgfake.Call, htmlOK func(string) bool, wantHTML string, wantButtons ...string) {
		t.Helper()
		rm, _ := c.Params["rich_message"].(map[string]interface{})
		html, _ := rm["html"].(string)
		var buttons []string
		markup, _ := c.Params["reply_markup"].(map[string]interface{})
		rows, _ := markup["inline_keyboard"].([]interface{})
		for _, row := range rows {
			for _, b := range row.([]interface{}) {
				buttons = append(buttons, b.(map[string]interface{})["text"].(string))
			}
		}
		if !htmlOK(html) || !slices.Equal(buttons, wantButtons) {
			t.Errorf("%s: html %q, buttons %q; want html %s, buttons %q", step, html, buttons, wantHTML, wantButtons)
		}
	}
	isCollapsed := func(html string) bool { return html == collapsed }
	sent, err := SendCaptureReply(bot, chat, 0, bs.Pages, content, false)
	if err != nil {
		t.Fatalf("SendCaptureReply (/p path): %v", err)
	}
	press(bot, chat, sent.ID, 0, "ce", "c")
	check("after Collapse", lastCall(t, s, "editMessageText"), isCollapsed, collapsed, "📖 Expand", "🗑 Delete")
	press(bot, chat, sent.ID, 0, "ce", "e")
	check("after Expand", lastCall(t, s, "editMessageText"), func(html string) bool {
		return strings.HasPrefix(html, "📺 Pane Capture<br>line 01<br>") && strings.Contains(html, "line 30")
	}, "the expanded capture (header, line 01 first, line 30 inside)", "📗 Collapse", "🗑 Delete")
	if _, err := SendCaptureReply(bot, chat, 0, bs.Pages, content, true); err != nil {
		t.Fatalf("SendCaptureReply (rewind path): %v", err)
	}
	check("rewind capture", lastCall(t, s, "sendRichMessage"), isCollapsed, collapsed, "📖 Expand", "🗑 Delete")
}
