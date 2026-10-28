package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	tele "gopkg.in/telebot.v3"
)

// TestProcessUserInput_MergeBufferActive_NoVoiceEcho covers SPEC TC10: processUserInput with an active
// merge buffer for the chat must not send a separate voice-echo reply — master 03b1c74 messages.go:403
// c.Reply(voicePrefix + " " + text) inside the merge-buffer branch is deleted.
func TestProcessUserInput_MergeBufferActive_NoVoiceEcho(t *testing.T) {
	setupTestConfig(t, config.AppConfig{VoicePrefix: "🗣️"})
	bot, tc := newTrackingBot(t)
	bs := &types.BotState{Bot: bot, MergeBuffers: stores.NewMergeBufferStore(t.TempDir())}

	chat := &tele.Chat{ID: 7001, Type: tele.ChatPrivate}
	key := stores.MergeKey(chat.ID)
	// notifyMsgID=0 deliberately skips the RetryEdit preview-edit branch (messages.go: "if notifyMsgID
	// != 0") — unrelated to this assertion, and would otherwise add an editMessageText call that has
	// nothing to do with the voice echo.
	bs.MergeBuffers.Start(key, chat.ID, "%1", 0)

	msg := &tele.Message{ID: 20, Chat: chat, Sender: &tele.User{ID: 1}}
	ctx := bot.NewContext(tele.Update{Message: msg})

	if err := processUserInput(bs, ctx, bot, "hello", true, "🗣️"); err != nil {
		t.Fatalf("processUserInput: %v", err)
	}
	if calls := tc.snapshot(); len(calls) != 0 {
		t.Errorf("calls = %+v, want none — the merge-buffer branch must not send a voice-echo reply", calls)
	}
}

// TestProcessUserInput_GroupQuickReply_ReactionRecordedNoVoiceEcho covers SPEC TC11: the group
// quick-reply branch's sendFeedback (messages.go:522-524; the echo deleted from master 03b1c74 messages.go:427 per Step 2) must record the
// ✍ receipt-acknowledgement reaction and must NOT send a separate voice-echo reply, exercised through
// the real processUserInput call (not a narrower seam) via resolveGroupTarget -> sendFeedback ->
// InjectMessage/SafeInjectText against a real tmux pane on a private, test-only server.
//
// Goes red if: the deleted c.Reply(voicePrefix+" "+text) (master 03b1c74 messages.go:427) is reintroduced (fails C3);
// or recordPending is dropped from sendFeedback (fails C2).
func TestProcessUserInput_GroupQuickReply_ReactionRecordedNoVoiceEcho(t *testing.T) {
	setupTestConfig(t, config.AppConfig{VoicePrefix: "🗣️"})

	// Isolate on a private tmux server so this test never touches the default server where live agent
	// sessions run. kill-server stops the server but leaves its socket file behind, so socketPath is
	// captured right after CreateSession and removed in Cleanup, in that order — kill-server first, then
	// the exact socket file, then restore ServerName. PID-based name so two concurrent test runs (e.g. a
	// normal run and a -race run) cannot kill each other's server.
	origServerName := injector.ServerName
	testServer := fmt.Sprintf("tgcli-test-tc11-%d", os.Getpid())
	injector.ServerName = testServer
	var socketPath string
	t.Cleanup(func() {
		injector.GlobalTmuxCmd("kill-server").Run()
		if socketPath != "" {
			os.Remove(socketPath)
		}
		injector.ServerName = origServerName
	})

	const sessionName = "tc11-pane-session"
	if err := injector.CreateSession(sessionName, ""); err != nil {
		t.Fatalf("CreateSession on private server %s: %v", testServer, err)
	}
	if out, err := injector.GlobalTmuxCmd("display-message", "-p", "#{socket_path}").Output(); err == nil {
		socketPath = strings.TrimSpace(string(out))
	} else {
		t.Fatalf("display-message #{socket_path}: %v", err)
	}
	panes, err := injector.ListPanes(sessionName)
	if err != nil || len(panes) == 0 {
		t.Fatalf("ListPanes: %v panes=%v", err, panes)
	}
	pane := panes[0] // bare pane id, e.g. "%0" — NO "@socket" suffix

	// Guard proving the bare pane id resolves on THIS private server before anything is pasted into it —
	// a bare pane id only means something relative to one tmux server, and the default server is where
	// live agent sessions run.
	out, err := injector.TmuxCmd(injector.TmuxTarget{PaneID: pane}, "display-message", "-p", "-t", pane, "#{session_name}").Output()
	if err != nil {
		t.Fatalf("display-message guard: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != sessionName {
		t.Fatalf("pane %s does not resolve on private server %s: got session_name=%q, want %q", pane, testServer, got, sessionName)
	}

	bot, tc := newTrackingBot(t)
	bs := &types.BotState{
		Bot:             bot,
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
	}

	const sessionID = "sess-tc11"
	const routeName = "tc11route"
	bs.SessionState.Add(sessionID, pane, "", "")
	if ok, msg := bs.SessionState.SetName(sessionID, routeName); !ok {
		t.Fatalf("SetName: %s", msg)
	}

	const chatID int64 = 7002
	creds, err := config.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	creds.NameRouteMap = map[string]config.NameRoute{routeName: {ChatID: chatID}}
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	const text = "hello group"
	const voicePrefix = "🗣️"
	injectionText := voicePrefix + " " + text
	chat := &tele.Chat{ID: chatID, Type: tele.ChatGroup}
	msg := &tele.Message{ID: 500, Chat: chat, Sender: &tele.User{ID: 1}}
	ctx := bot.NewContext(tele.Update{Message: msg})

	// Simulate the real UserPromptSubmit hook event via InjectConfirmStore's own exported
	// NotifyUserPromptSubmit — the same method the real hook handler calls in production when a live
	// agent in the pane receives the injected text. Test setup using an already-existing public store
	// method, no production-code change. 200ms delay ensures InjectConfirm.Register (phase1) has already
	// run before this fires; if it fires first the notification is silently dropped and the test falls
	// back to a 10s timeout path — timing-only, the PASS/FAIL outcome never changes.
	go func() {
		time.Sleep(200 * time.Millisecond)
		bs.InjectConfirm.NotifyUserPromptSubmit(pane, injectionText)
	}()

	if err := processUserInput(bs, ctx, bot, text, true, voicePrefix); err != nil {
		t.Fatalf("processUserInput: %v", err)
	}

	calls := tc.snapshot()

	// No sendMessage whose decoded text == voicePrefix + " " + text. Any OTHER sendMessage (e.g. a
	// "❌ Injection failed: ..." reply if the bare pane rejects the paste) is allowed and must not fail
	// this test.
	for _, c := range calls {
		if c.method != "sendMessage" {
			continue
		}
		if decodeCallParams(t, c.body)["text"] == injectionText {
			t.Errorf("found a voice-echo sendMessage (the deleted master 03b1c74 messages.go:427 line was reintroduced): %+v", c)
		}
	}

	// Assert the ✍ reaction from the RECORDED setMessageReaction Bot API call, not from TakeReactions —
	// SafeInjectText clears the tracker on several paths (ClearReactions), so what TakeReactions returns
	// after processUserInput depends on the injection's outcome. The recorded call cannot be undone by
	// anything later: a clear only ADDS another (empty-reaction) call.
	//
	// bot.Raw marshals its payload directly as the JSON request body (telebot v3.3.8 api.go:22-28) — NOT
	// through embedSendOptions' flat string-map encoding — so chat_id/message_id are JSON numbers and
	// reaction is a JSON array, not strings. decodeCallParams (map[string]string) cannot decode this body.
	type reactionPayload struct {
		ChatID    int64 `json:"chat_id"`
		MessageID int   `json:"message_id"`
		Reaction  []struct {
			Type  string `json:"type"`
			Emoji string `json:"emoji"`
		} `json:"reaction"`
	}
	foundReaction := false
	for _, c := range calls {
		if c.method != "setMessageReaction" {
			continue
		}
		var rp reactionPayload
		if err := json.Unmarshal([]byte(c.body), &rp); err != nil {
			t.Fatalf("failed to decode setMessageReaction body: %v body=%q", err, c.body)
		}
		if rp.ChatID != chatID || rp.MessageID != msg.ID {
			continue
		}
		for _, r := range rp.Reaction {
			if r.Emoji == "✍" {
				foundReaction = true
			}
		}
	}
	if !foundReaction {
		t.Errorf("no setMessageReaction call recorded for chat_id=%d message_id=%d with emoji ✍ (recordPending dropped from sendFeedback?): calls=%+v", chatID, msg.ID, calls)
	}
}
