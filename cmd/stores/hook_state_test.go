package stores

import (
	"testing"
	"time"
)

func TestHasRecentStop(t *testing.T) {
	s := NewStopCooldownStore()

	// No record — should return false
	if s.HasRecentStop("%5", 10*time.Second) {
		t.Fatal("expected false with no record")
	}

	// Record a stop
	s.Record("%5")

	// Should return true within the window
	if !s.HasRecentStop("%5", 10*time.Second) {
		t.Fatal("expected true after Record")
	}

	// Different target — should return false
	if s.HasRecentStop("%6", 10*time.Second) {
		t.Fatal("expected false for different target")
	}
}
