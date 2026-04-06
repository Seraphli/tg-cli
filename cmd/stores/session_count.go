package stores

import "sync"

// SessionCountStore manages per-session counts and per-session mutexes.
type SessionCountStore struct {
	mu     sync.Mutex
	Counts map[string]int
	Locks  map[string]*sync.Mutex
}

// NewSessionCountStore creates an empty SessionCountStore.
func NewSessionCountStore() *SessionCountStore {
	return &SessionCountStore{
		Counts: make(map[string]int),
		Locks:  make(map[string]*sync.Mutex),
	}
}

// GetLock returns (creating if needed) the per-session mutex for the given session ID.
func (s *SessionCountStore) GetLock(sessionID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Locks[sessionID] == nil {
		s.Locks[sessionID] = &sync.Mutex{}
	}
	return s.Locks[sessionID]
}

// Cleanup removes the count and lock for the given session ID.
func (s *SessionCountStore) Cleanup(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Counts, sessionID)
	delete(s.Locks, sessionID)
}
