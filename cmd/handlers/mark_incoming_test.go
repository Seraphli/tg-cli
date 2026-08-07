package handlers

import (
	"errors"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	tele "gopkg.in/telebot.v3"
)

// fakeReplyCtx is a minimal tele.Context stub exposing only the methods the marking seam touches
// (Chat/Message/Reply/Send). Embedding a nil tele.Context is safe because the tests never call any
// other method. replyErr/sendErr control the simulated send outcome.
type fakeReplyCtx struct {
	tele.Context
	chat     *tele.Chat
	msg      *tele.Message
	replyErr error
	sendErr  error
	replies  int
	sends    int
}

func (f *fakeReplyCtx) Chat() *tele.Chat       { return f.chat }
func (f *fakeReplyCtx) Message() *tele.Message { return f.msg }
func (f *fakeReplyCtx) Reply(what interface{}, opts ...interface{}) error {
	f.replies++
	return f.replyErr
}
func (f *fakeReplyCtx) Send(what interface{}, opts ...interface{}) error {
	f.sends++
	return f.sendErr
}

// TestMarkIncoming verifies markIncoming marks ONLY on entry (the deferred Mark was removed). now()
// is called exactly once and the marker holds the entry timestamp (base+1s).
func TestMarkIncoming(t *testing.T) {
	m := helpers.NewFloatMarkerStore()

	callCount := 0
	base := time.Now()
	now := func() time.Time {
		callCount++
		return base.Add(time.Duration(callCount) * time.Second)
	}

	var midCallCount int
	nextFn := func() error {
		midCallCount = callCount // should be 1 (entry Mark already called)
		return nil
	}

	if err := markIncoming(m, 10, 0, now, nextFn); err != nil {
		t.Fatalf("markIncoming returned error: %v", err)
	}
	// now() was called exactly once (entry only — no deferred Mark).
	if callCount != 1 {
		t.Errorf("now() called %d times, want 1 (deferred Mark must be gone)", callCount)
	}
	// next ran after the entry Mark (callCount==1 inside next).
	if midCallCount != 1 {
		t.Errorf("next ran at callCount=%d, want 1 (entry Mark not first)", midCallCount)
	}
	// The marker holds the ENTRY (t=base+1s) timestamp.
	got := m.LastMark(10, 0)
	want := base.Add(1 * time.Second)
	if !got.Equal(want) {
		t.Errorf("LastMark = %v, want %v (entry Mark only)", got, want)
	}
}

// TestMarkIncoming_EntryMarkBeforeNext verifies the entry Mark is applied before next runs.
func TestMarkIncoming_EntryMarkBeforeNext(t *testing.T) {
	m := helpers.NewFloatMarkerStore()
	base := time.Now()
	call := 0
	now := func() time.Time {
		call++
		return base.Add(time.Duration(call) * time.Second)
	}

	var afterEntry time.Time
	nextFn := func() error {
		// Capture the marker right after entry Mark (call==1 has fired).
		afterEntry = m.LastMark(10, 0)
		return nil
	}
	markIncoming(m, 10, 0, now, nextFn)

	// Entry Mark (t=base+1s) must have been applied before next ran.
	if afterEntry.IsZero() {
		t.Error("entry Mark was not applied before next() ran")
	}
}

// --- codexnote's four re-float timelines ---
// The busy manager re-floats iff LastMark.After(SentAt) (bot_busy.go:124). Each timeline drives
// markIncoming (+ the decorated Reply seam) with a controllable clock, then asserts that predicate.

// Timeline 1 (the boss bug): a slow handler that sends NOTHING while a status is created between
// entry and return. The marker must hold ONLY the entry stamp (< SentAt) → NO re-float.
func TestRefloatTimeline_SlowNoSendHandler(t *testing.T) {
	m := helpers.NewFloatMarkerStore()
	base := time.Now()
	cur := base
	now := func() time.Time { return cur }

	// Entry Mark at T0.
	var sentAt time.Time
	nextFn := func() error {
		// Status is created AFTER entry (the 1s busy loop), and the handler returns without sending.
		sentAt = base.Add(1 * time.Second)
		cur = base.Add(2 * time.Second) // time advances, but nothing is sent below the status
		return nil
	}
	markIncoming(m, 10, 0, now, nextFn)

	if m.LastMark(10, 0).After(sentAt) {
		t.Errorf("LastMark %v After SentAt %v — spurious re-float (the boss bug)", m.LastMark(10, 0), sentAt)
	}
}

// Timeline 2: the handler performs a SUCCESSFUL decorated Reply AFTER SentAt → marker advances past
// SentAt → re-float (a real message landed below the status).
func TestRefloatTimeline_SuccessfulReplyAfterStatus(t *testing.T) {
	m := helpers.NewFloatMarkerStore()
	base := time.Now()
	cur := base
	now := func() time.Time { return cur }

	fc := &fakeReplyCtx{chat: &tele.Chat{ID: 10}, msg: &tele.Message{ThreadID: 0}, replyErr: nil}
	mc := &markingContext{Context: fc, marker: m, now: now}

	sentAt := base.Add(1 * time.Second)
	nextFn := func() error {
		cur = base.Add(2 * time.Second) // handler replies after the status was created
		return mc.Reply("hello")
	}
	markIncoming(m, 10, 0, now, nextFn)

	if fc.replies != 1 {
		t.Fatalf("decorated Reply called %d times, want 1", fc.replies)
	}
	if !m.LastMark(10, 0).After(sentAt) {
		t.Errorf("LastMark %v NOT After SentAt %v — should re-float after a real reply", m.LastMark(10, 0), sentAt)
	}
}

// Timeline 3: a pre-existing status (SentAt in the past) + an incoming no-reply message → the entry
// Mark postdates SentAt → re-float (the message legitimately landed below the old status).
func TestRefloatTimeline_IncomingBelowOldStatus(t *testing.T) {
	m := helpers.NewFloatMarkerStore()
	base := time.Now()
	sentAt := base // status already exists at T0
	cur := base.Add(1 * time.Second)
	now := func() time.Time { return cur }

	markIncoming(m, 10, 0, now, func() error { return nil })

	if !m.LastMark(10, 0).After(sentAt) {
		t.Errorf("LastMark %v NOT After SentAt %v — an incoming message below an old status should re-float", m.LastMark(10, 0), sentAt)
	}
}

// Timeline 4: a FAILED decorated Reply → markOnSuccess does NOT mark → marker stays at the entry
// stamp (< SentAt) → NO re-float (nothing was actually delivered).
func TestRefloatTimeline_FailedReplyDoesNotMark(t *testing.T) {
	m := helpers.NewFloatMarkerStore()
	base := time.Now()
	cur := base
	now := func() time.Time { return cur }

	fc := &fakeReplyCtx{chat: &tele.Chat{ID: 10}, msg: &tele.Message{ThreadID: 0}, replyErr: errors.New("send failed")}
	mc := &markingContext{Context: fc, marker: m, now: now}

	sentAt := base.Add(1 * time.Second)
	nextFn := func() error {
		cur = base.Add(2 * time.Second)
		return mc.Reply("hello") // fails
	}
	markIncoming(m, 10, 0, now, nextFn)

	if fc.replies != 1 {
		t.Fatalf("decorated Reply called %d times, want 1", fc.replies)
	}
	if m.LastMark(10, 0).After(sentAt) {
		t.Errorf("LastMark %v After SentAt %v — a FAILED reply must not mark (no re-float)", m.LastMark(10, 0), sentAt)
	}
}

// --- nil-guard tests ---

// markOnSuccess must not mark and must not panic when Chat() or Message() is nil, or on error.
func TestMarkOnSuccess_NilGuards(t *testing.T) {
	m := helpers.NewFloatMarkerStore()
	now := func() time.Time { return time.Now() }

	// nil Chat → no mark.
	fcNoChat := &fakeReplyCtx{chat: nil, msg: &tele.Message{ThreadID: 0}}
	if err := markOnSuccess(m, fcNoChat, now, nil); err != nil {
		t.Errorf("markOnSuccess(nil chat) err = %v, want nil", err)
	}
	if !m.LastMark(10, 0).IsZero() {
		t.Error("markOnSuccess marked despite nil Chat")
	}

	// nil Message → no mark.
	fcNoMsg := &fakeReplyCtx{chat: &tele.Chat{ID: 10}, msg: nil}
	if err := markOnSuccess(m, fcNoMsg, now, nil); err != nil {
		t.Errorf("markOnSuccess(nil msg) err = %v, want nil", err)
	}
	if !m.LastMark(10, 0).IsZero() {
		t.Error("markOnSuccess marked despite nil Message")
	}

	// non-nil error → no mark, error passed through.
	fc := &fakeReplyCtx{chat: &tele.Chat{ID: 10}, msg: &tele.Message{ThreadID: 0}}
	sendErr := errors.New("boom")
	if err := markOnSuccess(m, fc, now, sendErr); err != sendErr {
		t.Errorf("markOnSuccess(err) = %v, want passthrough %v", err, sendErr)
	}
	if !m.LastMark(10, 0).IsZero() {
		t.Error("markOnSuccess marked despite send error")
	}

	// success → marks.
	base := time.Now()
	nowFixed := func() time.Time { return base }
	if err := markOnSuccess(m, fc, nowFixed, nil); err != nil {
		t.Errorf("markOnSuccess(success) err = %v, want nil", err)
	}
	if !m.LastMark(10, 0).Equal(base) {
		t.Errorf("markOnSuccess(success) LastMark = %v, want %v", m.LastMark(10, 0), base)
	}
}

// markToOnSuccess must not mark and must not panic on nil to / nil to.Chat / error.
func TestMarkToOnSuccess_NilGuards(t *testing.T) {
	m := helpers.NewFloatMarkerStore()
	now := func() time.Time { return time.Now() }

	// nil to → no mark, no panic.
	if err := markToOnSuccess(m, now, nil, nil); err != nil {
		t.Errorf("markToOnSuccess(nil to) err = %v, want nil", err)
	}
	// nil to.Chat → no mark, no panic.
	if err := markToOnSuccess(m, now, &tele.Message{Chat: nil}, nil); err != nil {
		t.Errorf("markToOnSuccess(nil to.Chat) err = %v, want nil", err)
	}
	// non-nil error → no mark, passthrough.
	sendErr := errors.New("boom")
	if err := markToOnSuccess(m, now, &tele.Message{Chat: &tele.Chat{ID: 10}}, sendErr); err != sendErr {
		t.Errorf("markToOnSuccess(err) = %v, want passthrough %v", err, sendErr)
	}
	if !m.LastMark(10, 0).IsZero() {
		t.Error("markToOnSuccess marked despite nil/error inputs")
	}

	// success → marks.
	base := time.Now()
	nowFixed := func() time.Time { return base }
	if err := markToOnSuccess(m, nowFixed, &tele.Message{Chat: &tele.Chat{ID: 10}, ThreadID: 0}, nil); err != nil {
		t.Errorf("markToOnSuccess(success) err = %v, want nil", err)
	}
	if !m.LastMark(10, 0).Equal(base) {
		t.Errorf("markToOnSuccess(success) LastMark = %v, want %v", m.LastMark(10, 0), base)
	}
}
