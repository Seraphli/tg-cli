package stores

import "testing"

// TestAppendExisting_SetsNewTextSinceTool (S14a, rev 14 BLOCKER3): a continuation delta on an EXISTING bubble
// (AppendExisting on the appended path) sets NewTextSinceTool, so a drained continuation delta triggers the
// S6 pre-tool wait-skip + flush — not just a fresh bubble (AppendDelta). f25: this flag no longer drives the
// compact-cycle reset (that keys on SendBelowSinceTool). The marker is cleared first (a fresh bubble set it),
// then AppendExisting must re-set it.
func TestAppendExisting_SetsNewTextSinceTool(t *testing.T) {
	s := NewStreamStore()
	sid := "s1"
	// Create the bubble (AppendDelta sets the marker), then consume it so we can observe AppendExisting's set.
	s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "first delta", false)
	if !s.TakeNewTextSinceTool(sid) {
		t.Fatal("AppendDelta on a new bubble must set NewTextSinceTool")
	}
	if s.TakeNewTextSinceTool(sid) {
		t.Fatal("marker must be cleared after the first Take")
	}
	// A continuation delta on the SAME existing bubble via AppendExisting must set the marker again.
	handled, dropped, _ := s.AppendExisting(sid, "m1", "", 1, " continuation", false)
	if !handled || dropped {
		t.Fatalf("AppendExisting on an existing unsealed bubble must append (handled && !dropped), got handled=%v dropped=%v", handled, dropped)
	}
	if !s.TakeNewTextSinceTool(sid) {
		t.Fatal("AppendExisting on the appended path must set NewTextSinceTool (rev 14 BLOCKER3)")
	}
}

// TestAppendExisting_DropPath_DoesNotSetMarker (S14a): AppendExisting on a DROP path (sealed entry) must NOT
// set NewTextSinceTool — only the appended path sets it.
func TestAppendExisting_DropPath_DoesNotSetMarker(t *testing.T) {
	s := NewStreamStore()
	sid := "s1"
	s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "content", true)
	// Seal the entry so the next AppendExisting is dropped.
	ss := s.Session(sid)
	ss.DataMu.Lock()
	ss.Msgs["m1"].Sealed = true
	ss.DataMu.Unlock()
	// Clear the marker set by the initial AppendDelta.
	s.TakeNewTextSinceTool(sid)
	handled, dropped, _ := s.AppendExisting(sid, "m1", "", 1, " late", false)
	if !handled || !dropped {
		t.Fatalf("AppendExisting on a sealed entry must be dropped, got handled=%v dropped=%v", handled, dropped)
	}
	if s.TakeNewTextSinceTool(sid) {
		t.Fatal("a dropped AppendExisting must NOT set NewTextSinceTool")
	}
}

// TestTakeNewTextSinceTool_ConsumedUnconditionally (S14a, rev 16 BLOCKER3): the marker is consumed
// unconditionally on each read — a Take after a set returns true ONCE, then false on the next read (the marker
// does not leak to the next PreToolUse). This is the store-level invariant that the S6 handler relies on to
// avoid a stale-marker skip.
func TestTakeNewTextSinceTool_ConsumedUnconditionally(t *testing.T) {
	s := NewStreamStore()
	sid := "s1"
	s.AppendDelta(sid, StreamMeta{MessageID: "m1", ChatID: 1}, 0, "text", false)
	if !s.TakeNewTextSinceTool(sid) {
		t.Fatal("first Take after a set must return true")
	}
	// PTU2 arrives with NO new text — the marker must read a FRESH false (it did not leak).
	if s.TakeNewTextSinceTool(sid) {
		t.Fatal("the marker must NOT leak to a later read (rev 16 BLOCKER3): second Take must be false")
	}
	// An unknown session also reads false (no leak / no panic).
	if s.TakeNewTextSinceTool("unknown-session") {
		t.Fatal("Take on an unknown session must return false")
	}
}
