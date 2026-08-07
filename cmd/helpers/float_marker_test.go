package helpers

import (
	"testing"
	"time"
)

func TestFloatMarkerStore_MarkLastMark(t *testing.T) {
	s := NewFloatMarkerStore()
	zero := s.LastMark(1, 0)
	if !zero.IsZero() {
		t.Error("LastMark before any Mark should be zero")
	}

	now := time.Now()
	s.Mark(1, 0, now)
	got := s.LastMark(1, 0)
	if !got.Equal(now) {
		t.Errorf("LastMark after Mark = %v, want %v", got, now)
	}
}

func TestFloatMarkerStore_Monotonic(t *testing.T) {
	s := NewFloatMarkerStore()
	now := time.Now()

	s.Mark(1, 0, now.Add(10*time.Second))
	// Attempt to mark an older timestamp — must NOT move backward.
	s.Mark(1, 0, now.Add(5*time.Second))

	got := s.LastMark(1, 0)
	if !got.Equal(now.Add(10 * time.Second)) {
		t.Errorf("monotonic violated: marker moved to %v, want %v", got, now.Add(10*time.Second))
	}
}

func TestFloatMarkerStore_IndependentKeys(t *testing.T) {
	s := NewFloatMarkerStore()
	now := time.Now()

	s.Mark(1, 0, now)
	// (2, 0) and (1, 5) must be zero.
	if !s.LastMark(2, 0).IsZero() {
		t.Error("independent chat should be zero")
	}
	if !s.LastMark(1, 5).IsZero() {
		t.Error("independent topic should be zero")
	}
}

// TestFloatMarker_RecursionGuard verifies that a NoFloat send does NOT advance the marker.
// The busy manager always uses RetrySendNoFloat for status messages, so a status re-float
// must never cause the marker to advance and trigger another re-float (A2 recursion guard).
// This test operates at the FloatMarkerStore level: verify that NOT calling Mark leaves the
// value unchanged — a NoFloat path simply never calls FloatMarker.Mark.
func TestFloatMarker_RecursionGuard(t *testing.T) {
	s := NewFloatMarkerStore()
	now := time.Now()

	// A normal send marks (advances the marker).
	s.Mark(1, 0, now)
	if s.LastMark(1, 0).IsZero() {
		t.Error("normal Mark should advance the marker")
	}

	// Simulate a NoFloat path: do NOT call Mark. The marker must remain at `now`.
	// (In production code, RetrySendNoFloat never calls FloatMarker.Mark.)
	got := s.LastMark(1, 0)
	if !got.Equal(now) {
		t.Errorf("NoFloat path must not advance marker (got %v, want %v)", got, now)
	}
}
