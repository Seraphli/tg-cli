package stores

import "sync"

// CompactToolEntry tracks the current compact tool notification message for a session.
type CompactToolEntry struct {
	MsgID   int
	ChatID  int64
	TopicID int
	Lines   []string
}

// CompactToolStore maps sessionID to the current compact tool notification message.
type CompactToolStore struct {
	mu      sync.RWMutex
	entries map[string]*CompactToolEntry
}

func NewCompactToolStore() *CompactToolStore {
	return &CompactToolStore{entries: make(map[string]*CompactToolEntry)}
}

func (s *CompactToolStore) Get(sessionID string) (*CompactToolEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[sessionID]
	return e, ok
}

func (s *CompactToolStore) Store(sessionID string, entry *CompactToolEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[sessionID] = entry
}

func (s *CompactToolStore) Reset(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, sessionID)
}
