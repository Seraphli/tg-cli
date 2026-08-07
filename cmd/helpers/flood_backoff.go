package helpers

import (
	"sync"
	"time"
)

// FloodBackoffStore tracks per-chat FloodError backoff expiry (max-monotonic: never moves backward).
// All methods are nil-safe for call sites that check `if FloodBackoff != nil`.
type FloodBackoffStore struct {
	mu    sync.Mutex
	until map[int64]time.Time
}

// NewFloodBackoffStore creates an empty FloodBackoffStore.
func NewFloodBackoffStore() *FloodBackoffStore {
	return &FloodBackoffStore{until: make(map[int64]time.Time)}
}

// Set records that chatID is in flood backoff until `until`. Monotonic: only advances
// the expiry (a concurrent earlier timestamp never moves it backward).
func (s *FloodBackoffStore) Set(chatID int64, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until.After(s.until[chatID]) {
		s.until[chatID] = until
	}
}

// InBackoff returns true when chatID is currently in flood backoff relative to now.
func (s *FloodBackoffStore) InBackoff(chatID int64, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Before(s.until[chatID])
}

// FloodBackoff is the process-wide FloodError backoff store, set at bot startup.
var FloodBackoff *FloodBackoffStore
