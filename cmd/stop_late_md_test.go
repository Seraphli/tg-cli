package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/hooks"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/tgfake"
	tele "gopkg.in/telebot.v3"
)

// The reply of the 2026-11-19 duplicate, with invented words: a heading line directly above a list inside ONE
// paragraph (paragraph 2), which cc cut after the heading line.
const lateMDStopText = "Checked the deployment notes.\n\n**Site**\n- Reachable at https://example.test/app\n- Staging copy at https://example.test/staging\n\n" +
	"Three changes:\n1. The retry limit is now 5.\n2. The timeout is 30 seconds.\n3. The log level is info.\n\nNext step: run the smoke test."

// hookRig is a real hooks.Register mux (real stream flush and notification code) with a fake Telegram and one bound session.
type hookRig struct {
	bs   *types.BotState
	tg   *tgfake.Server
	post func(event string, p map[string]interface{})
}

const rigSession = "sess-late"

func newHookRig(t *testing.T, backend string) *hookRig {
	t.Helper()
	t.Cleanup(setTestConfigDir(t, t.TempDir()))
	var next atomic.Int64
	next.Store(9000)
	tg := tgfake.New(t)
	for _, m := range []string{"sendMessage", "sendRichMessage", "editMessageText"} {
		tg.On(m, func(map[string]interface{}) tgfake.Reply {
			return tgfake.Reply{Result: map[string]interface{}{"message_id": next.Add(1), "chat": map[string]interface{}{"id": 1}}}
		})
	}
	dir := t.TempDir()
	bs := &types.BotState{
		Bot: tg.Bot(t), SessionState: stores.NewSessionStateStore(t.TempDir()), SessionCounts: stores.NewSessionCountStore(),
		InjectQueue: stores.NewInjectQueueStore(dir), InjectConfirm: stores.NewInjectConfirmStore(), InjectRoute: stores.NewInjectRouteStore(),
		HookRunning: stores.NewHookRunningStateStore(), StopCooldown: stores.NewStopCooldownStore(), ToolUseMsgs: stores.NewToolUseMsgStore(),
		SessionEvents: stores.NewSessionEventStore(), MessageQueue: stores.NewSessionEventStore(), MsgIDMap: stores.NewMsgIDMap(),
		AtChannels: stores.NewAtChannelStore(dir), CompactTools: stores.NewCompactToolStore(), Streams: stores.NewStreamStore(),
		PendingWait: stores.NewPendingWaitStore(), PendingMsgStore: stores.NewPendingMsgStore(),
	}
	mux := http.NewServeMux()
	hooks.Register(mux, bs, 0, hooks.Callbacks{
		ResolveChat:              func(*types.BotState, string) (*tele.Chat, string, int) { return &tele.Chat{ID: 1}, "1", 0 },
		ProcessTranscriptUpdates: func(*types.BotState, string, string, ...bool) string { return "" },
		SendEventNotification:    sendEventNotification, TypingLog: func(string, ...interface{}) {}, RouteInjectQueue: func(*types.BotState, string, string) {},
		CheckSessionVersion: func(*types.BotState, string) {}, StreamFlush: streamFlush, StreamFlushAwaitNewText: streamFlushAwaitNewText, FlushStreamOp: flushStreamOp,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	const target = "%900"
	bs.SessionState.Add(rigSession, target, "/work", "")
	post := func(event string, p map[string]interface{}) {
		p["session_id"], p["tmux_target"], p["backend"] = rigSession, target, backend
		body, _ := json.Marshal(p)
		resp, err := http.Post(srv.URL+"/hook/"+event, "application/json", bytes.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /hook/%s: %v", event, err)
		}
		resp.Body.Close()
	}
	return &hookRig{bs, tg, post}
}

// lateMDRun drives a real hooks.Register mux (real stream flush and notification code, fake Telegram) through an
// earlier bubble ("none", "sealed" or "live"), the Stop of lateMDStopText and then the late deltas; it returns the
// payloads of the rich messages the late deltas caused.
func lateMDRun(t *testing.T, earlier string, lateID string, late func(post func(idx int, final bool, delta string))) []string {
	t.Helper()
	rig := newHookRig(t, "cc")
	bs, tg, post := rig.bs, rig.tg, rig.post
	const sid = rigSession
	md := func(id string, idx int, final bool, delta string) {
		post("MessageDisplay", map[string]interface{}{"message_id": id, "turn_id": "T1", "index": idx, "final": final, "delta": delta})
	}
	want := 1 // the Stop job's own send
	if earlier != "none" {
		md("m-early", 0, true, "Let me check the files first.")
		ss := bs.Streams.Session(sid)
		waitUntil(t, 3*time.Second, func() bool {
			ss.DataMu.Lock()
			defer ss.DataMu.Unlock()
			return ss.Msgs["m-early"] != nil
		})
		if earlier == "sealed" {
			ss.DataMu.Lock()
			ss.Msgs["m-early"].Sealed = true
			ss.DataMu.Unlock()
			want = 2 // the sealed bubble is relabelled besides the Stop's send
		}
	}
	post("Stop", map[string]interface{}{"project": "proj", "last_assistant_message": lateMDStopText})
	waitUntil(t, 5*time.Second, func() bool { return tg.Count("sendRichMessage") >= want })
	time.Sleep(400 * time.Millisecond)
	before := tg.Count("sendRichMessage")
	late(func(idx int, final bool, delta string) { md(lateID, idx, final, delta) })
	time.Sleep(600 * time.Millisecond)
	var sent []string
	for _, c := range tg.Calls() {
		if body, _ := json.Marshal(c.Params); c.Method == "sendRichMessage" {
			sent = append(sent, string(body))
		}
	}
	return sent[before:]
}

// TestStopThenLateMD: the Stop job sent the reply, then the MessageDisplay deltas of that reply arrive late. Text
// the Stop message already carries must not be sent again, wherever cc cut the deltas and in whatever order they
// arrive; a late message with any text outside the Stop message is a new message and is sent whole.
func TestStopThenLateMD(t *testing.T) {
	cut := strings.Index(lateMDStopText, "**Site**\n") + len("**Site**\n") // inside paragraph 2, after the heading line
	blank := strings.Index(lateMDStopText, "\n\n") + 2                     // on a paragraph boundary
	last := lateMDStopText[strings.LastIndex(lateMDStopText, "\n\n")+2:]   // the last paragraph
	reversed := func(post func(int, bool, string)) {
		post(1, true, lateMDStopText[cut:])
		post(0, false, lateMDStopText[:cut])
	}
	cases := []struct {
		name, earlier, lateID string
		late                  func(post func(int, bool, string))
		want                  int
		has                   []string // text the one late send must carry
	}{
		{"incident: earlier bubble sealed, cut after the heading line, idx1 before idx0", "sealed", "m-final", reversed, 0, nil},
		{"no earlier bubble, same cut and order", "none", "m-final", reversed, 0, nil},
		{"earlier bubble still live, late deltas of another message id", "live", "m-final", reversed, 0, nil},
		{"cut on a paragraph boundary, idx0 before idx1", "sealed", "m-final", func(post func(int, bool, string)) {
			post(0, false, lateMDStopText[:blank])
			post(1, true, lateMDStopText[blank:])
		}, 0, nil},
		{"a late message with text the Stop did not carry", "sealed", "m-late", func(post func(int, bool, string)) {
			post(0, true, "Also noting that the build finished in 12 seconds.")
		}, 1, []string{"Also noting that the build finished in 12 seconds."}},
		{"piece 1 inside the Stop text arrives first, piece 0 with new text second", "sealed", "m-late", func(post func(int, bool, string)) {
			post(1, true, last)
			post(0, false, "Update: the smoke test passed.\n\n")
		}, 1, []string{"Update: the smoke test passed.", "Next step: run the smoke test."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sent := lateMDRun(t, c.earlier, c.lateID, c.late)
			if len(sent) != c.want {
				t.Fatalf("late deltas caused %d rich sends, want %d", len(sent), c.want)
			}
			for _, text := range c.has {
				if !strings.Contains(sent[0], text) {
					t.Errorf("the late send lacks %q", text)
				}
			}
		})
	}
}
