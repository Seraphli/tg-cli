package stores

import "testing"

// TestCompactToolStore_ResetAndTakeInternalID (S14b): ResetAndTakeInternalID discards the session's compact
// entry and returns its InternalID; a Reset with no entry returns 0 (the resetCompactAndScheduleDelete no-op
// path — no msg:id-delete is enqueued for id 0).
func TestCompactToolStore_ResetAndTakeInternalID(t *testing.T) {
	s := NewCompactToolStore()
	// No entry yet → 0 (no mapping to delete).
	if got := s.ResetAndTakeInternalID("sessA"); got != 0 {
		t.Fatalf("ResetAndTakeInternalID with no entry must return 0, got %d", got)
	}
	s.Store("sessA", &CompactToolEntry{InternalID: 7, ChatID: 1, Lines: []string{"x"}})
	if got := s.ResetAndTakeInternalID("sessA"); got != 7 {
		t.Fatalf("ResetAndTakeInternalID must return the discarded entry's InternalID, got %d", got)
	}
	if _, ok := s.Get("sessA"); ok {
		t.Fatal("ResetAndTakeInternalID must discard the entry")
	}
	// A second take (entry already gone) returns 0.
	if got := s.ResetAndTakeInternalID("sessA"); got != 0 {
		t.Fatalf("second ResetAndTakeInternalID must return 0, got %d", got)
	}
}
