package stores

import "sync"

// SessionCountStore manages per-session transcript offsets (every transcript line before the offset was sent or
// skipped) and per-session mutexes.
type SessionCountStore struct {
	mu      sync.Mutex
	Offsets map[string]int64
	Locks   map[string]*sync.Mutex
}

// NewSessionCountStore creates an empty SessionCountStore.
func NewSessionCountStore() *SessionCountStore {
	return &SessionCountStore{
		Offsets: make(map[string]int64),
		Locks:   make(map[string]*sync.Mutex),
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

// Cleanup removes the offset and lock for the given session ID.
func (s *SessionCountStore) Cleanup(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Offsets, sessionID)
	delete(s.Locks, sessionID)
}
