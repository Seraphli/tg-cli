package helpers

import (
	"fmt"
	"sync"
	"time"
)

// FloatMarkerStore tracks the most-recent outbound message timestamp per (chatID, topicID) route.
// Monotonic: Mark only advances, never moves backward. Used by the busy manager to detect when a
// real message has been sent after the current status position (triggering a re-float).
type FloatMarkerStore struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// NewFloatMarkerStore creates an empty FloatMarkerStore.
func NewFloatMarkerStore() *FloatMarkerStore {
	return &FloatMarkerStore{last: make(map[string]time.Time)}
}

// Mark records t as the latest outbound-message timestamp for (chatID, topicID). Monotonic: only
// advances the marker — an older concurrent timestamp never overwrites a newer one.
func (s *FloatMarkerStore) Mark(chatID int64, topicID int, t time.Time) {
	key := fmt.Sprintf("%d:%d", chatID, topicID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.After(s.last[key]) {
		s.last[key] = t
	}
}

// LastMark returns the most recent outbound-message timestamp for (chatID, topicID), or zero if
// none has been recorded.
func (s *FloatMarkerStore) LastMark(chatID int64, topicID int) time.Time {
	key := fmt.Sprintf("%d:%d", chatID, topicID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[key]
}

// FloatMarker is the process-wide float marker store, set at bot startup.
var FloatMarker *FloatMarkerStore
