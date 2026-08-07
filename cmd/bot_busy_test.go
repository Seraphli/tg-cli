package cmd

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
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
	}
}

func TestDecideBusyAction_NilEntry(t *testing.T) {
	now := time.Now()

	// e==nil && running → create
	if got := decideBusyAction(nil, true, time.Time{}, false, now); got != busyCreate {
		t.Errorf("nil+running: want busyCreate, got %v", got)
	}
	// e==nil && !running → none
	if got := decideBusyAction(nil, false, time.Time{}, false, now); got != busyNone {
		t.Errorf("nil+!running: want busyNone, got %v", got)
	}
}

func TestDecideBusyAction_InFlight(t *testing.T) {
	now := time.Now()
	// MsgID==0 while running → none (in-flight create, not yet completed)
	e := &stores.BusyStatusEntry{MsgID: 0, SentAt: now.Add(-30 * time.Second)}
	if got := decideBusyAction(e, true, time.Time{}, false, now); got != busyNone {
		t.Errorf("in-flight running: want busyNone, got %v", got)
	}
	// MsgID==0 while idle → none (same: don't start grace on placeholder)
	if got := decideBusyAction(e, false, time.Time{}, false, now); got != busyNone {
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

	if got := decideBusyAction(e, true, lastMark, false, now); got != busyRefloat {
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
	if got := decideBusyAction(e, true, lastMark, true, now); got == busyRefloat {
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

	got := decideBusyAction(e, true, lastMark, false, now)
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

	if got := decideBusyAction(e, true, lastMark, false, now); got != busyRefloat {
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

	if got := decideBusyAction(e, true, lastMark, false, now); got != busyEdit {
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
	if got := decideBusyAction(e, true, lastMark, true, now); got == busyEdit {
		t.Errorf("backoff-gated: edit must not fire during backoff, got %v", got)
	}
}

func TestDecideBusyAction_GraceStart(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	e := newEntry(1, 0, 42, sentAt, sentAt, now.Add(-20*time.Second), now.Add(-3*time.Second))
	// IdleSince zero → grace start
	e.IdleSince = time.Time{}

	if got := decideBusyAction(e, false, time.Time{}, false, now); got != busyGraceStart {
		t.Errorf("grace start: want busyGraceStart, got %v", got)
	}
}

func TestDecideBusyAction_GraceDelete(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	e := newEntry(1, 0, 42, sentAt, sentAt, now.Add(-20*time.Second), now.Add(-3*time.Second))
	// IdleSince set 2s ago → delete
	e.IdleSince = now.Add(-2 * time.Second)

	if got := decideBusyAction(e, false, time.Time{}, false, now); got != busyGraceDelete {
		t.Errorf("grace delete: want busyGraceDelete, got %v", got)
	}
}

func TestDecideBusyAction_GraceWait(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	e := newEntry(1, 0, 42, sentAt, sentAt, now.Add(-20*time.Second), now.Add(-3*time.Second))
	// IdleSince set <2s ago → wait (still in grace period)
	e.IdleSince = now.Add(-1 * time.Second)

	if got := decideBusyAction(e, false, time.Time{}, false, now); got != busyNone {
		t.Errorf("grace wait: want busyNone (<2s grace), got %v", got)
	}
}

// TestDecideBusyAction_BusyResumeClearsIdleSince verifies that the manager clears IdleSince when
// a session becomes running again. This is implemented in runBusyTick (not in decideBusyAction
// itself), so we verify the decideBusyAction pure logic handles an already-cleared IdleSince
// correctly (running path with no refloat/edit conditions yields busyNone).
func TestDecideBusyAction_BusyResumeNone(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-5 * time.Second)
	lastEditAt := now.Add(-5 * time.Second) // <15s ago
	lastFloatAt := now.Add(-5 * time.Second)
	lastMark := sentAt.Add(-10 * time.Second) // before sentAt: no refloat
	e := newEntry(1, 0, 42, sentAt, sentAt, lastEditAt, lastFloatAt)
	// IdleSince was cleared (busy resume)
	e.IdleSince = time.Time{}

	if got := decideBusyAction(e, true, lastMark, false, now); got != busyNone {
		t.Errorf("busy resume (no-op conditions): want busyNone, got %v", got)
	}
}

func TestDecideBusyAction_ExactGrace2s(t *testing.T) {
	now := time.Now()
	sentAt := now.Add(-30 * time.Second)
	e := newEntry(1, 0, 42, sentAt, sentAt, now.Add(-20*time.Second), now.Add(-3*time.Second))
	// IdleSince exactly 2s ago → delete fires
	e.IdleSince = now.Add(-2 * time.Second)

	if got := decideBusyAction(e, false, time.Time{}, false, now); got != busyGraceDelete {
		t.Errorf("exact 2s grace: want busyGraceDelete, got %v", got)
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
	if got := decideBusyAction(e, true, markDuringSend, false, now); got == busyRefloat {
		t.Errorf("f29 E: a mark created DURING the send (consumed by the fresh post-send SentAt) must NOT refloat, got %v", got)
	}

	// Chain: after a re-float, SentAt2 is a later post-send stamp; a mark before it must still not refloat.
	postSendStamp2 := now
	now2 := postSendStamp2.Add(3 * time.Second)
	markBeforeStamp2 := postSendStamp2.Add(-500 * time.Millisecond)
	e2 := newEntry(1, 0, 43, tickNow, postSendStamp2, postSendStamp2, postSendStamp2)
	if got := decideBusyAction(e2, true, markBeforeStamp2, false, now2); got == busyRefloat {
		t.Errorf("f29 E (chain): after a re-float, a mark before the fresh stamp must NOT refloat again, got %v", got)
	}

	// Positive control: a genuine outbound message AFTER the post-send stamp still triggers a legit refloat.
	markAfterStamp := postSendStamp.Add(500 * time.Millisecond) // > SentAt
	if got := decideBusyAction(e, true, markAfterStamp, false, now); got != busyRefloat {
		t.Errorf("f29 E: a mark created AFTER the send must still refloat (the fix must not over-suppress), got %v", got)
	}
}

// TestMatches401 covers the f29 D 401 signature matcher: each signature, both together (same and separate
// lines), the negative, dedup of a repeated line, and the tail bound (a 401 above the last N lines does
// NOT match — it self-scopes to the current tail so a historical/scrolled-up 401 is not re-reported).
func TestMatches401(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want []string
	}{
		{"login-signature", "some output\nPlease run /login\n", []string{"Please run /login"}},
		{"revoked-signature", "x\n401 OAuth access token has been revoked\ny", []string{"401 OAuth access token has been revoked"}},
		{"both-separate-lines", "401 OAuth access token has been revoked\nPlease run /login", []string{"401 OAuth access token has been revoked", "Please run /login"}},
		{"both-same-line", "Error: 401 OAuth access token has been revoked. Please run /login", []string{"Error: 401 OAuth access token has been revoked. Please run /login"}},
		{"no-401", "all good\nrunning fine\ndone", nil},
		{"dedup-repeated", "Please run /login\nPlease run /login", []string{"Please run /login"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matches401(tc.pane); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("matches401(%q) = %v, want %v", tc.pane, got, tc.want)
			}
		})
	}
	// Tail bound: a 401 far ABOVE the last tail401Lines must NOT match (historical, scrolled up).
	var sb strings.Builder
	sb.WriteString("Please run /login\n")
	for i := 0; i < tail401Lines+5; i++ {
		fmt.Fprintf(&sb, "filler line %d\n", i)
	}
	if got := matches401(sb.String()); len(got) != 0 {
		t.Errorf("a 401 above the last %d lines must NOT match (historical); got %v", tail401Lines, got)
	}
}

// TestShouldWarn401OnTransition covers the transition predicate: warn ONLY on true→false.
func TestShouldWarn401OnTransition(t *testing.T) {
	if shouldWarn401OnTransition(false, true) {
		t.Error("false→true (rearm) must not warn")
	}
	if shouldWarn401OnTransition(true, true) {
		t.Error("true→true (still busy) must not warn")
	}
	if !shouldWarn401OnTransition(true, false) {
		t.Error("true→false (busy→idle) MUST warn")
	}
	if shouldWarn401OnTransition(false, false) {
		t.Error("false→false (still idle) must not warn — once-per-stall dedup")
	}
}

// TestTransition401_OncePerStall simulates the runBusyTick prevRunning update over a
// busy→idle→idle→resume→idle sequence: the warn fires exactly once per busy→idle edge (dedup while idle,
// rearm on resume), and a dead target is cleaned from the map.
func TestTransition401_OncePerStall(t *testing.T) {
	prev := map[string]bool{}
	target := "%3"
	warns := 0
	// feed mirrors the inline runBusyTick logic: warn on transition, then record the new state.
	feed := func(running bool) {
		if shouldWarn401OnTransition(prev[target], running) {
			warns++
		}
		prev[target] = running
	}
	feed(true)  // first sight: absent(false)→true → rearm, no warn
	feed(false) // busy→idle → warn (1)
	feed(false) // still idle → dedup, no warn
	feed(false) // still idle → dedup, no warn
	feed(true)  // resume: idle→busy → rearm, no warn
	feed(false) // busy→idle (new stall) → warn (2)
	if warns != 2 {
		t.Errorf("expected 2 warns (one per stall), got %d", warns)
	}
	// Dead-target cleanup: a target absent from the live set is dropped from the map.
	seen := map[string]bool{} // target no longer live this tick
	for t2 := range prev {
		if !seen[t2] {
			delete(prev, t2)
		}
	}
	if _, ok := prev[target]; ok {
		t.Errorf("dead target %q must be cleaned from the transition map", target)
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
		got := statusText("mybot", tc.elapsed)
		if got != tc.want {
			t.Errorf("statusText(%v) = %q, want %q", tc.elapsed, got, tc.want)
		}
	}
}
