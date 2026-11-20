package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/tgfake"
)

// newEntry is a test helper that builds a BusyStatusEntry with a live MsgID.
func newEntry(chatID int64, topicID, msgID int, startedAt, sentAt, lastEditAt, lastFloatAt time.Time) *stores.BusyStatusEntry {
	return &stores.BusyStatusEntry{
		ChatID:      chatID,
		TopicID:     topicID,
		MsgID:       msgID,
		StartedAt:   startedAt,
		SentAt:      sentAt,
		LastEditAt:  lastEditAt,
		LastFloatAt: lastFloatAt,
		State:       stores.BusyWorking,
	}
}

func TestDecideBusyAction_NilEntry(t *testing.T) {
	now := time.Now()

	// e==nil && running → create
	if got := decideBusyAction(nil, stores.BusyWorking, time.Time{}, false, now); got != busyCreate {
		t.Errorf("nil+running: want busyCreate, got %v", got)
	}
	// e==nil && !running → none
	if got := decideBusyAction(nil, stores.BusyIdle, time.Time{}, false, now); got != busyNone {
		t.Errorf("nil+!running: want busyNone, got %v", got)
	}
}

func TestDecideBusyAction_InFlight(t *testing.T) {
	now := time.Now()
	// MsgID==0 while running → none (in-flight create, not yet completed)
	e := &stores.BusyStatusEntry{MsgID: 0, SentAt: now.Add(-30 * time.Second)}
	if got := decideBusyAction(e, stores.BusyWorking, time.Time{}, false, now); got != busyNone {
		t.Errorf("in-flight running: want busyNone, got %v", got)
	}
	// MsgID==0 while idle → none (same: a placeholder is never deleted)
	if got := decideBusyAction(e, stores.BusyIdle, time.Time{}, false, now); got != busyNone {
		t.Errorf("in-flight idle: want busyNone, got %v", got)
	}
}

func TestDecideBusyAction_Refloat(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	lastFloatAt := now.Add(-3 * time.Second) // >=2s ago
	// lastMark is AFTER sentAt → re-float
	lastMark := now.Add(-25 * time.Second) // after sentAt
	e := newEntry(1, 0, 42, sentAt, sentAt, now.Add(-20*time.Second), lastFloatAt)

	if got := decideBusyAction(e, stores.BusyWorking, lastMark, false, now); got != busyRefloat {
		t.Errorf("refloat: want busyRefloat, got %v", got)
	}
}

func TestDecideBusyAction_Refloat_Backoff(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	lastFloatAt := now.Add(-3 * time.Second)
	lastMark := now.Add(-25 * time.Second)
	e := newEntry(1, 0, 42, sentAt, sentAt, now.Add(-20*time.Second), lastFloatAt)

	// In flood backoff → no refloat
	if got := decideBusyAction(e, stores.BusyWorking, lastMark, true, now); got == busyRefloat {
		t.Errorf("backoff-gated: refloat must not fire during backoff, got %v", got)
	}
}

func TestDecideBusyAction_Refloat_Debounce(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	// lastFloatAt < 2s ago → debounce blocks refloat
	lastFloatAt := now.Add(-1 * time.Second)
	lastMark := now.Add(-25 * time.Second)
	e := newEntry(1, 0, 42, sentAt, sentAt, now.Add(-20*time.Second), lastFloatAt)

	got := decideBusyAction(e, stores.BusyWorking, lastMark, false, now)
	if got == busyRefloat {
		t.Errorf("2s debounce: refloat must not fire < 2s after lastFloatAt, got %v", got)
	}
}

func TestDecideBusyAction_RefloatBeforeEdit(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	lastFloatAt := now.Add(-3 * time.Second)
	// lastEditAt also >15s ago → but refloat condition is met first → refloat wins
	lastEditAt := now.Add(-20 * time.Second)
	lastMark := now.Add(-25 * time.Second)
	e := newEntry(1, 0, 42, sentAt, sentAt, lastEditAt, lastFloatAt)

	if got := decideBusyAction(e, stores.BusyWorking, lastMark, false, now); got != busyRefloat {
		t.Errorf("precedence: want busyRefloat over busyEdit, got %v", got)
	}
}

func TestDecideBusyAction_Edit(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	lastFloatAt := now.Add(-3 * time.Second)
	// lastMark NOT after sentAt → no refloat; but lastEditAt >15s ago → edit
	lastMark := sentAt.Add(-5 * time.Second) // before sentAt
	lastEditAt := now.Add(-20 * time.Second)
	e := newEntry(1, 0, 42, sentAt, sentAt, lastEditAt, lastFloatAt)

	if got := decideBusyAction(e, stores.BusyWorking, lastMark, false, now); got != busyEdit {
		t.Errorf("edit: want busyEdit, got %v", got)
	}
}

func TestDecideBusyAction_Edit_Backoff(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	lastFloatAt := now.Add(-3 * time.Second)
	lastMark := sentAt.Add(-5 * time.Second)
	lastEditAt := now.Add(-20 * time.Second)
	e := newEntry(1, 0, 42, sentAt, sentAt, lastEditAt, lastFloatAt)

	// In flood backoff → no edit
	if got := decideBusyAction(e, stores.BusyWorking, lastMark, true, now); got == busyEdit {
		t.Errorf("backoff-gated: edit must not fire during backoff, got %v", got)
	}
}

// TestDecideBusyAction_E_FreshPostSendStampConsumesMark is the f29 E regression. With SentAt stamped by a
// FRESH time.Now() taken AFTER the send completes (the fix), a FloatMarker mark created DURING that send
// (tickNow < lastMark < postSendStamp) is CONSUMED by SentAt and must NOT spuriously re-float. Before the
// fix SentAt held the pre-I/O tick `now`, so such a mark satisfied lastMark.After(SentAt) and re-floated
// ~2s later. A mark created AFTER the post-send stamp still triggers a legit re-float (positive control).
func TestDecideBusyAction_E_FreshPostSendStampConsumesMark(t *testing.T) {
	tickNow := time.Now()
	postSendStamp := tickNow.Add(1 * time.Second)         // the send took ~1s; the fresh stamp is AFTER the tick
	markDuringSend := tickNow.Add(500 * time.Millisecond) // tickNow < mark < postSendStamp
	now := postSendStamp.Add(3 * time.Second)             // past the 2s LastFloatAt gate

	// busyCreate path with the fix: SentAt/LastFloatAt/LastEditAt = postSendStamp; StartedAt = tickNow.
	e := newEntry(1, 0, 42, tickNow, postSendStamp, postSendStamp, postSendStamp)
	if got := decideBusyAction(e, stores.BusyWorking, markDuringSend, false, now); got == busyRefloat {
		t.Errorf("f29 E: a mark created DURING the send (consumed by the fresh post-send SentAt) must NOT refloat, got %v", got)
	}

	// Chain: after a re-float, SentAt2 is a later post-send stamp; a mark before it must still not refloat.
	postSendStamp2 := now
	now2 := postSendStamp2.Add(3 * time.Second)
	markBeforeStamp2 := postSendStamp2.Add(-500 * time.Millisecond)
	e2 := newEntry(1, 0, 43, tickNow, postSendStamp2, postSendStamp2, postSendStamp2)
	if got := decideBusyAction(e2, stores.BusyWorking, markBeforeStamp2, false, now2); got == busyRefloat {
		t.Errorf("f29 E (chain): after a re-float, a mark before the fresh stamp must NOT refloat again, got %v", got)
	}

	// Positive control: a genuine outbound message AFTER the post-send stamp still triggers a legit refloat.
	markAfterStamp := postSendStamp.Add(500 * time.Millisecond) // > SentAt
	if got := decideBusyAction(e, stores.BusyWorking, markAfterStamp, false, now); got != busyRefloat {
		t.Errorf("f29 E: a mark created AFTER the send must still refloat (the fix must not over-suppress), got %v", got)
	}
}

func TestBusyDisplayName(t *testing.T) {
	cases := []struct {
		name       string
		tmuxTarget string
		want       string
	}{
		{"", "%1@/tmp/x", "%1"},
		{"mysession", "%1@/tmp/x", "mysession"},
		{"", "%3", "%3"},
		{"", "", ""},
	}
	for _, tc := range cases {
		got := busyDisplayName(tc.name, tc.tmuxTarget)
		if got != tc.want {
			t.Errorf("busyDisplayName(%q, %q) = %q, want %q", tc.name, tc.tmuxTarget, got, tc.want)
		}
	}
}

func TestStatusText(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    string
	}{
		{0, "⏳ [mybot] Working… (0s)"},
		{15 * time.Second, "⏳ [mybot] Working… (15s)"},
		{2*time.Minute + 15*time.Second, "⏳ [mybot] Working… (2m15s)"},
		{60 * time.Second, "⏳ [mybot] Working… (1m0s)"},
	}
	for _, tc := range cases {
		got := statusText("mybot", tc.elapsed, stores.BusyWorking, "")
		if got != tc.want {
			t.Errorf("statusText(%v) = %q, want %q", tc.elapsed, got, tc.want)
		}
	}
}

// TestStatusTextWaiting covers the stall text with the countdown copied from the pane.
func TestStatusTextWaiting(t *testing.T) {
	if got, want := statusText("mybot", 2*time.Minute+15*time.Second, stores.BusyWaiting, "4m 40s"), "⚠️ [mybot] Waiting for API response · retry in 4m 40s (2m15s)"; got != want {
		t.Errorf("waiting with countdown = %q, want %q", got, want)
	}
}

// TestStatusTextCompacting covers the compaction text: its own message, in English.
func TestStatusTextCompacting(t *testing.T) {
	if got, want := statusText("mybot", 2*time.Minute+15*time.Second, stores.BusyCompacting, ""), "🗜️ [mybot] Compacting conversation (2m15s)"; got != want {
		t.Errorf("compacting = %q, want %q", got, want)
	}
}

// TestDecideBusyAction_StateChange proves a change of the shown state (working, waiting, compacting) edits the
// message at the next tick, not at the 15s cadence: an entry edited 1s ago is left alone while the shown state
// is still right.
func TestDecideBusyAction_StateChange(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	e := newEntry(1, 0, 42, sentAt, sentAt, now.Add(-time.Second), now.Add(-3*time.Second))
	for _, c := range []struct {
		shown, current stores.BusyState
		want           busyAction
	}{
		{stores.BusyWorking, stores.BusyWorking, busyNone},
		{stores.BusyWorking, stores.BusyWaiting, busyEdit},
		{stores.BusyWaiting, stores.BusyWorking, busyEdit},
		{stores.BusyWorking, stores.BusyCompacting, busyEdit},
	} {
		e.State = c.shown
		if got := decideBusyAction(e, c.current, time.Time{}, false, now); got != c.want {
			t.Errorf("message shows %v, session now %v: want %v, got %v", c.shown, c.current, c.want, got)
		}
	}
}

// TestBusyTick_StatusMessageOnlyForSingleSessionGroupRoute covers where a working session can be routed: an
// unnamed session falls back to the private chat (the paired user's own chat), two sessions bound to the same
// group chat share one route, and a second session that is idle must not make the shared route show and drop a
// message as the first one runs. None of these gets a busy status message; a working session alone on a group
// route does, and its message follows the session: edited when it starts to compact, deleted at the first tick that
// reads idle.
func TestBusyTick_StatusMessageOnlyForSingleSessionGroupRoute(t *testing.T) {
	t.Cleanup(setTestConfigDir(t, t.TempDir()))
	routes := config.Routes{NameRouteMap: map[string]config.NameRoute{}}
	for name, chat := range map[string]int64{"grp": -1002, "shared-a": -1003, "shared-b": -1003, "mixed-a": -1004, "mixed-b": -1004} {
		routes.NameRouteMap[name] = config.NameRoute{ChatID: chat}
	}
	if err := config.SaveRoutes(routes); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveCredentials(config.Credentials{PairingAllow: config.PairingAllow{DefaultChatID: "5001"}}); err != nil {
		t.Fatal(err)
	}
	const idleTarget = "%6@/tmp/tg-cli-test-none"
	busy := stores.BusyWorking
	orig := liveBusyState
	liveBusyState = func(_ *stores.HookRunningStateStore, target string) (stores.BusyState, string) {
		if target == idleTarget {
			return stores.BusyIdle, ""
		}
		return busy, ""
	}
	t.Cleanup(func() { liveBusyState = orig })
	s := tgfake.New(t)
	bs := &BotState{
		Bot:          s.Bot(t),
		SessionState: stores.NewSessionStateStore(t.TempDir()),
		HookRunning:  stores.NewHookRunningStateStore(),
		BusyStatus:   stores.NewBusyStatusStore(t.TempDir()),
	}
	for i, name := range []string{"", "grp", "shared-a", "shared-b", "mixed-a", "mixed-b"} {
		sid := fmt.Sprintf("sess-%d", i)
		bs.SessionState.Add(sid, fmt.Sprintf("%%%d@/tmp/tg-cli-test-none", i+1), "/work", "")
		if name != "" {
			bs.SessionState.SetName(sid, name)
		}
	}
	runBusyTick(bs)
	waitUntil(t, 2*time.Second, func() bool { return s.Count("sendMessage") > 0 })
	time.Sleep(200 * time.Millisecond)
	var chats []string
	for _, c := range s.Calls() {
		if c.Method == "sendMessage" {
			chats = append(chats, fmt.Sprint(c.Params["chat_id"]))
		}
	}
	if len(chats) != 1 || chats[0] != "-1002" {
		t.Errorf("busy status messages went to chats %v, want only the single-session group route -1002", chats)
	}
	// The session starts to compact: one edit at the next tick, none at the tick after (the entry holds the shown state).
	busy = stores.BusyCompacting
	runBusyTick(bs)
	waitUntil(t, 2*time.Second, func() bool { return s.Count("editMessageText") > 0 })
	runBusyTick(bs)
	time.Sleep(200 * time.Millisecond)
	if n := s.Count("editMessageText"); n != 1 {
		t.Errorf("%d edits in two ticks of a compacting session, want 1", n)
	}
	// The session turns idle: the very next tick deletes the message.
	busy = stores.BusyIdle
	runBusyTick(bs)
	waitUntil(t, 2*time.Second, func() bool { return s.Count("deleteMessage") > 0 })
}

// TestBusyTick_CodexErrorTurnNotifies covers the end of a codex turn that ended on an API error: codex has no hook
// there, so the loop reads the rollout file at the busy→idle change. The route of the session whose rollout holds
// an error turn gets ONE Run Error message; a session whose rollout file is gone gets none; the next tick (the
// stored state is idle by then) sends nothing more.
func TestBusyTick_CodexErrorTurnNotifies(t *testing.T) {
	t.Cleanup(setTestConfigDir(t, t.TempDir()))
	if err := config.SaveRoutes(config.Routes{NameRouteMap: map[string]config.NameRoute{"cx-err": {ChatID: -1005}, "cx-gone": {ChatID: -1006}}}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveCredentials(config.Credentials{PairingAllow: config.PairingAllow{DefaultChatID: "5001"}}); err != nil {
		t.Fatal(err)
	}
	orig := liveBusyState
	liveBusyState = func(*stores.HookRunningStateStore, string) (stores.BusyState, string) { return stores.BusyIdle, "" }
	t.Cleanup(func() { liveBusyState = orig })
	s := tgfake.New(t)
	bs := &BotState{
		Bot:          s.Bot(t),
		SessionState: stores.NewSessionStateStore(t.TempDir()),
		HookRunning:  stores.NewHookRunningStateStore(),
		BusyStatus:   stores.NewBusyStatusStore(t.TempDir()),
		MessageQueue: stores.NewSessionEventStore(),
	}
	rollouts := map[string]string{"cx-err": "../tests/fixtures/codex-rollout/error-turn.jsonl", "cx-gone": filepath.Join(t.TempDir(), "gone.jsonl")}
	for i, name := range []string{"cx-err", "cx-gone"} {
		target := fmt.Sprintf("%%%d@/tmp/tg-cli-test-none", 11+i)
		bs.SessionState.Add("sid-"+name, target, "/work", rollouts[name])
		bs.SessionState.SetName("sid-"+name, name)
		bs.SessionState.SetBackend("sid-"+name, "codex")
		bs.HookRunning.SetBusyState(target, stores.BusyWorking, "")
	}
	runBusyTick(bs)
	waitUntil(t, 2*time.Second, func() bool { return s.Count("sendRichMessage") > 0 })
	runBusyTick(bs)
	time.Sleep(300 * time.Millisecond)
	var sent []string
	for _, c := range s.Calls() {
		if c.Method == "sendRichMessage" || c.Method == "sendMessage" {
			sent = append(sent, fmt.Sprint(c.Params["chat_id"], " ", c.Params["rich_message"], c.Params["text"]))
		}
	}
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "-1005 ") || !strings.Contains(sent[0], "Run Error") ||
		!strings.Contains(sent[0], "⚠️ codex run error") || !strings.Contains(sent[0], "The usage limit of this test account is reached.") {
		t.Errorf("messages sent = %q, want exactly one to chat -1005 holding Run Error, the codex run error line and the rollout's message", sent)
	}
}
