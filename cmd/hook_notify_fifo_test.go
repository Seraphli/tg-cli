package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/hooks"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	tele "gopkg.in/telebot.v3"
)

// notifyFIFOCall records one cb.SendEventNotification invocation captured by the test stub in
// TestHookNotify_OnMessageFIFO, after the stub's release gate opens.
type notifyFIFOCall struct {
	event     string
	sessionID string
}

// newHookNotifyFIFOState builds the minimal BotState + Callbacks needed to drive the SessionStart and
// agent_idle (aborted/error) hook paths through hooks.Register, isolated from production state
// (config.ConfigDir + bs.SessionState both point at t.TempDir()). The SendEventNotification stub blocks on
// release until the test closes it, so the caller can assert the HTTP hook response returns BEFORE the
// notification actually sends — proving the send is enqueued onto the Message FIFO rather than running
// inline on the Hook FIFO (the HTTP handler blocks on the Hook FIFO job via DispatchWithMeta).
func newHookNotifyFIFOState(t *testing.T, release <-chan struct{}) (*BotState, hooks.Callbacks, *[]notifyFIFOCall, *sync.Mutex) {
	t.Helper()
	t.Cleanup(setTestConfigDir(t, t.TempDir()))
	bs := &types.BotState{
		SessionState:  stores.NewSessionStateStore(t.TempDir()),
		HookRunning:   stores.NewHookRunningStateStore(),
		SessionEvents: stores.NewSessionEventStore(),
		MessageQueue:  stores.NewSessionEventStore(),
	}
	var mu sync.Mutex
	var calls []notifyFIFOCall
	cb := hooks.Callbacks{
		ResolveChat: func(bs *types.BotState, tmuxTarget string) (*tele.Chat, string, int) {
			return &tele.Chat{ID: 1}, "1", 0
		},
		SendEventNotification: func(bs *types.BotState, chat *tele.Chat, chatID, sessionID, event, project, cwd, tmuxTarget, body, toolName, agentName string, topicID int) int {
			<-release
			mu.Lock()
			calls = append(calls, notifyFIFOCall{event: event, sessionID: sessionID})
			mu.Unlock()
			return 0
		},
		TypingLog: func(format string, args ...interface{}) {},
	}
	return bs, cb, &calls, &mu
}

// postHookNotifyFIFO POSTs a hook payload and asserts the response returns fast (proving the notification
// send did NOT run inline on the Hook FIFO — an inline send would block the HTTP response on `release`,
// which the caller has not closed yet).
func postHookNotifyFIFO(t *testing.T, srv *httptest.Server, event string, payload map[string]string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	resp, err := client.Post(srv.URL+"/hook/"+event, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /hook/%s: %v (send appears to run INLINE on the Hook FIFO and blocked on the unreleased notification gate)", event, err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/hook/%s: expected 200, got %d", event, resp.StatusCode)
	}
	if elapsed > time.Second {
		t.Fatalf("/hook/%s response took %v — send appears to run INLINE on the Hook FIFO (should be async on the Message FIFO)", event, elapsed)
	}
}

// TestHookNotify_SessionStart_OnMessageFIFO asserts the SessionStart notification (register.go ~114, the
// ~20-min registration blocker under a TG flood) is enqueued onto the Message FIFO via DispatchAsync,
// not sent inline on the Hook FIFO.
func TestHookNotify_SessionStart_OnMessageFIFO(t *testing.T) {
	release := make(chan struct{})
	closeOnce := sync.Once{}
	closeRelease := func() { closeOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	bs, cb, calls, mu := newHookNotifyFIFOState(t, release)
	mux := http.NewServeMux()
	hooks.Register(mux, bs, 0, cb)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	postHookNotifyFIFO(t, srv, "SessionStart", map[string]string{
		"session_id":  "sess-ss-fifo",
		"tmux_target": "%501",
		"project":     "proj",
	})

	mu.Lock()
	n := len(*calls)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("SessionStart notification must not have sent yet (blocked on release), got %d calls", n)
	}

	closeRelease()
	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(*calls) == 1
	})
	mu.Lock()
	got := (*calls)[0]
	mu.Unlock()
	if got.event != "SessionStart" || got.sessionID != "sess-ss-fifo" {
		t.Fatalf("unexpected notification call: %+v", got)
	}
}

// TestHookNotify_AgentInterrupted_OnMessageFIFO asserts the agent_idle/aborted -> AgentInterrupted
// notification (register.go ~470) is enqueued onto the Message FIFO, not sent inline on the Hook FIFO.
func TestHookNotify_AgentInterrupted_OnMessageFIFO(t *testing.T) {
	release := make(chan struct{})
	closeOnce := sync.Once{}
	closeRelease := func() { closeOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	bs, cb, calls, mu := newHookNotifyFIFOState(t, release)
	mux := http.NewServeMux()
	hooks.Register(mux, bs, 0, cb)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	postHookNotifyFIFO(t, srv, "agent_idle", map[string]string{
		"session_id":  "sess-ai-fifo",
		"tmux_target": "%502",
		"project":     "proj",
		"stop_reason": "aborted",
	})

	mu.Lock()
	n := len(*calls)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("AgentInterrupted notification must not have sent yet (blocked on release), got %d calls", n)
	}

	closeRelease()
	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(*calls) == 1
	})
	mu.Lock()
	got := (*calls)[0]
	mu.Unlock()
	if got.event != "AgentInterrupted" || got.sessionID != "sess-ai-fifo" {
		t.Fatalf("unexpected notification call: %+v", got)
	}
}

// TestHookNotify_AgentError_OnMessageFIFO asserts the agent_idle/error -> AgentError notification
// (register.go ~473) is enqueued onto the Message FIFO, not sent inline on the Hook FIFO.
func TestHookNotify_AgentError_OnMessageFIFO(t *testing.T) {
	release := make(chan struct{})
	closeOnce := sync.Once{}
	closeRelease := func() { closeOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	bs, cb, calls, mu := newHookNotifyFIFOState(t, release)
	mux := http.NewServeMux()
	hooks.Register(mux, bs, 0, cb)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	postHookNotifyFIFO(t, srv, "agent_idle", map[string]string{
		"session_id":    "sess-ae-fifo",
		"tmux_target":   "%503",
		"project":       "proj",
		"stop_reason":   "error",
		"error_message": "provider retries exhausted",
	})

	mu.Lock()
	n := len(*calls)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("AgentError notification must not have sent yet (blocked on release), got %d calls", n)
	}

	closeRelease()
	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(*calls) == 1
	})
	mu.Lock()
	got := (*calls)[0]
	mu.Unlock()
	if got.event != "AgentError" || got.sessionID != "sess-ae-fifo" {
		t.Fatalf("unexpected notification call: %+v", got)
	}
}
