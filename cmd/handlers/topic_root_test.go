package handlers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/tgfake"
	tele "gopkg.in/telebot.v3"
)

// topicRootNotifyText is the first line of the bot notification the reply tests reply to; its 📟 line names the target.
const topicRootNotifyText = "🔔 Stop"

// topicRootMsg is a message as Telegram delivers one typed in topic 12 of forumChat: ThreadID 12, the topic flag
// set, and reply_to_message = the topic's creation message (id 12).
func topicRootMsg(id int, text string) *tele.Message {
	return &tele.Message{
		ID: id, Chat: forumChat, Sender: &tele.User{ID: 1}, Text: text, ThreadID: 12, TopicMessage: true,
		ReplyTo: &tele.Message{ID: 12, Chat: forumChat, ThreadID: 12, TopicMessage: true},
	}
}

// topicRootEnv is one named session "troot" bound to topic 12 of forumChat: a pane on a private tmux server,
// the real handlers (Register) on a fake Bot API, and a log file that receives the INFO lines.
type topicRootEnv struct {
	s    *tgfake.Server
	bot  *tele.Bot
	bs   *types.BotState
	pane string
	log  string
}

func newTopicRootEnv(t *testing.T) *topicRootEnv {
	t.Helper()
	setupTestConfig(t, config.AppConfig{})
	creds := config.Credentials{
		PairingAllow: config.PairingAllow{IDs: []string{fmt.Sprint(forumChat.ID), fmt.Sprint(plainChat.ID)}},
		Port:         12500,
	}
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	if err := config.SaveRoutes(config.Routes{NameRouteMap: map[string]config.NameRoute{"troot": {ChatID: forumChat.ID, TopicID: 12}}}); err != nil {
		t.Fatalf("SaveRoutes: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "bot.log")
	logger.Init(logPath, true) // debug=true so INFO lands in the file
	// os.DevNull, not "": an empty path makes lumberjack write later WARN/ERROR lines to <os.TempDir()>/handlers.test-lumberjack.log.
	t.Cleanup(func() { logger.Init(os.DevNull, false) })

	// A private tmux server, as in TestProcessUserInput_GroupQuickReply_ReactionRecordedNoVoiceEcho: kill-server,
	// then the socket file, then ServerName back; the PID in the name keeps two concurrent runs apart.
	origServerName := injector.ServerName
	injector.ServerName = fmt.Sprintf("tgcli-test-troot-%d", os.Getpid())
	t.Setenv("SHELL", "/bin/sh") // the pane runs a plain sh: zsh in the temp HOME would open its first-run menu
	var socketPath string
	t.Cleanup(func() {
		injector.GlobalTmuxCmd("kill-server").Run()
		if socketPath != "" {
			os.Remove(socketPath)
		}
		injector.ServerName = origServerName
	})
	if err := injector.CreateSession("troot-session", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	out, err := injector.GlobalTmuxCmd("display-message", "-p", "#{socket_path}").Output()
	if err != nil {
		t.Fatalf("display-message #{socket_path}: %v", err)
	}
	socketPath = strings.TrimSpace(string(out))
	panes, err := injector.ListPanes("troot-session")
	if err != nil || len(panes) == 0 {
		t.Fatalf("ListPanes: %v panes=%v", err, panes)
	}

	s := tgfake.New(t)
	s.On("getChat", func(p map[string]interface{}) tgfake.Reply {
		return tgfake.Reply{Result: map[string]interface{}{"id": forumChat.ID, "type": "supergroup", "title": "Test Forum", "is_forum": true}}
	})
	bot := s.Bot(t)
	bs := &types.BotState{
		Bot:             bot,
		Pages:           stores.NewPageCacheStore(),
		SessionState:    stores.NewSessionStateStore(t.TempDir()),
		MergeBuffers:    stores.NewMergeBufferStore(t.TempDir()),
		AtChannels:      stores.NewAtChannelStore(t.TempDir()),
		ReactionTracker: stores.NewReactionTrackerStore(),
		PendingWait:     stores.NewPendingWaitStore(),
		HookRunning:     stores.NewHookRunningStateStore(),
		InjectQueue:     stores.NewInjectQueueStore(t.TempDir()),
		InjectConfirm:   stores.NewInjectConfirmStore(),
		StopCooldown:    stores.NewStopCooldownStore(),
		PendingMsgStore: stores.NewPendingMsgStore(),
		CommandStats:    stores.NewCommandStatsStore(t.TempDir()),
	}
	bs.SessionState.Add("sess-troot", panes[0], "/work/troot", "")
	if ok, msg := bs.SessionState.SetName("sess-troot", "troot"); !ok {
		t.Fatalf("SetName: %s", msg)
	}
	Register(bs)
	return &topicRootEnv{s: s, bot: bot, bs: bs, pane: panes[0], log: logPath}
}

// deliver runs msg through the registered handlers. Like the real hook, a goroutine reports the
// UserPromptSubmit event with injected, the prompt the handler is expected to inject, so the injection
// is confirmed at once instead of after SafeInjectText's timeout.
func (e *topicRootEnv) deliver(msg *tele.Message, injected string) {
	go func() {
		time.Sleep(200 * time.Millisecond)
		e.bs.InjectConfirm.NotifyUserPromptSubmit(e.pane, injected)
	}()
	e.bot.ProcessUpdate(tele.Update{Message: msg})
}

// replies returns the text of every sendMessage the bot made.
func (e *topicRootEnv) replies() []string {
	var out []string
	for _, c := range e.s.Calls() {
		if c.Method == "sendMessage" {
			out = append(out, callText(c))
		}
	}
	return out
}

func (e *topicRootEnv) logText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.log)
	if err != nil && !os.IsNotExist(err) { // the log file is created by the first INFO line
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

func (e *topicRootEnv) paneText(t *testing.T) string {
	t.Helper()
	out, err := injector.CapturePane(injector.TmuxTarget{PaneID: e.pane})
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	return out
}

// TestTopicRoot_TextReachesBoundSession covers the boss's case: a text typed in a bound topic (ReplyTo = the
// topic's creation message) is injected into the session bound to (chat, topic 12) through the group route,
// and the reply path's "No tmux session info" error is not sent.
func TestTopicRoot_TextReachesBoundSession(t *testing.T) {
	e := newTopicRootEnv(t)
	const text = "topicroot-text-1"
	e.deliver(topicRootMsg(17, text), text)
	for _, r := range e.replies() {
		if strings.Contains(r, "No tmux session info") {
			t.Errorf("reply %q sent for a message typed in the topic", r)
		}
	}
	if pane := e.paneText(t); !strings.Contains(pane, text) {
		t.Errorf("pane does not contain %q:\n%s", text, pane)
	}
	log := e.logText(t)
	if want := "Group quick reply: target=" + e.pane + " voice=false text=" + text; !strings.Contains(log, want) {
		t.Errorf("log has no %q", want)
	}
	if strings.Contains(log, "Injected reply to") {
		t.Errorf("log has an \"Injected reply to\" line: the reply path ran")
	}
}

// TestTopicRoot_RealReplyKeepsReplyPath: a reply to a bot notification keeps the reply path (the target comes
// from the notification's 📟 line and the notification is quoted), both inside a topic (ReplyTo.ID differs from
// ThreadID) and in a supergroup without topics, where ThreadID equals ReplyTo.ID but the topic flag is false.
func TestTopicRoot_RealReplyKeepsReplyPath(t *testing.T) {
	cases := []struct {
		name  string
		build func(id int, text, notify string) *tele.Message
	}{
		{"reply to a notification inside the topic", func(id int, text, notify string) *tele.Message {
			return &tele.Message{ID: id, Chat: forumChat, Sender: &tele.User{ID: 1}, Text: text, ThreadID: 12, TopicMessage: true,
				ReplyTo: &tele.Message{ID: 40, Chat: forumChat, Text: notify, ThreadID: 12, TopicMessage: true}}
		}},
		{"reply thread in a supergroup without topics", func(id int, text, notify string) *tele.Message {
			return &tele.Message{ID: id, Chat: plainChat, Sender: &tele.User{ID: 1}, Text: text, ThreadID: 40,
				ReplyTo: &tele.Message{ID: 40, Chat: plainChat, Text: notify}}
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTopicRootEnv(t)
			text := fmt.Sprintf("topicroot-reply-%d", i)
			notify := topicRootNotifyText + "\n📟 " + e.pane
			e.deliver(c.build(18, text, notify), "> "+topicRootNotifyText+"\n> 📟 "+e.pane+"\n\n"+text)
			if pane := e.paneText(t); !strings.Contains(pane, text) {
				t.Errorf("pane does not contain %q:\n%s", text, pane)
			}
			log := e.logText(t)
			if want := "Injected reply to " + e.pane + " voice=false text=" + text; !strings.Contains(log, want) {
				t.Errorf("log has no %q", want)
			}
			if strings.Contains(log, "Group quick reply:") {
				t.Errorf("log has a \"Group quick reply:\" line: the group route ran for a real reply")
			}
		})
	}
}

// TestTopicRoot_Commands: the commands with a separate reply branch (an OnText command such as /stop, /resume,
// /bot_bind, /bot_unbind, /bot_name), typed in the bound topic, take their no-reply branch: each gives its
// no-reply answer, not the reply path's "No tmux session info" error.
func TestTopicRoot_Commands(t *testing.T) {
	cases := []struct {
		text   string
		prefix string
	}{
		{"/stop", "⏹ Escape sent"},
		{"/resume", "📂 No previous sessions found for this project."},
		{"/bot_bind", "Select a session to bind to this chat:"},
		{"/bot_unbind", "Select a route to unbind:"},
		{"/bot_name renamed", "✅ Session named: renamed"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			e := newTopicRootEnv(t)
			e.bot.ProcessUpdate(tele.Update{Message: topicRootMsg(19, c.text)})
			replies := e.replies()
			if len(replies) != 1 || !strings.HasPrefix(replies[0], c.prefix) {
				t.Errorf("replies = %q, want one starting with %q", replies, c.prefix)
			}
		})
	}
}
