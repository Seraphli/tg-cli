package handlers

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/tgfake"

	tele "gopkg.in/telebot.v3"
)

// callThread returns the message_thread_id of a recorded Bot API call, "" when it carries none.
func callThread(c tgfake.Call) string {
	if v, ok := c.Params["message_thread_id"]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

// firstCall returns the first recorded call of the method.
func firstCall(t *testing.T, s *tgfake.Server, method string) tgfake.Call {
	t.Helper()
	for _, c := range s.Calls() {
		if c.Method == method {
			return c
		}
	}
	t.Fatalf("no %s call recorded (calls: %v)", method, s.Methods())
	return tgfake.Call{}
}

// TestTopicAnswer_Capture is the boss's case (rounds/3/ISSUES.md I2): /p typed in the bound topic gets the
// capture in that topic, not in General.
func TestTopicAnswer_Capture(t *testing.T) {
	e := newTopicRootEnv(t)
	if err := injector.GlobalTmuxCmd("send-keys", "-t", e.pane, "echo capture-marker", "Enter").Run(); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	for i := 0; i < 50 && !strings.Contains(e.paneText(t), "capture-marker"); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	e.bot.ProcessUpdate(tele.Update{Message: topicRootMsg(20, "/p")})
	if got := callThread(firstCall(t, e.s, "sendRichMessage")); got != "12" {
		t.Errorf("capture message_thread_id = %q, want 12", got)
	}
}

// TestTopicAnswer_Menus: the menu /bot_settings typed in the bound topic is sent to that topic and keeps its
// buttons and HTML (telebot drops the options collected before a *SendOptions).
func TestTopicAnswer_Menus(t *testing.T) {
	e := newTopicRootEnv(t)
	e.bot.ProcessUpdate(tele.Update{Message: topicRootMsg(21, "/bot_settings")})
	c := firstCall(t, e.s, "sendMessage")
	if got := callThread(c); got != "12" {
		t.Errorf("message_thread_id = %q, want 12", got)
	}
	if c.Params["reply_markup"] == nil || c.Params["parse_mode"] != "HTML" {
		t.Errorf("the menu lost its buttons or HTML: %v", c.Params)
	}
}
