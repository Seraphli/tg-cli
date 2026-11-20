package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
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
	body      string
}

// newHookNotifyFIFOState builds the minimal BotState + Callbacks needed to drive the SessionStart and
// agent_idle (aborted/error) hook paths through hooks.Register, isolated from production state
// (config.ConfigDir + bs.SessionState both point at t.TempDir()). The SendEventNotification stub blocks on
// release until the test closes it, so the caller can assert a later hook job runs BEFORE the
// notification actually sends — proving the send is enqueued onto the Message FIFO rather than running
// inline on the Hook FIFO.
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
			calls = append(calls, notifyFIFOCall{event: event, sessionID: sessionID, body: body})
			mu.Unlock()
			return 0
		},
		TypingLog: func(format string, args ...interface{}) {},
	}
	return bs, cb, &calls, &mu
}

// postHookNotifyFIFO POSTs a hook payload, then a probe hook of the same session (agent_start: it only marks its
// own pane running) and waits for the probe's effect while the notification send is still held on the unreleased
// gate. The reply no longer waits for the job, so the probe is the check: it is a later job of the same Hook
// FIFO, and when the send runs inline on that FIFO the probe stays queued behind the closed gate.
func postHookNotifyFIFO(t *testing.T, srv *httptest.Server, bs *BotState, event string, payload map[string]string) {
	t.Helper()
	post := func(event string, payload map[string]string) {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		resp, err := http.Post(srv.URL+"/hook/"+event, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /hook/%s: %v", event, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/hook/%s: expected 200, got %d", event, resp.StatusCode)
		}
	}
	post(event, payload)
	probe := "%probe-" + payload["session_id"]
	post("agent_start", map[string]string{"session_id": payload["session_id"], "tmux_target": probe, "project": "proj"})
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		if running, _ := bs.HookRunning.IsRunning(probe); running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a later hook of the session did not run within 1 s of /hook/%s: the send appears to run INLINE on the Hook FIFO, behind the unreleased notification gate", event)
		}
	}
}

// TestHookNotify_SessionStart_OnMessageFIFO asserts the SessionStart notification (register.go ~117, the
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

	postHookNotifyFIFO(t, srv, bs, "SessionStart", map[string]string{
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
// notification (register.go ~473) is enqueued onto the Message FIFO, not sent inline on the Hook FIFO.
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

	postHookNotifyFIFO(t, srv, bs, "agent_idle", map[string]string{
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
// (register.go ~476) is enqueued onto the Message FIFO, not sent inline on the Hook FIFO.
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

	postHookNotifyFIFO(t, srv, bs, "agent_idle", map[string]string{
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

// TestHookStopFailure_AgentErrorOnly covers a cc turn that ends on an API error: cc posts StopFailure instead
// of Stop, with the pane's error line in last_assistant_message. The turn gives ONE Run Error notification with
// that line and nothing of a Stop (no Task Completed notice), and the hook run state reads idle again (a state
// left running makes the idle diagnostic write a bot.log line at the next idle reading).
func TestHookStopFailure_AgentErrorOnly(t *testing.T) {
	release := make(chan struct{})
	close(release)
	bs, cb, calls, mu := newHookNotifyFIFOState(t, release)
	cb.ProcessTranscriptUpdates = func(*types.BotState, string, string, ...bool) string { return "transcript text" }
	mux := http.NewServeMux()
	hooks.Register(mux, bs, 0, cb)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	bs.HookRunning.SetRunning("%504")
	const errLine = "There's an issue with the selected model (claude-test-0)."
	postHookNotifyFIFO(t, srv, bs, "StopFailure", map[string]string{
		"session_id": "sess-sf", "tmux_target": "%504", "project": "proj",
		"error": "model_not_found", "last_assistant_message": errLine,
	})
	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(*calls) > 0
	})
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	got := append([]notifyFIFOCall(nil), *calls...)
	mu.Unlock()
	if len(got) != 1 || got[0].event != "AgentError" || got[0].body != "⚠️ cc run error\n\n"+errLine {
		t.Errorf("notifications = %+v, want exactly one AgentError with the cc run error line and the pane's error line", got)
	}
	if running, _ := bs.HookRunning.IsRunning("%504"); running {
		t.Error("hook run state after StopFailure = running, want idle")
	}
}

// TestHookUserPromptSubmit_StoresWorking covers a second Telegram message arriving right after a submit:
// the UserPromptSubmit hook stores working at once, so the session reads running before the next 1 s tick.
func TestHookUserPromptSubmit_StoresWorking(t *testing.T) {
	release := make(chan struct{})
	close(release)
	bs, cb, _, _ := newHookNotifyFIFOState(t, release)
	bs.InjectConfirm = stores.NewInjectConfirmStore()
	bs.Streams = stores.NewStreamStore()
	bs.PendingWait = stores.NewPendingWaitStore()
	mux := http.NewServeMux()
	hooks.Register(mux, bs, 0, cb)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	target := "%503@/tmp/tg-cli-test-none"
	bs.HookRunning.SetBusyState(target, stores.BusyIdle, "")
	postHookNotifyFIFO(t, srv, bs, "UserPromptSubmit", map[string]string{"session_id": "sess-ups", "tmux_target": target, "project": "proj"})
	if !helpers.IsSessionRunning(bs.HookRunning, target) {
		t.Error("IsSessionRunning after UserPromptSubmit = false, want true before any tick")
	}
}
