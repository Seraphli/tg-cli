package helpers_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/handlers"
	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
	"github.com/Seraphli/tg-cli/internal/tgfake"
	tele "gopkg.in/telebot.v3"
)

// rwButton is one inline button of a recorded Bot API request.
type rwButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// TestRewindLiveBot runs the real bot handlers (handlers.Register) on a fake Telegram against the real cc in default
// permission mode. B: /rewind on an idle cc sends the status message and edits that same message into the list of three
// turns (oldest at the top, numbered 3, newest at the bottom, numbered 1, one button to a row, Cancel alone on the last row); a tap on the second entry button
// restores the conversation: the result is the edit of the tapped message itself (the restored text in a <pre> block), and the
// pane capture follows collapsed with the last 12 lines of the pane. A: /rewind while cc runs a tool is refused
// (the status message is edited into the refusal) and no key reaches the pane. HOME is a temp dir, so the bot config
// never touches ~/.tg-cli.
func TestRewindLiveBot(t *testing.T) {
	target := helpers.LiveStart(t, "cc", 20, "default")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	chat := &tele.Chat{ID: -1001234567890, Type: tele.ChatSuperGroup}
	for _, err := range []error{
		config.SaveAppConfig(config.AppConfig{}),
		config.SaveCredentials(config.Credentials{PairingAllow: config.PairingAllow{IDs: []string{fmt.Sprint(chat.ID)}}}),
		config.SaveRoutes(config.Routes{NameRouteMap: map[string]config.NameRoute{"rw": {ChatID: chat.ID, TopicID: 12}}}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	s := tgfake.New(t)
	bot := s.Bot(t)
	bs := &types.BotState{Bot: bot, Pages: stores.NewPageCacheStore(), SessionState: stores.NewSessionStateStore(t.TempDir()),
		PendingWait: stores.NewPendingWaitStore(), HookRunning: stores.NewHookRunningStateStore(),
		PendingMsgStore: stores.NewPendingMsgStore(), CommandStats: stores.NewCommandStatsStore(t.TempDir())}
	bs.SessionState.Add("sess-rw", injector.FormatTarget(target), "/work/rw", "")
	if ok, msg := bs.SessionState.SetName("sess-rw", "rw"); !ok {
		t.Fatalf("SetName: %s", msg)
	}
	handlers.Register(bs)
	// say types text in topic 12 as Telegram delivers it: ThreadID 12, the topic flag and ReplyTo the topic creation message
	say := func(id int, text string) {
		bot.ProcessUpdate(tele.Update{Message: &tele.Message{ID: id, Chat: chat, Sender: &tele.User{ID: 1}, Text: text, ThreadID: 12, TopicMessage: true,
			ReplyTo: &tele.Message{ID: 12, Chat: chat, ThreadID: 12, TopicMessage: true}}})
	}
	// tap presses the button with callback_data data; 9001 is the message id tgfake returns for every send, so the menu's id
	tap := func(data string) {
		bot.ProcessUpdate(tele.Update{Callback: &tele.Callback{ID: "cb", Sender: &tele.User{ID: 1}, Message: &tele.Message{ID: 9001, Chat: chat, ThreadID: 12}, Data: data}})
	}
	// rows returns the inline buttons, row by row, of the last request of method (telebot sends reply_markup as a JSON
	// string); a request without one gives no rows, which the assertions on the result report
	rows := func(method string) [][]rwButton {
		var raw string
		for _, c := range s.Calls() {
			if c.Method == method {
				raw, _ = c.Params["reply_markup"].(string)
			}
		}
		var markup struct {
			InlineKeyboard [][]rwButton `json:"inline_keyboard"`
		}
		json.Unmarshal([]byte(raw), &markup)
		return markup.InlineKeyboard
	}
	// scenario B: idle cc, three finished turns; the status message becomes the list; Restore conversation to the second entry
	for _, w := range []string{"alpha", "beta", "gamma"} {
		helpers.LiveTurn(t, target, "Reply with exactly: "+w, w)
	}
	say(20, "/rewind")
	var sends, edits []tgfake.Call
	for _, c := range s.Calls() {
		switch c.Method {
		case "sendMessage":
			sends = append(sends, c)
		case "editMessageText":
			edits = append(edits, c)
		}
	}
	if len(sends) != 1 || sends[0].Params["text"] != "⏳ Reading the rewind menu…" || len(edits) != 1 || fmt.Sprint(edits[0].Params["message_id"]) != "9001" {
		helpers.LiveFail(t, target, "after /rewind: sendMessage requests %v, editMessageText requests %v; want the status message sent once and then edited into the list", sends, edits)
	}
	list := rows("editMessageText")
	for _, row := range list {
		if len(row) != 1 {
			helpers.LiveFail(t, target, "a keyboard row holds %d buttons, want exactly 1: %+v", len(row), list)
		}
	}
	if len(list) < 2 || list[len(list)-1][0].Text != "❌ Cancel" {
		helpers.LiveFail(t, target, "list keyboard %+v: want the entry rows and ❌ Cancel alone on the last row", list)
	}
	entries := slices.Concat(list[:len(list)-1]...)
	if len(entries) != 3 {
		helpers.LiveFail(t, target, "entry buttons %+v, want three (one per turn)", entries)
	}
	// numbers start at the newest message, so the entry button directly above the Cancel row (the last one) must be number 1 and
	// the body line with that number must be the newest turn
	newest := entries[len(entries)-1]
	if body, _ := edits[0].Params["text"].(string); !strings.HasPrefix(newest.Text, "1. ") || !strings.Contains(body, "\n1. Reply with exactly: gamma") {
		helpers.LiveFail(t, target, "the entry button above the Cancel row %q is not number 1 for the newest message (gamma); list text %q", newest.Text, body)
	}
	tap(entries[1].Data)
	actions := slices.Concat(rows("editMessageText")...)
	i := slices.IndexFunc(actions, func(b rwButton) bool { return strings.HasSuffix(b.Text, "Restore conversation") })
	if i < 0 {
		helpers.LiveFail(t, target, "no Restore conversation button after the pick: %+v", actions)
	}
	tap(actions[i].Data) // the bot is synchronous: the tap returns when the drive, the edit and the capture post are done
	// the result is the edit of the tapped message itself (HTML), not a post: the status message stays the only sendMessage
	const done = "✅ Rewound: Restore conversation\n↩️ Your message (tap to copy):\n<pre>Reply with exactly: beta</pre>"
	nSend, lastEdit, capture := 0, tgfake.Call{}, tgfake.Call{}
	for _, c := range s.Calls() {
		switch c.Method {
		case "sendMessage":
			nSend++
		case "editMessageText":
			lastEdit = c
		case "sendRichMessage":
			capture = c
		}
	}
	if nSend != 1 || lastEdit.Params["text"] != done || lastEdit.Params["parse_mode"] != "HTML" {
		helpers.LiveFail(t, target, "after the action tap: %d sendMessage requests (want 1, the status message), last edit %v (want the text %q with parse_mode HTML)", nSend, lastEdit, done)
	}
	// the pane follows as a collapsed capture: the header, then the last 12 lines of the pane (separators shortened, trailing blank lines dropped, HTML-escaped), and the buttons Expand and Delete
	pane, _ := injector.CapturePane(target)
	paneLines := strings.Split(strings.TrimRight(helpers.ShortenSeparators(pane), " \t\r\n"), "\n")
	if len(paneLines) <= 12 {
		helpers.LiveFail(t, target, "the pane holds %d lines, want more than 12 so that the last 12 differ from the first 12", len(paneLines))
	}
	for i := range paneLines {
		paneLines[i] = markdown.EscapeHTML(paneLines[i])
	}
	wantHTML := "📺 Pane Capture · last 12 lines<br>" + strings.Join(paneLines[len(paneLines)-12:], "<br>")
	rm, _ := capture.Params["rich_message"].(map[string]interface{})
	if html, _ := rm["html"].(string); html != wantHTML {
		helpers.LiveFail(t, target, "capture html %q, want the collapsed view %q", html, wantHTML)
	}
	var buttons []string
	if markup, ok := capture.Params["reply_markup"].(map[string]interface{}); ok {
		for _, row := range markup["inline_keyboard"].([]interface{}) {
			for _, b := range row.([]interface{}) {
				buttons = append(buttons, b.(map[string]interface{})["text"].(string))
			}
		}
	}
	if !slices.Equal(buttons, []string{"📖 Expand", "🗑 Delete"}) {
		helpers.LiveFail(t, target, "capture buttons %v, want [📖 Expand 🗑 Delete]", buttons)
	}
	if v := helpers.ViewportNow(target); helpers.HasReplyLine(v, "beta") || helpers.HasReplyLine(v, "gamma") {
		helpers.LiveFail(t, target, "the viewport still shows the beta or the gamma reply")
	}
	if text, ok := helpers.CCBoxNow(target); !ok || text != "" {
		helpers.LiveFail(t, target, "the input box is %q (box found: %v), want it empty", text, ok)
	}
	// scenario A: cc runs a tool; /rewind is refused before any key reaches the pane
	if err := injector.InjectText(target, `Run this command in the foreground with the Bash tool and wait for it: python3 -c "import time; time.sleep(45)"`); err != nil {
		t.Fatal(err)
	}
	helpers.LiveWait(t, target, 60*time.Second, func(v string) bool { return strings.Contains(v, "ctrl+b to run in background") })
	say(21, "/rewind")
	const busy = "⏳ The agent is busy. Press /t to interrupt it, then run /rewind again."
	calls := s.Calls()
	if last := calls[len(calls)-1]; last.Method != "editMessageText" || last.Params["text"] != busy {
		helpers.LiveFail(t, target, "last request after /rewind on a busy cc: %v, want an editMessageText with %q (the status message edited into the refusal)", last, busy)
	}
	text, _ := helpers.CCBoxNow(target)
	if strings.Contains(helpers.ViewportNow(target), "Enter to continue · Esc to cancel") || strings.Contains(text, "/rewind") {
		helpers.LiveFail(t, target, "a key reached the busy cc: the rewind menu is open or /rewind is in the box %q", text)
	}
	// the 45 s command must not outlive the test
	helpers.InterruptIfBusy(bs.Bot, bs.PendingWait, bs.PendingMsgStore, bs.HookRunning, notify.FormatPaneID, target)
}
