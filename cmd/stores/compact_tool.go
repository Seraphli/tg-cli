package stores

import "sync"

// CompactToolEntry tracks the current compact tool notification message for a session.
type CompactToolEntry struct {
	// InternalID is the internal message id (allocated on the Hook FIFO via MsgIDMap.Allocate) that
	// resolves to the delivered TG message id via MsgIDMap on the Message FIFO. Replaces the old
	// cross-op TG MsgID field (INV4).
	InternalID int64
	ChatID     int64
	TopicID    int
	Lines      []string
	Rich       bool // true = message was first sent as rich (Bot API 10.1); all edits must match
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

// ResetAndTakeInternalID atomically discards the session's compact entry and returns the discarded
// entry's InternalID (0 if none). The caller (Hook FIFO, INV5) enqueues a Message-FIFO msg:id-delete
// for the returned id so the map mutation is ordered after any prior ops on that id.
func (s *CompactToolStore) ResetAndTakeInternalID(sessionID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[sessionID]
	if !ok {
		return 0
	}
	delete(s.entries, sessionID)
	return e.InternalID
}
