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

// TestCCActivityRecordAndTTL covers the cc busy-TTL bridge: CCActive is true immediately after
// RecordCCActivity, then false once ttl elapses (a tiny ttl + short sleep, no 4s real wait).
func TestCCActivityRecordAndTTL(t *testing.T) {
	h := NewHookRunningStateStore()
	target := "%1"
	ttl := 20 * time.Millisecond

	// Unknown target — CCActive must be false.
	if h.CCActive(target, ttl) {
		t.Fatal("expected false before any RecordCCActivity")
	}

	h.RecordCCActivity(target)
	if !h.CCActive(target, ttl) {
		t.Fatal("expected true immediately after RecordCCActivity")
	}

	time.Sleep(3 * ttl)
	if h.CCActive(target, ttl) {
		t.Fatal("expected false after ttl elapsed")
	}
}

// TestCCActivityClear covers explicit clearing via ClearCCActivity (Stop/SessionEnd override).
func TestCCActivityClear(t *testing.T) {
	h := NewHookRunningStateStore()
	target := "%2"
	ttl := 10 * time.Second // long enough that only Clear (not TTL expiry) can flip it false

	h.RecordCCActivity(target)
	if !h.CCActive(target, ttl) {
		t.Fatal("expected true after RecordCCActivity")
	}
	h.ClearCCActivity(target)
	if h.CCActive(target, ttl) {
		t.Fatal("expected false after ClearCCActivity")
	}
}

// TestCCActivityTurnEndOrdering mirrors the turn-end MD, MD, Stop FIFO ordering: two Record calls
// followed by a Clear must leave CCActive false (Stop's Clear wins, final state is idle).
func TestCCActivityTurnEndOrdering(t *testing.T) {
	h := NewHookRunningStateStore()
	target := "%3"
	ttl := 10 * time.Second

	h.RecordCCActivity(target) // MD delta
	h.RecordCCActivity(target) // MD final
	h.ClearCCActivity(target)  // Stop
	if h.CCActive(target, ttl) {
		t.Fatal("expected false after Record, Record, Clear (turn-end ordering)")
	}
}

// TestCCActivityDoesNotAffectPiBoolPath proves the ccActivity map is additive: the existing pi
// SetRunning/SetIdle/IsRunning bool path is unaffected by cc-activity writes and clears.
func TestCCActivityDoesNotAffectPiBoolPath(t *testing.T) {
	h := NewHookRunningStateStore()
	target := "%4"

	h.SetRunning(target)
	h.RecordCCActivity(target)
	if running, known := h.IsRunning(target); !known || !running {
		t.Fatalf("pi bool path affected by RecordCCActivity: running=%v known=%v, want true/true", running, known)
	}

	h.ClearCCActivity(target)
	if running, known := h.IsRunning(target); !known || !running {
		t.Fatalf("pi bool path affected by ClearCCActivity: running=%v known=%v, want true/true", running, known)
	}
}
