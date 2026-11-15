package handlers

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/tgfake"
	tele "gopkg.in/telebot.v3"
)

var (
	forumChat = &tele.Chat{ID: -1001234567890, Type: tele.ChatSuperGroup, Title: "Test Forum"}
	plainChat = &tele.Chat{ID: -1009999000001, Type: tele.ChatSuperGroup, Title: "Plain Group"}
)

// forumBindEnv isolates HOME, scripts getChat/createForumTopic on a fake Bot API, seeds one named and one
// unnamed session and registers the real handlers.
func forumBindEnv(t *testing.T) (*tgfake.Server, *tele.Bot, *types.BotState) {
	t.Helper()
	setupTestConfig(t, config.AppConfig{})
	allowChat(t, forumChat.ID)
	s := tgfake.New(t)
	s.On("getChat", func(p map[string]interface{}) tgfake.Reply {
		switch p["chat_id"] {
		case "-1001234567890":
			return tgfake.Reply{Result: map[string]interface{}{"id": forumChat.ID, "type": "supergroup", "title": "Test Forum", "is_forum": true}}
		case "-1009999000001":
			return tgfake.Reply{Result: map[string]interface{}{"id": plainChat.ID, "type": "supergroup", "title": "Plain Group", "is_forum": false}}
		}
		return tgfake.Reply{Err: "Bad Request: chat not found"}
	})
	s.On("createForumTopic", func(p map[string]interface{}) tgfake.Reply {
		return tgfake.Reply{Result: map[string]interface{}{"message_thread_id": 77, "name": p["name"]}}
	})
	bot := s.Bot(t)
	bs := &types.BotState{Bot: bot, SessionState: stores.NewSessionStateStore(t.TempDir()), CommandStats: stores.NewCommandStatsStore(t.TempDir())}
	bs.SessionState.Add("sess-alpha-0001", "main:1.0", "/work/alpha", "")
	bs.SessionState.SetName("sess-alpha-0001", "alpha")
	bs.SessionState.Add("sess-unnamed-02", "main:2.0", "/work/other", "")
	Register(bs)
	return s, bot, bs
}

// press delivers an inline-button callback on message msgID of chat; data "" sends the bare "\f<unique>" form.
func press(bot *tele.Bot, chat *tele.Chat, msgID, threadID int, unique, data string) {
	payload := "\f" + unique
	if data != "" {
		payload += "|" + data
	}
	bot.ProcessUpdate(tele.Update{Callback: &tele.Callback{ID: "cb", Sender: &tele.User{ID: 1}, Message: &tele.Message{ID: msgID, Chat: chat, ThreadID: threadID}, Data: payload}})
}

// lastCall returns the last recorded request of method.
func lastCall(t *testing.T, s *tgfake.Server, method string) tgfake.Call {
	t.Helper()
	calls := s.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == method {
			return calls[i]
		}
	}
	t.Fatalf("no %s request recorded; methods=%v", method, s.Methods())
	return tgfake.Call{}
}

func callText(c tgfake.Call) string {
	text, _ := c.Params["text"].(string)
	return text
}

type bindButton struct{ Text, Data string }

// bindButtons returns the (text, callback_data) pairs of the bind_select buttons in the call's reply_markup
// (telebot sends reply_markup as a JSON string).
func bindButtons(t *testing.T, c tgfake.Call) []bindButton {
	t.Helper()
	raw, _ := c.Params["reply_markup"].(string)
	var markup struct {
		InlineKeyboard [][]struct {
			Text         string `json:"text"`
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(raw), &markup); err != nil {
		t.Fatalf("decode reply_markup %q: %v", raw, err)
	}
	var out []bindButton
	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			if strings.HasPrefix(b.CallbackData, "\fbind_select|") {
				out = append(out, bindButton{Text: b.Text, Data: b.CallbackData})
			}
		}
	}
	return out
}

// bindNumber returns N of the single bind button whose text contains `contains`.
func bindNumber(t *testing.T, c tgfake.Call, contains string) string {
	t.Helper()
	var found []string
	for _, b := range bindButtons(t, c) {
		if strings.Contains(b.Text, contains) {
			found = append(found, strings.TrimPrefix(b.Data, "\fbind_select|"))
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one bind button containing %q, got %d in %+v", contains, len(found), bindButtons(t, c))
	}
	return found[0]
}

// methodsDuring returns the Bot API methods requested while f ran.
func methodsDuring(s *tgfake.Server, f func()) []string {
	n := len(s.Methods())
	f()
	return s.Methods()[n:]
}

func loadCreds(t *testing.T) config.Credentials {
	t.Helper()
	creds, err := config.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	return creds
}

func wantRoute(t *testing.T, name string, want config.NameRoute) {
	t.Helper()
	if got := loadCreds(t).NameRouteMap[name]; got != want {
		t.Errorf("route %q = %+v, want %+v", name, got, want)
	}
}

func TestForumBind_CreatesTopicAndRecords(t *testing.T) {
	checkRecorded := func(t *testing.T, s *tgfake.Server) {
		t.Helper()
		if n := s.Count("createForumTopic"); n != 1 {
			t.Fatalf("createForumTopic count = %d, want 1", n)
		}
		p := lastCall(t, s, "createForumTopic").Params
		if p["name"] != "alpha" || p["chat_id"] != "-1001234567890" {
			t.Errorf("createForumTopic params = %v, want name=alpha chat_id=-1001234567890", p)
		}
		wantRoute(t, "alpha", config.NameRoute{ChatID: forumChat.ID, TopicID: 77, Title: "Test Forum"})
		if got := loadCreds(t).ForumTopics[config.ForumTopicKey(forumChat.ID, "alpha")]; got != 77 {
			t.Errorf("ForumTopics record = %d, want 77", got)
		}
	}
	t.Run("settings menu", func(t *testing.T) {
		s, bot, bs := forumBindEnv(t)
		// Pressed inside topic 5: the menu lists the named session alone and the session still gets its own topic.
		press(bot, forumChat, 2000, 5, "settings_bind", "")
		if btns := bindButtons(t, lastCall(t, s, "editMessageText")); len(btns) != 1 || !strings.HasPrefix(btns[0].Text, "🔗 1: alpha") {
			t.Errorf("bind buttons = %+v, want exactly one starting with %q", btns, "🔗 1: alpha")
		}
		bs.SettingsMenuMsgs.Store(2000, true)
		press(bot, forumChat, 2000, 5, "bind_select", "1")
		checkRecorded(t, s)
		want := "  alpha (main:1.0) → Test Forum (-1001234567890), topic=#77"
		text := callText(lastCall(t, s, "editMessageText"))
		if !slices.Contains(strings.Split(text, "\n"), want) {
			t.Errorf("last edit text = %q, want a line equal to %q", text, want)
		}
	})
	t.Run("bot_bind", func(t *testing.T) {
		s, bot, _ := forumBindEnv(t)
		bot.ProcessUpdate(tele.Update{Message: &tele.Message{ID: 1, Chat: forumChat, Sender: &tele.User{ID: 1}, Text: "/bot_bind"}})
		press(bot, forumChat, 9001, 0, "bind_select", "1")
		checkRecorded(t, s)
		want := "✅ Bound to topic.\n🏷 alpha → -1001234567890, topic=77"
		if text := callText(lastCall(t, s, "editMessageText")); text != want {
			t.Errorf("last edit text = %q, want %q", text, want)
		}
	})
}

func TestForumBind_ReuseAfterLeaving(t *testing.T) {
	s, bot, _ := forumBindEnv(t)
	bindAlpha := func(chat *tele.Chat, msgID int) {
		t.Helper()
		press(bot, chat, msgID, 0, "settings_bind", "")
		press(bot, chat, msgID, 0, "bind_select", bindNumber(t, lastCall(t, s, "editMessageText"), ": alpha"))
	}
	forumRoute := config.NameRoute{ChatID: forumChat.ID, TopicID: 77, Title: "Test Forum"}
	bindAlpha(forumChat, 4000)
	wantRoute(t, "alpha", forumRoute)
	if n := s.Count("createForumTopic"); n != 1 {
		t.Fatalf("createForumTopic count after first bind = %d, want 1", n)
	}
	bindAlpha(plainChat, 4100)
	wantRoute(t, "alpha", config.NameRoute{ChatID: plainChat.ID, TopicID: 0, Title: "Plain Group"})
	if got := loadCreds(t).ForumTopics[config.ForumTopicKey(forumChat.ID, "alpha")]; got != 77 {
		t.Errorf("ForumTopics record after leaving = %d, want 77", got)
	}
	if n := s.Count("createForumTopic"); n != 1 {
		t.Errorf("createForumTopic count after leaving = %d, want 1", n)
	}
	bindAlpha(forumChat, 4200)
	wantRoute(t, "alpha", forumRoute)
	if n := s.Count("createForumTopic"); n != 1 {
		t.Errorf("createForumTopic count after returning = %d, want 1 (topic reused)", n)
	}
}

func TestForumBind_Failures(t *testing.T) {
	t.Run("createForumTopic fails", func(t *testing.T) {
		s, bot, _ := forumBindEnv(t)
		s.On("createForumTopic", func(p map[string]interface{}) tgfake.Reply {
			return tgfake.Reply{Err: "Bad Request: not enough rights to create a topic"}
		})
		press(bot, forumChat, 5000, 0, "settings_bind", "")
		press(bot, forumChat, 5000, 0, "bind_select", "1")
		want := "❌ telegram: Bad Request: not enough rights to create a topic (400)"
		if text := callText(lastCall(t, s, "editMessageText")); text != want {
			t.Errorf("last edit text = %q, want %q", text, want)
		}
		creds := loadCreds(t)
		if _, ok := creds.NameRouteMap["alpha"]; ok || len(creds.ForumTopics) != 0 {
			t.Errorf("failed bind stored data: routes=%v topics=%v", creds.NameRouteMap, creds.ForumTopics)
		}
	})
}

func TestBuildRoutesSections_NoGetChat(t *testing.T) {
	s, bot, bs := forumBindEnv(t)
	creds := loadCreds(t)
	creds.NameRouteMap = map[string]config.NameRoute{
		"alpha":   {ChatID: -1001234567890, TopicID: 77, Title: "Test Forum"},
		"beta":    {ChatID: -1009999000002},
		"gamma":   {ChatID: -1009999000003, Title: "Other"},
		"dev&ops": {ChatID: -1009999000004, Title: "R&D"},
	}
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	lines := strings.Split(buildRoutesSections(bs)[0], "\n")
	if lines[0] != "📋 Agent routes:" {
		t.Errorf("first line = %q, want the routes header", lines[0])
	}
	got := lines[1:]
	want := []string{
		"  alpha (main:1.0) → Test Forum (-1001234567890), topic=#77",
		"  beta → -1009999000002",
		"  gamma → Other (-1009999000003)",
		"  dev&amp;ops → R&amp;D (-1009999000004)",
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("route lines =\n%q\nwant\n%q", got, want)
	}
	if n := s.Count("getChat"); n != 0 {
		t.Errorf("getChat count after buildRoutesSections = %d, want 0", n)
	}
	m := methodsDuring(s, func() { press(bot, forumChat, 7000, 0, "settings", "routes") })
	if len(m) == 0 || m[0] != "answerCallbackQuery" || !slices.Contains(m[1:], "editMessageText") {
		t.Errorf("methods of the Routes button = %v, want answerCallbackQuery first, then editMessageText", m)
	}
	if n := s.Count("getChat"); n != 0 {
		t.Errorf("getChat count after the Routes button = %d, want 0", n)
	}
}

func TestNewGroupTitle(t *testing.T) {
	_, bot, _ := forumBindEnv(t)
	creds := loadCreds(t)
	creds.NameRouteMap = map[string]config.NameRoute{
		"alpha": {ChatID: forumChat.ID, Title: "Old"},
		"beta":  {ChatID: forumChat.ID, TopicID: 9, Title: "Old"},
		"gamma": {ChatID: -1009999000003, Title: "Other"},
	}
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	bot.ProcessUpdate(tele.Update{Message: &tele.Message{ID: 1, Chat: forumChat, NewGroupTitle: "New"}})
	wantRoute(t, "alpha", config.NameRoute{ChatID: forumChat.ID, Title: "New"})
	wantRoute(t, "beta", config.NameRoute{ChatID: forumChat.ID, TopicID: 9, Title: "New"})
	wantRoute(t, "gamma", config.NameRoute{ChatID: -1009999000003, Title: "Other"})
}
