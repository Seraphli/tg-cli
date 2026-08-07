package helpers

import (
	"testing"
	"time"
)

func TestFloodBackoffStore_SetInBackoff(t *testing.T) {
	s := NewFloodBackoffStore()
	now := time.Now()

	// Not in backoff initially.
	if s.InBackoff(1, now) {
		t.Error("expected not in backoff before any Set")
	}

	// Set backoff 10s from now.
	s.Set(1, now.Add(10*time.Second))
	if !s.InBackoff(1, now) {
		t.Error("expected in backoff after Set")
	}

	// Exactly at boundary: not in backoff.
	if s.InBackoff(1, now.Add(10*time.Second)) {
		t.Error("expected NOT in backoff at exact expiry (before = strict)")
	}

	// Past expiry: not in backoff.
	if s.InBackoff(1, now.Add(11*time.Second)) {
		t.Error("expected NOT in backoff after expiry")
	}
}

func TestFloodBackoffStore_SetMonotonic(t *testing.T) {
	s := NewFloodBackoffStore()
	now := time.Now()

	// Set a 10s backoff.
	s.Set(1, now.Add(10*time.Second))
	// Attempt to set an EARLIER backoff — must NOT move backward.
	s.Set(1, now.Add(5*time.Second))

	// 7s from now must still be in backoff (the 10s expiry was not overwritten).
	if !s.InBackoff(1, now.Add(7*time.Second)) {
		t.Error("monotonic violated: older Set moved expiry backward")
	}
}

func TestFloodBackoffStore_IndependentChats(t *testing.T) {
	s := NewFloodBackoffStore()
	now := time.Now()

	s.Set(1, now.Add(10*time.Second))

	// Chat 2 must not be in backoff.
	if s.InBackoff(2, now) {
		t.Error("chat 2 should not be in backoff")
	}
}
