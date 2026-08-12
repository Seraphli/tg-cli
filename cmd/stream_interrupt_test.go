package cmd

import (
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
)

// TestFlushInterruptedRerendersSealedBubble covers Round-4 Item 7's render seam: a SEALED (truncated) bubble is
// normally skipped by flushSession, but once MarkInterruptedRetry flags it the flush must re-render it ONCE to
// add the "🔄 Interrupted — retrying…" header — the same sealed-entry re-render exception as the Stop relabel.
// After the render op commits InterruptRendered the bubble is skipped again (one-shot). Uses the gated-worker
// seam (nil Bot): only flushSession's renderable decision is asserted, no TG I/O runs.
func TestFlushInterruptedRerendersSealedBubble(t *testing.T) {
	bs := newSendBelowBS(t)
	sid := "ir"
	gateWorker(t, bs, sid)
	// A sealed (truncated) bubble on turn t1.
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", TurnID: "t1", ChatID: 1}, 0, "a half sentence that got cut", true)
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	ss.Msgs["m1"].Sealed = true
	ss.DataMu.Unlock()
	bs.Streams.TakeNewTextSinceTool(sid) // clear the any-text flag AppendDelta set

	// Baseline: a sealed-only bubble is not renderable.
	if flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("a sealed-only bubble must not be renderable before the interrupt mark")
	}

	// Mark interrupted -> the sealed bubble becomes renderable (the needInterrupt re-render exception).
	if !bs.Streams.MarkInterruptedRetry(sid, "t1") {
		t.Fatal("MarkInterruptedRetry should mark the t1 bubble")
	}
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("an interrupted sealed bubble MUST be renderable (needInterrupt re-render exception)")
	}

	// The render op is gated (never runs), so simulate its post-render commit of InterruptRendered, then assert
	// the one-shot: the sealed bubble is skipped again.
	ss.DataMu.Lock()
	ss.Msgs["m1"].InterruptRendered = true
	ss.DataMu.Unlock()
	if flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("once InterruptRendered, the sealed bubble must be skipped again (one-shot)")
	}
}
