package cmd

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	tele "gopkg.in/telebot.v3"
)

// waitUntil polls pred within a timeout; fails the test if it never holds. Deterministic synchronization
// helper for the dual-FIFO tests (avoids fixed sleeps racing).
func waitUntil(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// TestOrderingUnderMessageFIFOStall (S14 ordering-under-FloodError): the FIRST Message-FIFO send op stalls
// (parked on a channel, simulating a FloodError 5s backoff sleep), yet (a) the Hook FIFO keeps admitting
// ingress jobs within the 128 buffer (a full stall does NOT block hook ingress), and (b) once the stall
// releases the Message FIFO delivers the ops in Hook-FIFO enqueue order: text1 -> tool -> text2.
func TestOrderingUnderMessageFIFOStall(t *testing.T) {
	hookFIFO := stores.NewSessionEventStore()
	msgFIFO := stores.NewSessionEventStore()
	sid := "sess"
	var mu sync.Mutex
	var sendOrder []string
	stall := make(chan struct{})
	// Enqueue three Message-FIFO ops in Hook-FIFO order: text1 (stalls), tool, text2.
	msgFIFO.DispatchAsync(sid, "msg:text1", func() error {
		<-stall // simulate a FloodError backoff sleep on the first send
		mu.Lock()
		sendOrder = append(sendOrder, "text1")
		mu.Unlock()
		return nil
	})
	msgFIFO.DispatchAsync(sid, "msg:tool", func() error {
		mu.Lock()
		sendOrder = append(sendOrder, "tool")
		mu.Unlock()
		return nil
	})
	msgFIFO.DispatchAsync(sid, "msg:text2", func() error {
		mu.Lock()
		sendOrder = append(sendOrder, "text2")
		mu.Unlock()
		return nil
	})
	// While the Message FIFO is stalled, the Hook FIFO must keep admitting ingress jobs (within the 128 cap).
	// A blocking Dispatch of many quick hook jobs must complete promptly — ingress is NOT blocked by the stall.
	var hookRan atomic.Int64
	admitted := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			hookFIFO.DispatchAsync(sid, "hook:ingress", func() error { hookRan.Add(1); return nil })
		}
		close(admitted)
	}()
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		close(stall)
		t.Fatal("Hook FIFO ingress blocked while the Message FIFO stalled")
	}
	waitUntil(t, 2*time.Second, func() bool { return hookRan.Load() == 100 })
	// Now release the stall — the Message FIFO drains the three ops in enqueue order.
	close(stall)
	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sendOrder) == 3
	})
	mu.Lock()
	got := strings.Join(sendOrder, ",")
	mu.Unlock()
	if got != "text1,tool,text2" {
		t.Fatalf("Message FIFO must preserve Hook-FIFO order text1->tool->text2, got %q", got)
	}
}

// TestSnapshotOrdering (S14e): two overlapping flushSession callers each enqueue ONE render op; the render
// ops run on the Message FIFO in the order flushSession enqueued them (FlushMu serializes snapshot+enqueue,
// so a later caller cannot leapfrog an earlier one). Assert the two sessions' first sends preserve the
// enqueue order via a shared record; here we drive two entries on ONE session through two flush calls and
// assert the render op order equals the flush-call order.
func TestSnapshotOrdering(t *testing.T) {
	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s := string(b)
		mu.Lock()
		switch {
		case strings.Contains(s, "alpha"):
			order = append(order, "alpha")
		case strings.Contains(s, "bravo"):
			order = append(order, "bravo")
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1}}}`)
	}))
	defer srv.Close()
	bot, err := tele.NewBot(tele.Settings{Token: "test", URL: srv.URL, Offline: true, Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bs := &types.BotState{Bot: bot, Streams: stores.NewStreamStore(),
		MessageQueue: stores.NewSessionEventStore(), MsgIDMap: stores.NewMsgIDMap()}
	sid := "sess-snap"
	// First flush: one entry "alpha" — its render op is enqueued first.
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "alpha body", true)
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("first flush must enqueue a render op")
	}
	// Second flush: a new entry "bravo" (m1 is now sealed after the first render, so only bravo renders).
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m2", ChatID: 1}, 0, "bravo body", true)
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("second flush must enqueue a render op")
	}
	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 2
	})
	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "alpha,bravo" {
		t.Fatalf("render ops must run in flush-enqueue (snapshot) order, got %q", got)
	}
}

// TestTickerSealedStopRelabel (S14f): the ticker seals the last entry (💬, NOT Relabeled) via a normal
// (non-stop) flush; a subsequent Stop flush must relabel it to ✅ EXACTLY ONCE (the needRelabel snapshot
// exception renders the sealed-but-unrelabeled last entry once, then marks Relabeled so a repeat Stop is a no-op).
func TestTickerSealedStopRelabel(t *testing.T) {
	var mu sync.Mutex
	var sends, edits int
	var lastRelabel bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s := string(b)
		mu.Lock()
		if strings.Contains(r.URL.Path, "editMessageText") {
			edits++
			if strings.Contains(s, "\\u2705") || strings.Contains(s, "✅") {
				lastRelabel = true
			}
		} else {
			sends++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":77,"chat":{"id":1}}}`)
	}))
	defer srv.Close()
	bot, err := tele.NewBot(tele.Settings{Token: "test", URL: srv.URL, Offline: true, Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bs := &types.BotState{Bot: bot, Streams: stores.NewStreamStore(),
		MessageQueue: stores.NewSessionEventStore(), MsgIDMap: stores.NewMsgIDMap()}
	sid := "sess-relabel"
	// A COMPLETE MD stream (final=true) so the normal flush seals it (💬, not relabeled).
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "assistant text complete", true)
	// Normal (non-stop) flush via the Message FIFO: sends + seals the last entry, NOT relabeled.
	if !flushSession(bs, sid, false, false, flushSync) {
		t.Fatal("normal flush must enqueue a render op")
	}
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	sealed := ss.Msgs["m1"].Sealed
	relabeled := ss.Msgs["m1"].Relabeled
	ss.DataMu.Unlock()
	if !sealed || relabeled {
		t.Fatalf("after normal flush: want Sealed && !Relabeled, got sealed=%v relabeled=%v", sealed, relabeled)
	}
	mu.Lock()
	sendsAfterFlush := sends
	mu.Unlock()
	if sendsAfterFlush != 1 {
		t.Fatalf("normal flush must send the bubble once, got %d", sendsAfterFlush)
	}
	// Stop flush: relabel the sealed-but-unrelabeled last entry to ✅ EXACTLY ONCE (an edit, no new send).
	streamFlush(bs, sid, true)
	mu.Lock()
	editsAfterStop := edits
	sawRelabel := lastRelabel
	mu.Unlock()
	if editsAfterStop != 1 {
		t.Fatalf("Stop relabel must edit the last entry EXACTLY ONCE, got %d edits", editsAfterStop)
	}
	if !sawRelabel {
		t.Fatal("Stop relabel must set the ✅ finalized header")
	}
	ss.DataMu.Lock()
	relabeled2 := ss.Msgs["m1"].Relabeled
	ss.DataMu.Unlock()
	if !relabeled2 {
		t.Fatal("Stop must mark the entry Relabeled")
	}
	// A repeat Stop flush must be a no-op (already relabeled) — no extra edit.
	streamFlush(bs, sid, true)
	mu.Lock()
	editsAfterRepeat := edits
	mu.Unlock()
	if editsAfterRepeat != 1 {
		t.Fatalf("repeat Stop must not re-edit an already-relabeled entry, got %d edits", editsAfterRepeat)
	}
}

// TestLifecycleFence (S14g): a SLOW render op enqueued first, then a lifecycle-rotate op enqueued after it on
// the SAME Message FIFO — the Rotate runs AFTER the render completes (totally ordered, no render-after-clear
// race). The render sends to the pre-rotate entry; the rotate then clears the stream so no stray send targets
// a cleared entry.
func TestLifecycleFence(t *testing.T) {
	var mu sync.Mutex
	var events []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		mu.Lock()
		events = append(events, "send")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":9,"chat":{"id":1}}}`)
	}))
	defer srv.Close()
	bot, err := tele.NewBot(tele.Settings{Token: "test", URL: srv.URL, Offline: true, Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bs := &types.BotState{Bot: bot, Streams: stores.NewStreamStore(),
		MessageQueue: stores.NewSessionEventStore(), MsgIDMap: stores.NewMsgIDMap()}
	sid := "sess-fence"
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "text before rotate", true)
	// Enqueue the SLOW render op first (async), then the lifecycle-rotate op — same Message FIFO, so rotate
	// runs strictly after render finishes. Wrap the render enqueue so it records "render" ordering.
	renderGate := make(chan struct{})
	bs.MessageQueue.DispatchAsync(sid, "msg:slow-render", func() error {
		<-renderGate // hold the render op so the rotate op is guaranteed queued behind it
		mu.Lock()
		events = append(events, "render")
		mu.Unlock()
		// Do a real send to the pre-rotate entry.
		bs.Bot.Send(&tele.Chat{ID: 1}, "x")
		return nil
	})
	rotated := make(chan struct{})
	bs.MessageQueue.DispatchAsync(sid, "msg:lifecycle-rotate", func() error {
		mu.Lock()
		events = append(events, "rotate")
		mu.Unlock()
		bs.Streams.Rotate(sid)
		close(rotated)
		return nil
	})
	// Release the render; the rotate must observe the render-then-rotate order.
	close(renderGate)
	select {
	case <-rotated:
	case <-time.After(2 * time.Second):
		t.Fatal("rotate op never ran")
	}
	mu.Lock()
	got := strings.Join(events, ",")
	mu.Unlock()
	if got != "render,send,rotate" && got != "render,rotate" {
		t.Fatalf("Rotate must run AFTER the render op (render then rotate), got %q", got)
	}
	// The stream must be cleared by the rotate (no stray entry survives).
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	n := len(ss.Order)
	ss.DataMu.Unlock()
	if n != 0 {
		t.Fatalf("Rotate must clear the stream, got %d entries", n)
	}
}

// TestStateOwnershipUnderStalledMessageFIFO (S14i): the Hook-FIFO-owned state mutations (ClearReactions /
// ToolUseMsgs.CleanupSession / resetCompactAndScheduleDelete's ResetAndTakeInternalID) run to completion even
// when the Message FIFO is stalled (parked on a blocked send op) — state ownership is on the Hook FIFO, not the
// Message FIFO (INV4/INV5). Assert the state mutations complete while the Message FIFO is full/stalled.
func TestStateOwnershipUnderStalledMessageFIFO(t *testing.T) {
	bs := &types.BotState{
		Streams: stores.NewStreamStore(), MessageQueue: stores.NewSessionEventStore(),
		MsgIDMap: stores.NewMsgIDMap(), ToolUseMsgs: stores.NewToolUseMsgStore(),
		CompactTools: stores.NewCompactToolStore(),
	}
	sid := "sess-owner"
	// Stall the Message FIFO: park its worker on a blocked op, then fill it to cap so any further send op blocks.
	stall := make(chan struct{})
	bs.MessageQueue.DispatchAsync(sid, "msg:block", func() error { <-stall; return nil })
	waitUntil(t, time.Second, func() bool { return bs.MessageQueue.QueueDepth(sid) == 0 })
	for i := 0; i < 128; i++ {
		bs.MessageQueue.DispatchAsync(sid, "msg:fill", func() error { <-stall; return nil })
	}
	// Seed Hook-FIFO-owned state: a compact entry (with an internal id) + a tool-use entry for this session.
	bs.CompactTools.Store(sid, &stores.CompactToolEntry{InternalID: bs.MsgIDMap.Allocate(), ChatID: 1})
	bs.ToolUseMsgs.Store("tu1", &stores.ToolUseMsgEntry{InternalID: bs.MsgIDMap.Allocate(), SessionID: sid})
	// These state mutations are Hook-FIFO / store-level — they must complete despite the stalled Message FIFO.
	// ResetAndTakeInternalID discards the compact entry (returns its id); CleanupSession drops the tool entry.
	oldID := bs.CompactTools.ResetAndTakeInternalID(sid)
	if oldID == 0 {
		t.Fatal("ResetAndTakeInternalID must return the discarded internal id")
	}
	if _, ok := bs.CompactTools.Get(sid); ok {
		t.Fatal("compact entry must be discarded even though the Message FIFO is stalled")
	}
	bs.ToolUseMsgs.CleanupSession(sid)
	if _, ok := bs.ToolUseMsgs.Get("tu1"); ok {
		t.Fatal("CleanupSession must drop the tool-use entry even though the Message FIFO is stalled")
	}
	close(stall)
}

// TestTickerCrossSession (S14j): session A's Message FIFO is full (stalled), yet a ticker-style TryDispatchAsync
// flush for session B still succeeds — a full session queue never stalls other sessions (per-session workers,
// MAJOR 3). The ticker uses TryDispatchAsync so A's full queue only fails A's own enqueue, never B's.
func TestTickerCrossSession(t *testing.T) {
	q := stores.NewSessionEventStore()
	// Fill session A to cap with a parked worker so its queue stays full.
	stall := make(chan struct{})
	q.DispatchAsync("A", "block", func() error { <-stall; return nil })
	waitUntil(t, time.Second, func() bool { return q.QueueDepth("A") == 0 })
	for i := 0; i < 128; i++ {
		q.DispatchAsync("A", "fill", func() error { <-stall; return nil })
	}
	// A ticker TryDispatchAsync for A must now FAIL (A is at cap).
	if q.TryDispatchAsync("A", "a-try", func() error { return nil }) {
		t.Fatal("session A at cap must reject a TryDispatchAsync flush")
	}
	// But session B (a DIFFERENT worker) must still accept the ticker flush and run it.
	bRan := make(chan struct{})
	if !q.TryDispatchAsync("B", "b-try", func() error { close(bRan); return nil }) {
		t.Fatal("session B must still accept a ticker flush while A is full")
	}
	select {
	case <-bRan:
	case <-time.After(2 * time.Second):
		t.Fatal("session B flush never ran while A was full")
	}
	close(stall)
}
