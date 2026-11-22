package stores

import "testing"

// TestCompactToolStore_ResetAndTake (S14b): ResetAndTake discards the session's compact
// entry and returns its InternalID; a Reset with no entry returns 0 (the resetCompactAndScheduleDelete no-op
// path — no msg:id-delete is enqueued for id 0).
func TestCompactToolStore_ResetAndTake(t *testing.T) {
	s := NewCompactToolStore()
	// No entry yet → 0 (no mapping to delete).
	if got, _ := s.ResetAndTake("sessA"); got != 0 {
		t.Fatalf("ResetAndTake with no entry must return 0, got %d", got)
	}
	s.Store("sessA", &CompactToolEntry{InternalID: 7, ChatID: 1, Lines: []string{"x"}})
	if got, _ := s.ResetAndTake("sessA"); got != 7 {
		t.Fatalf("ResetAndTake must return the discarded entry's InternalID, got %d", got)
	}
	if _, ok := s.Get("sessA"); ok {
		t.Fatal("ResetAndTake must discard the entry")
	}
	// A second take (entry already gone) returns 0.
	if got, _ := s.ResetAndTake("sessA"); got != 0 {
		t.Fatalf("second ResetAndTake must return 0, got %d", got)
	}
}
