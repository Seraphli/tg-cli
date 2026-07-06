package stores

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestPendingMsgStore_SetAndDrain_ThenGet verifies that SetAndDrain stores coords
// and Get retrieves them correctly.
func TestPendingMsgStore_SetAndDrain_ThenGet(t *testing.T) {
	s := NewPendingMsgStore()
	s.SetAndDrain("u1", 42, 100, "hello", 5)
	msgID, chatID, msgText, topicID, ok := s.Get("u1")
	if !ok {
		t.Fatal("expected ok=true after SetAndDrain")
	}
	if msgID != 42 || chatID != 100 || msgText != "hello" || topicID != 5 {
		t.Fatalf("unexpected coords: msgID=%d chatID=%d msgText=%q topicID=%d", msgID, chatID, msgText, topicID)
	}
}

// TestPendingMsgStore_DeferFirst_ThenSetAndDrain verifies the deferred path:
// EditOrDefer stores a deferred fn when no coords exist; SetAndDrain drains it exactly once.
func TestPendingMsgStore_DeferFirst_ThenSetAndDrain(t *testing.T) {
	s := NewPendingMsgStore()
	var counter int32
	// EditOrDefer with no coords: fn NOT run yet, deferred stored, returns false.
	ran := s.EditOrDefer("u1", func(msgID int, chatID int64, msgText string, topicID int) {
		atomic.AddInt32(&counter, 1)
	})
	if ran {
		t.Fatal("expected EditOrDefer to return false (no coords yet)")
	}
	if atomic.LoadInt32(&counter) != 0 {
		t.Fatal("fn must not run before SetAndDrain")
	}
	// SetAndDrain drains the deferred fn, which runs exactly once.
	s.SetAndDrain("u1", 7, 200, "text", 0)
	if atomic.LoadInt32(&counter) != 1 {
		t.Fatalf("expected fn to run exactly once after SetAndDrain, got %d", atomic.LoadInt32(&counter))
	}
	// After drain, coords are NOT stored (the drain path skips storing).
	_, _, _, _, ok := s.Get("u1")
	if ok {
		t.Fatal("expected no stored coords after drained path")
	}
}

// TestPendingMsgStore_SetFirst_ThenEditOrDefer verifies reverse order:
// SetAndDrain first, then EditOrDefer runs immediately and returns true.
func TestPendingMsgStore_SetFirst_ThenEditOrDefer(t *testing.T) {
	s := NewPendingMsgStore()
	var counter int32
	s.SetAndDrain("u1", 9, 300, "msg", 1)
	// Coords exist: fn runs immediately, returns true, coords consumed.
	ran := s.EditOrDefer("u1", func(msgID int, chatID int64, msgText string, topicID int) {
		atomic.AddInt32(&counter, 1)
		if msgID != 9 || chatID != 300 || msgText != "msg" || topicID != 1 {
			panic("wrong coords passed to editFunc")
		}
	})
	if !ran {
		t.Fatal("expected EditOrDefer to return true when coords existed")
	}
	if atomic.LoadInt32(&counter) != 1 {
		t.Fatalf("expected fn called exactly once, got %d", atomic.LoadInt32(&counter))
	}
	// Coords consumed by EditOrDefer.
	_, _, _, _, ok := s.Get("u1")
	if ok {
		t.Fatal("expected coords consumed after EditOrDefer ran immediately")
	}
}

// TestPendingMsgStore_Delete_ClearsDeferred verifies that Delete clears any deferred fn;
// the fn never runs.
func TestPendingMsgStore_Delete_ClearsDeferred(t *testing.T) {
	s := NewPendingMsgStore()
	var counter int32
	s.EditOrDefer("u1", func(int, int64, string, int) { atomic.AddInt32(&counter, 1) })
	s.Delete("u1")
	if atomic.LoadInt32(&counter) != 0 {
		t.Fatal("deferred fn must not run after Delete")
	}
	// Tombstone: a subsequent SetAndDrain must not invoke any fn (entry is closed).
	s.SetAndDrain("u1", 1, 1, "x", 0)
	if atomic.LoadInt32(&counter) != 0 {
		t.Fatal("fn must not run even after SetAndDrain on closed uuid")
	}
}

// TestPendingMsgStore_ClosedTombstone_DiscardsEditOrDefer verifies that after Delete,
// any subsequent EditOrDefer is discarded: fn never runs, no deferred stored.
func TestPendingMsgStore_ClosedTombstone_DiscardsEditOrDefer(t *testing.T) {
	s := NewPendingMsgStore()
	var counter int32
	s.Delete("u1") // create tombstone without prior state
	ran := s.EditOrDefer("u1", func(int, int64, string, int) { atomic.AddInt32(&counter, 1) })
	if ran {
		t.Fatal("expected EditOrDefer to return false for closed uuid")
	}
	if atomic.LoadInt32(&counter) != 0 {
		t.Fatal("fn must not run for closed uuid")
	}
	// Even if SetAndDrain is called, fn stays silent because closed discards it too.
	s.SetAndDrain("u1", 5, 5, "y", 0)
	if atomic.LoadInt32(&counter) != 0 {
		t.Fatal("fn must not run for closed uuid even after SetAndDrain")
	}
}

// TestPendingMsgStore_Sweep_KeepsLive verifies that Sweep only removes closed tombstones
// and never touches live msgID entries.
func TestPendingMsgStore_Sweep_KeepsLive(t *testing.T) {
	s := NewPendingMsgStore()
	// Live entry: SetAndDrain without a deferred fn stores coords.
	s.SetAndDrain("live", 11, 111, "live-text", 0)
	// Closed entry: Delete creates a tombstone.
	s.Delete("closed")
	// Force the tombstone timestamp to epoch so Sweep(1s) expires it.
	s.mu.Lock()
	s.closed["closed"] = 0 // unix epoch — far in the past
	s.mu.Unlock()
	s.Sweep(time.Second)
	// Live entry must still be readable.
	msgID, _, _, _, ok := s.Get("live")
	if !ok {
		t.Fatal("live entry must not be removed by Sweep")
	}
	if msgID != 11 {
		t.Fatalf("expected msgID=11, got %d", msgID)
	}
}

// TestPendingMsgStore_Sweep_RemovesExpiredClosed verifies that Sweep removes expired
// closed tombstones while leaving unexpired ones alone.
func TestPendingMsgStore_Sweep_RemovesExpiredClosed(t *testing.T) {
	s := NewPendingMsgStore()
	s.Delete("old")
	// Force the tombstone timestamp far into the past so any positive ttl expires it.
	s.mu.Lock()
	s.closed["old"] = 0 // unix epoch — far in the past
	s.mu.Unlock()
	// Sweep with 1s ttl: cutoff = now-1s; ts=0 < cutoff → removed.
	s.Sweep(time.Second)
	// After sweep the tombstone is gone; a new SetAndDrain must succeed (not discard).
	s.SetAndDrain("old", 99, 99, "z", 0)
	_, _, _, _, ok := s.Get("old")
	if !ok {
		t.Fatal("expected coords stored after tombstone was swept and re-used")
	}
}

// TestPendingMsgStore_StandaloneWithoutPendingWait verifies the store works independently;
// no PendingWait coupling required.
func TestPendingMsgStore_StandaloneWithoutPendingWait(t *testing.T) {
	s := NewPendingMsgStore()
	s.SetAndDrain("solo", 1, 2, "standalone", 3)
	_, _, msgText, _, ok := s.Get("solo")
	if !ok || msgText != "standalone" {
		t.Fatalf("expected standalone coords, ok=%v msgText=%q", ok, msgText)
	}
}

// TestTryDispatchAsync_FullQueue verifies TryDispatchAsync returns false when the
// per-session channel is saturated and true when there is room.
func TestTryDispatchAsync_FullQueue(t *testing.T) {
	s := NewSessionEventStore()
	const sessionID = "test-session"
	// Channel cap is 128 (see session_queue.go getOrCreate).
	const cap = 128

	// Block the worker by sending a long-running job first via getOrCreate to ensure
	// the worker goroutine is created, then fill the remaining slots.
	// We start by doing one real dispatch to ensure the worker exists and is busy,
	// then fill the queue via TryDispatchAsync without letting the worker drain.
	blocked := make(chan struct{})
	release := make(chan struct{})

	// Send one blocking job via DispatchAsync so the worker goroutine is occupied.
	s.DispatchAsync(sessionID, "blocker", func() error {
		<-blocked // waits until we signal
		return nil
	})

	// Fill remaining cap-1 slots.
	for i := 0; i < cap-1; i++ {
		ok := s.TryDispatchAsync(sessionID, "filler", func() error { return nil })
		if !ok {
			t.Fatalf("TryDispatchAsync returned false before queue full (slot %d)", i)
		}
	}

	// Queue is now full: next TryDispatchAsync must fail.
	full := s.TryDispatchAsync(sessionID, "overflow", func() error { return nil })
	if full {
		t.Fatal("expected TryDispatchAsync to return false when queue is full")
	}

	// Release the blocker so the worker drains and the test exits cleanly.
	close(blocked)
	close(release)

	// After draining, TryDispatchAsync must accept new jobs.
	// Give the worker a moment to process.
	time.Sleep(20 * time.Millisecond)
	ok := s.TryDispatchAsync(sessionID, "after-drain", func() error { return nil })
	if !ok {
		t.Fatal("expected TryDispatchAsync to return true after queue drained")
	}
}

// TestTryDispatchAsync_EmptySessionID verifies that an empty sessionID causes
// the handler to run inline and return true.
func TestTryDispatchAsync_EmptySessionID(t *testing.T) {
	s := NewSessionEventStore()
	var ran int32
	ok := s.TryDispatchAsync("", "inline", func() error {
		atomic.AddInt32(&ran, 1)
		return nil
	})
	if !ok {
		t.Fatal("expected true for empty sessionID")
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Fatal("expected handler to run inline for empty sessionID")
	}
}
