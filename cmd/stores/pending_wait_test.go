package stores

import (
	"encoding/json"
	"testing"
)

func newEntry(uuid, sessionID, toolName, canonical, toolUseID string) *PendingWaitEntry {
	return &PendingWaitEntry{
		UUID:               uuid,
		SessionID:          sessionID,
		ToolName:           toolName,
		ToolInputCanonical: canonical,
		ToolUseID:          toolUseID,
	}
}

// Test 1: FindMatch returns UNRESOLVED-only (a Resolved entry is never matched).
func TestFindMatch_SkipsResolved(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{"cmd":"ls"}`, "")
	s.Register(e)
	e.Resolved = true
	_, ok := s.FindMatch("sess", "Bash", `{"cmd":"ls"}`, "")
	if ok {
		t.Fatal("expected no match for resolved entry")
	}
}

// Test 2: FindMatch matches by tool_use_id when present.
func TestFindMatch_ByToolUseID(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{"cmd":"ls"}`, "tid-123")
	s.Register(e)
	got, ok := s.FindMatch("sess", "Bash", `{"cmd":"ls"}`, "tid-123")
	if !ok || got.UUID != "u1" {
		t.Fatalf("expected match by tool_use_id, got ok=%v uuid=%v", ok, got)
	}
}

// Test 3: FindMatch matches by (session, tool, canonical) when tool_use_id is "".
func TestFindMatch_ByCanonical(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{"cmd":"ls"}`, "")
	s.Register(e)
	got, ok := s.FindMatch("sess", "Bash", `{"cmd":"ls"}`, "")
	if !ok || got.UUID != "u1" {
		t.Fatalf("expected match by canonical, got ok=%v", ok)
	}
}

// Test 4: A non-matching canonical is NOT found.
func TestFindMatch_NoMatch(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{"cmd":"ls"}`, "")
	s.Register(e)
	_, ok := s.FindMatch("sess", "Bash", `{"cmd":"pwd"}`, "")
	if ok {
		t.Fatal("expected no match for different canonical")
	}
}

// Test 5: FIFO among identical-input collisions.
func TestFindMatch_FIFO(t *testing.T) {
	s := NewPendingWaitStore()
	e1 := newEntry("u1", "sess", "Bash", `{"cmd":"ls"}`, "")
	e2 := newEntry("u2", "sess", "Bash", `{"cmd":"ls"}`, "")
	s.Register(e1)
	s.Register(e2)
	// First call must return e1 (lowest seq).
	got, ok := s.FindMatch("sess", "Bash", `{"cmd":"ls"}`, "")
	if !ok || got.UUID != "u1" {
		t.Fatalf("expected FIFO first entry u1, got %v", got)
	}
	// Resolve e1 and remove it.
	e1.Resolved = true
	s.Remove("u1")
	// Second call must return e2.
	got, ok = s.FindMatch("sess", "Bash", `{"cmd":"ls"}`, "")
	if !ok || got.UUID != "u2" {
		t.Fatalf("expected FIFO second entry u2, got %v", got)
	}
}

// Test 6: Push with no live reader sets Terminal; TakeTerminal returns it; entry still present.
func TestPush_NoLive_SetsTerminal(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{}`, "")
	s.Register(e)
	ev := WaitEvent{Type: "cancel"}
	if !s.Push("u1", ev) {
		t.Fatal("Push returned false")
	}
	// Entry must still exist.
	if _, ok := s.Get("u1"); !ok {
		t.Fatal("entry removed by Push — must not be removed")
	}
	// Terminal must be set.
	got := s.TakeTerminal("u1")
	if got == nil || got.Type != "cancel" {
		t.Fatalf("expected cancel terminal, got %v", got)
	}
	// TakeTerminal clears it.
	if s.TakeTerminal("u1") != nil {
		t.Fatal("expected nil after TakeTerminal cleared it")
	}
}

// Test 7: ClearLive with old gen does NOT clear Live after SetLive with new gen.
func TestClearLive_OldGenNoop(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{}`, "")
	s.Register(e)
	s.SetLive("u1", 2, e.Ch)
	s.ClearLive("u1", 1) // old gen — must be a no-op
	got, _ := s.Get("u1")
	if !got.Live {
		t.Fatal("Live was cleared by old gen — should not happen")
	}
	s.ClearLive("u1", 2) // correct gen — must clear
	if got.Live {
		t.Fatal("Live was not cleared by correct gen")
	}
}

// Test 8: Grace cancel keyed to gen N is a no-op after BumpGeneration advances to N+1.
func TestBumpGeneration_StaleGenNoop(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{}`, "")
	s.Register(e)
	gen := s.BumpGeneration("u1") // gen == 1
	s.SetLive("u1", gen, e.Ch)
	newGen := s.BumpGeneration("u1") // gen == 2
	// A handler from gen 1 tries to cancel — CurrentGeneration != 1.
	if s.CurrentGeneration("u1") == gen {
		t.Fatalf("expected generation to have advanced past %d", gen)
	}
	if s.CurrentGeneration("u1") != newGen {
		t.Fatalf("expected current generation %d, got %d", newGen, s.CurrentGeneration("u1"))
	}
}

// Test 9: SweepUndelivered returns only entries whose Terminal is older than TTL.
func TestSweepUndelivered(t *testing.T) {
	s := NewPendingWaitStore()
	e1 := newEntry("u1", "sess", "Bash", `{}`, "")
	e2 := newEntry("u2", "sess", "Bash", `{}`, "")
	s.Register(e1)
	s.Register(e2)
	ev := WaitEvent{Type: "cancel"}
	s.Push("u1", ev)
	s.Push("u2", ev)
	// Force e1's ResolvedAt to be old enough.
	s.mu.Lock()
	s.entries["u1"].ResolvedAt = 0 // far in the past
	s.mu.Unlock()
	// e2's ResolvedAt is just now — should not appear with ttl=1.
	swept := s.SweepUndelivered(1)
	if len(swept) != 1 || swept[0] != "u1" {
		t.Fatalf("expected only u1 in sweep, got %v", swept)
	}
}

// Test 10: Push never blocks (call with full channel and no reader).
func TestPush_NeverBlocks(t *testing.T) {
	s := NewPendingWaitStore()
	e := newEntry("u1", "sess", "Bash", `{}`, "")
	s.Register(e)
	// Mark as Live so Push tries to send on Ch (which is empty, capacity 1).
	s.SetLive("u1", 1, e.Ch)
	done := make(chan struct{})
	go func() {
		// Fill the channel so second push would block if not non-blocking.
		raw, _ := json.Marshal("data")
		s.Push("u1", WaitEvent{Type: "answer", Output: json.RawMessage(raw)})
		// Reset Resolved to allow a second push attempt on the same channel.
		s.mu.Lock()
		s.entries["u1"].Resolved = false
		s.mu.Unlock()
		// Channel is now full (cap 1); Push must not block.
		s.Push("u1", WaitEvent{Type: "cancel"})
		close(done)
	}()
	select {
	case <-done:
		// ok
	default:
		// done closed synchronously — also ok
		<-done
	}
}
