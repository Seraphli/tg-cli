package stores

import "testing"

// TestToolUseMsgStore_CleanupSession (S14b): CleanupSession removes ALL of a session's tool-use entries
// (abandoned PreToolUse with no matching PostToolUse) on SessionEnd (Hook FIFO), leaving other sessions'
// entries intact.
func TestToolUseMsgStore_CleanupSession(t *testing.T) {
	s := NewToolUseMsgStore()
	s.Store("tuA1", &ToolUseMsgEntry{InternalID: 1, SessionID: "sessA"})
	s.Store("tuA2", &ToolUseMsgEntry{InternalID: 2, SessionID: "sessA"})
	s.Store("tuB1", &ToolUseMsgEntry{InternalID: 3, SessionID: "sessB"})
	s.CleanupSession("sessA")
	if _, ok := s.Get("tuA1"); ok {
		t.Fatal("CleanupSession must remove sessA's first entry")
	}
	if _, ok := s.Get("tuA2"); ok {
		t.Fatal("CleanupSession must remove sessA's second entry")
	}
	if e, ok := s.Get("tuB1"); !ok || e.SessionID != "sessB" {
		t.Fatal("CleanupSession must NOT touch sessB's entry")
	}
}

// TestToolUseMsgStore_StoreGetDelete (S14b): the standard-tool result state machine — Store on PreToolUse,
// Get + Delete on PostToolUse (capture the InternalID, delete the entry, then enqueue the result op).
func TestToolUseMsgStore_StoreGetDelete(t *testing.T) {
	s := NewToolUseMsgStore()
	s.Store("tu1", &ToolUseMsgEntry{InternalID: 9, ChatID: 1, Body: "body", SessionID: "sessA"})
	e, ok := s.Get("tu1")
	if !ok || e.InternalID != 9 {
		t.Fatalf("Get after Store must return the entry, got %+v ok=%v", e, ok)
	}
	s.Delete("tu1")
	if _, ok := s.Get("tu1"); ok {
		t.Fatal("Delete must remove the entry (exactly one result edit per tool_use_id)")
	}
}
