package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
)

// f25 SendBelowSinceTool placement-signal tests (a-g). The compact-tool cycle reset must fire iff a NEW TG text
// message was (or will be) SENT below the last tool notification since the last tool — NOT on any-text. These
// tests exercise that signal at the flush level with a nil Bot: the Message-FIFO worker is GATED (gateWorker) so
// render ops never run and the bot is never dereferenced; only the synchronously-committed p3 placement state is
// read. (g) uses the extracted commitPositioned helper for a fully deterministic lifecycle-revalidation seam.

// newSendBelowBS builds a BotState with a nil Bot and a hermetic config dir (so the chunk budget is not read
// from the real user config).
func newSendBelowBS(t *testing.T) *BotState {
	t.Helper()
	old := config.ConfigDir
	config.ConfigDir = t.TempDir()
	t.Cleanup(func() { config.ConfigDir = old })
	return &types.BotState{Streams: stores.NewStreamStore(), MessageQueue: stores.NewSessionEventStore(), MsgIDMap: stores.NewMsgIDMap()}
}

// gateWorker parks the session's Message-FIFO worker on a never-closed channel so every subsequently-enqueued
// render op stays queued and NEVER runs (a nil Bot is never dereferenced). The worker goroutine is intentionally
// leaked for the test's lifetime — releasing the gate would let a render op run against the nil Bot.
func gateWorker(t *testing.T, bs *BotState, sid string) {
	t.Helper()
	parked := make(chan struct{})
	blocked := make(chan struct{})
	bs.MessageQueue.DispatchAsync(sid, "test:gate", func() error {
		close(parked)
		<-blocked // never closed — park the worker so queued render ops never execute
		return nil
	})
	<-parked // the worker is now parked inside the handler; the queue is empty
}

// readPositioned reads an entry's PositionedChunks under DataMu (-1 if the entry is gone).
func readPositioned(bs *BotState, sid, mid string) int {
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	defer ss.DataMu.Unlock()
	if e := ss.Msgs[mid]; e != nil {
		return e.PositionedChunks
	}
	return -1
}

// (a) A new COMPLETE bubble flushed once sets the placement signal and advances PositionedChunks 0 -> 1.
func TestSendBelow_NewBubble_SetsSignal(t *testing.T) {
	bs := newSendBelowBS(t)
	sid := "sa"
	gateWorker(t, bs, sid)
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "hello", true)
	if got := readPositioned(bs, sid, "m1"); got != 0 {
		t.Fatalf("(a) before flush PositionedChunks must be 0, got %d", got)
	}
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("(a) flushSession must enqueue a render op for a renderable bubble")
	}
	if !bs.Streams.TakeSendBelowSinceTool(sid) {
		t.Fatal("(a) a new bubble sent below must set SendBelowSinceTool")
	}
	if got := readPositioned(bs, sid, "m1"); got != 1 {
		t.Fatalf("(a) PositionedChunks must be 1 after enqueue, got %d", got)
	}
}

// (b) B1 REGRESSION (dual-FIFO lag): the render op is GATED (never completes) between the two flushes, yet the
// second flush reads the ENQUEUE-time PositionedChunks (committed in flush1 p3 under FlushMu). A fitting
// continuation gives planned == positioned -> NO signal. If PositionedChunks advanced at render COMPLETION
// instead, flush2 would read a stale 0 and re-split (the codexnote B1 bug).
func TestSendBelow_DualFifoLag_FittingContinuation_NoResplit(t *testing.T) {
	bs := newSendBelowBS(t)
	sid := "sb"
	gateWorker(t, bs, sid)
	// An INCOMPLETE first delta so AssembledText keeps reading the continuation (a final index-0 would stop there).
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "hello", false)
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("(b) flush1 must enqueue")
	}
	if !bs.Streams.TakeSendBelowSinceTool(sid) {
		t.Fatal("(b) flush1 (first placement) must set SendBelowSinceTool")
	}
	if got := readPositioned(bs, sid, "m1"); got != 1 {
		t.Fatalf("(b) after flush1 PositionedChunks must be 1, got %d", got)
	}
	// A FITTING continuation on the same (still-unsealed, render gated) bubble — one chunk still.
	handled, dropped, _ := bs.Streams.AppendExisting(sid, "m1", "", 1, " world", false)
	if !handled || dropped {
		t.Fatalf("(b) continuation must append, got handled=%v dropped=%v", handled, dropped)
	}
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("(b) flush2 must enqueue")
	}
	if bs.Streams.TakeSendBelowSinceTool(sid) {
		t.Fatal("(b) a fitting continuation (planned == positioned) must NOT set SendBelowSinceTool — the false split fix")
	}
	if got := readPositioned(bs, sid, "m1"); got != 1 {
		t.Fatalf("(b) after flush2 PositionedChunks must stay 1, got %d", got)
	}
}

// (c) A never-positioned bubble (missing index 0) yields no renderable job and no signal; once index 0 fills, a
// flush sends the first message below and sets the signal.
func TestSendBelow_MissingIndexZero_ThenFill(t *testing.T) {
	bs := newSendBelowBS(t)
	sid := "sc"
	gateWorker(t, bs, sid)
	// index 1 only -> Deltas[0] absent -> AssembledText empty.
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 1, "second", false)
	if flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("(c) a bubble missing index 0 must NOT be renderable (flushSession returns false)")
	}
	if bs.Streams.TakeSendBelowSinceTool(sid) {
		t.Fatal("(c) an empty (never-positioned) bubble must NOT set SendBelowSinceTool")
	}
	if got := readPositioned(bs, sid, "m1"); got != 0 {
		t.Fatalf("(c) PositionedChunks must stay 0 while unpositioned, got %d", got)
	}
	// Fill index 0 -> now assembled -> renderable -> first send below.
	handled, dropped, _ := bs.Streams.AppendExisting(sid, "m1", "", 0, "first ", false)
	if !handled || dropped {
		t.Fatalf("(c) index-0 fill must append, got handled=%v dropped=%v", handled, dropped)
	}
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("(c) filled bubble must be renderable")
	}
	if !bs.Streams.TakeSendBelowSinceTool(sid) {
		t.Fatal("(c) the first send of a filled bubble must set SendBelowSinceTool")
	}
	if got := readPositioned(bs, sid, "m1"); got != 1 {
		t.Fatalf("(c) PositionedChunks must be 1 after the first placement, got %d", got)
	}
}

// (d) Pagination growth: a real prior flush positions the bubble; a continuation that grows the plan past the
// chunk budget makes planned > positioned -> signal true. A small RichMaxRunes keeps the strings modest.
func TestSendBelow_PaginationGrowth_SetsSignal(t *testing.T) {
	bs := newSendBelowBS(t)
	if err := os.WriteFile(filepath.Join(config.GetConfigDir(), "config.json"), []byte(`{"richMaxRunes":1200}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	sid := "sd"
	gateWorker(t, bs, sid)
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, strings.Repeat("a", 300), false)
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("(d) flush1 must enqueue")
	}
	bs.Streams.TakeSendBelowSinceTool(sid) // consume the first-placement signal
	p1 := readPositioned(bs, sid, "m1")
	if p1 < 1 {
		t.Fatalf("(d) flush1 must position at least 1 chunk, got %d", p1)
	}
	// A continuation that grows the assembled text well past the budget -> more chunks.
	handled, dropped, _ := bs.Streams.AppendExisting(sid, "m1", "", 1, strings.Repeat("b", 2000), false)
	if !handled || dropped {
		t.Fatalf("(d) continuation must append, got handled=%v dropped=%v", handled, dropped)
	}
	if !flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("(d) flush2 must enqueue")
	}
	if !bs.Streams.TakeSendBelowSinceTool(sid) {
		t.Fatal("(d) pagination growth (planned > positioned) must set SendBelowSinceTool")
	}
	if p2 := readPositioned(bs, sid, "m1"); p2 <= p1 {
		t.Fatalf("(d) PositionedChunks must grow past %d, got %d", p1, p2)
	}
}

// (e) A sealed entry yields no renderable job -> no placement signal, no PositionedChunks commit. And an
// AppendExisting on the sealed entry is dropped, setting neither flag.
func TestSendBelow_SealedDrop_NeitherFlag(t *testing.T) {
	bs := newSendBelowBS(t)
	sid := "se"
	gateWorker(t, bs, sid)
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "done", true)
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	ss.Msgs["m1"].Sealed = true
	ss.DataMu.Unlock()
	bs.Streams.TakeNewTextSinceTool(sid) // clear the any-text flag the AppendDelta set
	if flushSession(bs, sid, false, false, flushAsync) {
		t.Fatal("(e) a sealed-only session must not be renderable (flushSession returns false)")
	}
	if bs.Streams.TakeSendBelowSinceTool(sid) {
		t.Fatal("(e) a sealed entry must not set SendBelowSinceTool")
	}
	if got := readPositioned(bs, sid, "m1"); got != 0 {
		t.Fatalf("(e) sealed entry must not get a PositionedChunks commit, got %d", got)
	}
	handled, dropped, _ := bs.Streams.AppendExisting(sid, "m1", "", 1, " late", false)
	if !handled || !dropped {
		t.Fatalf("(e) AppendExisting on a sealed entry must drop, got handled=%v dropped=%v", handled, dropped)
	}
	if bs.Streams.TakeNewTextSinceTool(sid) {
		t.Fatal("(e) a dropped AppendExisting must not set NewTextSinceTool")
	}
	if bs.Streams.TakeSendBelowSinceTool(sid) {
		t.Fatal("(e) a dropped AppendExisting must not set SendBelowSinceTool")
	}
}

// (f) flushTry on a FULL session queue does not commit (PositionedChunks unchanged, no signal, Dirty re-marked);
// when the queue has room the same flushTry commits both. Two gated sessions model full-vs-room deterministically
// (the gate keeps render ops from ever running; a same-session drain-retry is impossible without releasing the
// gate, which would deref the nil Bot).
func TestSendBelow_FlushTry_FullQueueNoCommit_RoomCommits(t *testing.T) {
	// full -> miss
	bs := newSendBelowBS(t)
	sid := "sf-full"
	gateWorker(t, bs, sid)
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "hello", true)
	// Fill the parked worker's queue to its cap.
	for bs.MessageQueue.TryDispatchAsync(sid, "fill", func() error { return nil }) {
	}
	if flushSession(bs, sid, false, false, flushTry) {
		t.Fatal("(f) flushTry on a full queue must return false (no enqueue)")
	}
	if bs.Streams.TakeSendBelowSinceTool(sid) {
		t.Fatal("(f) a full-queue miss must NOT set SendBelowSinceTool")
	}
	if got := readPositioned(bs, sid, "m1"); got != 0 {
		t.Fatalf("(f) a full-queue miss must NOT advance PositionedChunks, got %d", got)
	}
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	dirty := ss.Msgs["m1"].Dirty
	ss.DataMu.Unlock()
	if !dirty {
		t.Fatal("(f) a full-queue miss must re-mark the entry Dirty for retry")
	}

	// room -> commit
	bs2 := newSendBelowBS(t)
	sid2 := "sf-room"
	gateWorker(t, bs2, sid2)
	bs2.Streams.AppendDelta(sid2, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "hello", true)
	if !flushSession(bs2, sid2, false, false, flushTry) {
		t.Fatal("(f) flushTry with queue room must enqueue")
	}
	if !bs2.Streams.TakeSendBelowSinceTool(sid2) {
		t.Fatal("(f) a successful flushTry enqueue must set SendBelowSinceTool")
	}
	if got := readPositioned(bs2, sid2, "m1"); got != 1 {
		t.Fatalf("(f) a successful flushTry enqueue must commit PositionedChunks=1, got %d", got)
	}
}

// (g) Lifecycle revalidation: if the entry is Rotated away between the p1 snapshot and the p3 commit,
// commitPositioned must skip it — no stale signal, no commit on the dead entry (B2). Exercised at the extracted
// p3 helper for a fully deterministic seam (fablenote-approved fallback to the queue-block timing).
func TestSendBelow_LifecycleRevalidation_RotatedEntry_NoStaleCommit(t *testing.T) {
	bs := newSendBelowBS(t)
	sid := "sg"
	bs.Streams.AppendDelta(sid, stores.StreamMeta{MessageID: "m1", ChatID: 1}, 0, "hello", true)
	ss := bs.Streams.Session(sid)
	ss.DataMu.Lock()
	e := ss.Msgs["m1"]
	ss.DataMu.Unlock()
	// A snapshotted job that WOULD send below (planned 1 > positioned 0).
	jobs := []flushJob{{e: e, mid: "m1", chunks: []string{"hello"}, positionedAtSnapshot: 0, willSendBelow: true}}
	// The session resets (Rotate removes the entry) between the p1 release and the p3 commit.
	bs.Streams.Rotate(sid)
	commitPositioned(ss, jobs)
	if bs.Streams.TakeSendBelowSinceTool(sid) {
		t.Fatal("(g) a rotated-away entry must NOT set SendBelowSinceTool (stale signal)")
	}
	if e.PositionedChunks != 0 {
		t.Fatalf("(g) a rotated-away entry must NOT get a PositionedChunks commit, got %d", e.PositionedChunks)
	}
}
