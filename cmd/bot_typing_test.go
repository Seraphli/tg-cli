package cmd

import (
	"fmt"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/tgfake"
)

// TestTypingTick_FollowsStoredState proves one typing tick sends Telegram's typing action for a session whose
// stored busy state is working or waiting, and none for an idle one. The panes do not exist, so a tick that
// read the pane would see every session idle.
func TestTypingTick_FollowsStoredState(t *testing.T) {
	s := tgfake.New(t)
	bs := &BotState{
		Bot:          s.Bot(t),
		SessionState: stores.NewSessionStateStore(t.TempDir()),
		HookRunning:  stores.NewHookRunningStateStore(),
		InjectQueue:  stores.NewInjectQueueStore(t.TempDir()),
	}
	routes := &config.Routes{NameRouteMap: map[string]config.NameRoute{}}
	for i, c := range []struct {
		name  string
		chat  int64
		state stores.BusyState
	}{{"alpha", -1001, stores.BusyWorking}, {"beta", -1002, stores.BusyWaiting}, {"gamma", -1003, stores.BusyIdle}} {
		target := fmt.Sprintf("%%%d@/tmp/tg-cli-test-none", i+1)
		bs.SessionState.Add("sess-"+c.name, target, "/work", "")
		bs.SessionState.SetName("sess-"+c.name, c.name)
		routes.NameRouteMap[c.name] = config.NameRoute{ChatID: c.chat}
		bs.HookRunning.SetBusyState(target, c.state, "")
	}
	typingTick(bs, routes)
	for deadline := time.Now().Add(2 * time.Second); s.Count("sendChatAction") < 2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	chats := map[string]bool{}
	for _, c := range s.Calls() {
		if c.Method == "sendChatAction" {
			chats[fmt.Sprint(c.Params["chat_id"])] = true
		}
	}
	if len(chats) != 2 || !chats["-1001"] || !chats["-1002"] {
		t.Errorf("typing sent to chats %v, want exactly the working (-1001) and waiting (-1002) sessions", chats)
	}
}
