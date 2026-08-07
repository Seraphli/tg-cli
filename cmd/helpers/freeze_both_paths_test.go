package helpers

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	tele "gopkg.in/telebot.v3"
)

// freezeEditObserver is an httptest server that counts editMessageText calls and records the last request
// path, so a test can assert the freeze edit ran (exactly once) via the Message FIFO — never inline.
type freezeEditObserver struct {
	srv       *httptest.Server
	editCalls int32
	lastPath  atomic.Value // string
}

func newFreezeEditObserver(t *testing.T) *freezeEditObserver {
	t.Helper()
	o := &freezeEditObserver{}
	o.lastPath.Store("")
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := extractMethodFromPath(r.URL.Path)
		if method == "editMessageText" {
			atomic.AddInt32(&o.editCalls, 1)
			o.lastPath.Store(r.URL.Path)
		}
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, okResponse(555))
	}))
	return o
}

func (o *freezeEditObserver) bot(t *testing.T) *tele.Bot { return makeFakeBot(t, o.srv) }
func (o *freezeEditObserver) calls() int32               { return atomic.LoadInt32(&o.editCalls) }
func (o *freezeEditObserver) close()                     { o.srv.Close() }

// waitForEdits polls until the observer records `want` edit calls or the deadline elapses.
func waitForEdits(o *freezeEditObserver, want int32, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if o.calls() >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return o.calls() >= want
}

// registerAskQEntry registers an unresolved AskUserQuestion PendingWaitEntry so FreezeWaitEntryOnDesktop's
// ResolveIfUnresolved CAS wins and BuildFrozenMarkup produces a non-nil markup.
func registerAskQEntry(pw *stores.PendingWaitStore, uuid, sessionID string) stores.EntrySnapshot {
	e := &stores.PendingWaitEntry{
		UUID:      uuid,
		SessionID: sessionID,
		ToolName:  "AskUserQuestion",
		Questions: []stores.QuestionMeta{
			{QuestionText: "Confirm?", Header: "Confirmation", NumOptions: 2, OptionLabels: []string{"Yes", "No"}},
		},
	}
	pw.Register(e)
	snap, _ := pw.GetSnapshot(uuid)
	return snap
}

// TestFreezeWaitEntry_ImmediatePath_EnqueuesFreezeEdit verifies the coords-already-known (immediate) path:
// coords are Set before FreezeWaitEntryOnDesktop runs, so EditOrDefer fires the callback inline — but the
// callback ONLY enqueues a msg:freeze-edit op onto the Message FIFO (the edit I/O happens on the worker
// goroutine, NOT inline). The freeze edit runs exactly once.
func TestFreezeWaitEntry_ImmediatePath_EnqueuesFreezeEdit(t *testing.T) {
	o := newFreezeEditObserver(t)
	defer o.close()
	bot := o.bot(t)

	sessionID := "sess-immediate"
	pw := stores.NewPendingWaitStore()
	pms := stores.NewPendingMsgStore()
	mq := stores.NewSessionEventStore()

	snap := registerAskQEntry(pw, "uuid-imm", sessionID)
	// Coords already known: SetAndDrain BEFORE the freeze so EditOrDefer takes the immediate branch.
	pms.SetAndDrain(snap.UUID, 4242, 999, "original text", 0)

	// Block the Message FIFO worker with a sentinel op so we can observe that the freeze edit is ENQUEUED
	// (not run inline). The sentinel holds the worker until we release it.
	release := make(chan struct{})
	started := make(chan struct{})
	mq.DispatchAsync(sessionID, "sentinel", func() error {
		close(started)
		<-release
		return nil
	})
	<-started

	FreezeWaitEntryOnDesktop(bot, mq, pw, pms, snap, "❄️ frozen")

	// While the worker is blocked on the sentinel, the freeze edit must NOT have run inline.
	if got := o.calls(); got != 0 {
		t.Fatalf("freeze edit ran INLINE (before the Message FIFO drained): edit calls=%d, want 0", got)
	}
	// The op must be queued on the Message FIFO behind the sentinel.
	if d := mq.QueueDepth(sessionID); d < 1 {
		t.Fatalf("expected msg:freeze-edit queued on the Message FIFO, QueueDepth=%d", d)
	}

	// Release the worker; the freeze edit now runs on the FIFO worker goroutine.
	close(release)
	if !waitForEdits(o, 1, 2*time.Second) {
		t.Fatalf("freeze edit did not run after draining the Message FIFO: edit calls=%d", o.calls())
	}
	// Exactly once.
	time.Sleep(50 * time.Millisecond)
	if got := o.calls(); got != 1 {
		t.Fatalf("freeze edit must run exactly once; edit calls=%d", got)
	}
}

// TestFreezeWaitEntry_DeferredPath_EnqueuesFreezeEdit verifies the deferred-then-SetAndDrain path: no coords
// exist when FreezeWaitEntryOnDesktop runs, so EditOrDefer stores the callback; a later SetAndDrain drains
// it, and the drained callback ONLY enqueues a msg:freeze-edit op onto the Message FIFO. The freeze edit
// runs exactly once.
func TestFreezeWaitEntry_DeferredPath_EnqueuesFreezeEdit(t *testing.T) {
	o := newFreezeEditObserver(t)
	defer o.close()
	bot := o.bot(t)

	sessionID := "sess-deferred"
	pw := stores.NewPendingWaitStore()
	pms := stores.NewPendingMsgStore()
	mq := stores.NewSessionEventStore()

	snap := registerAskQEntry(pw, "uuid-def", sessionID)

	// No coords yet → FreezeWaitEntryOnDesktop stores a deferred callback and enqueues NOTHING.
	FreezeWaitEntryOnDesktop(bot, mq, pw, pms, snap, "❄️ frozen")
	if got := o.calls(); got != 0 {
		t.Fatalf("deferred path must not run the edit before SetAndDrain; edit calls=%d", got)
	}
	if d := mq.QueueDepth(sessionID); d != 0 {
		t.Fatalf("deferred path must enqueue nothing until SetAndDrain; QueueDepth=%d", d)
	}

	// Block the Message FIFO worker so we can observe the ENQUEUE that SetAndDrain triggers.
	release := make(chan struct{})
	started := make(chan struct{})
	mq.DispatchAsync(sessionID, "sentinel", func() error {
		close(started)
		<-release
		return nil
	})
	<-started

	// SetAndDrain fires the deferred callback → enqueues msg:freeze-edit (does NOT run the edit inline).
	pms.SetAndDrain(snap.UUID, 4242, 999, "original text", 0)
	if got := o.calls(); got != 0 {
		t.Fatalf("SetAndDrain ran the freeze edit INLINE; edit calls=%d, want 0", got)
	}
	if d := mq.QueueDepth(sessionID); d < 1 {
		t.Fatalf("SetAndDrain must enqueue msg:freeze-edit onto the Message FIFO; QueueDepth=%d", d)
	}

	close(release)
	if !waitForEdits(o, 1, 2*time.Second) {
		t.Fatalf("freeze edit did not run after draining the Message FIFO: edit calls=%d", o.calls())
	}
	time.Sleep(50 * time.Millisecond)
	if got := o.calls(); got != 1 {
		t.Fatalf("freeze edit must run exactly once; edit calls=%d", got)
	}
}

// TestFreezeWaitEntry_EditPayloadIsEditMessageText is a small guard that the enqueued op actually performs
// an editMessageText (the freeze edit I/O), confirming the observer's method classification is right.
func TestFreezeWaitEntry_EditPayloadIsEditMessageText(t *testing.T) {
	o := newFreezeEditObserver(t)
	defer o.close()
	bot := o.bot(t)

	sessionID := "sess-payload"
	pw := stores.NewPendingWaitStore()
	pms := stores.NewPendingMsgStore()
	mq := stores.NewSessionEventStore()

	snap := registerAskQEntry(pw, "uuid-pay", sessionID)
	pms.SetAndDrain(snap.UUID, 4242, 999, "original text", 0)
	FreezeWaitEntryOnDesktop(bot, mq, pw, pms, snap, "❄️ frozen")

	if !waitForEdits(o, 1, 2*time.Second) {
		t.Fatalf("freeze edit did not run: edit calls=%d", o.calls())
	}
	if path, _ := o.lastPath.Load().(string); extractMethodFromPath(path) != "editMessageText" {
		t.Fatalf("expected editMessageText, got path=%q", path)
	}
	// The winning ResolveIfUnresolved CAS means a second freeze attempt on the same entry is a no-op.
	FreezeWaitEntryOnDesktop(bot, mq, pw, pms, snap, "❄️ frozen again")
	time.Sleep(50 * time.Millisecond)
	if got := o.calls(); got != 1 {
		t.Fatalf("a second freeze on a resolved entry must NOT edit again; edit calls=%d", got)
	}
}
