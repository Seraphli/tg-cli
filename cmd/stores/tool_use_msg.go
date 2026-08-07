package stores

import "sync"

// ToolUseMsgEntry tracks a PreToolUse notification message for PostToolUse updates.
type ToolUseMsgEntry struct {
	// InternalID is the internal message id (allocated on the Hook FIFO via MsgIDMap.Allocate) that
	// resolves to the delivered TG message id via MsgIDMap on the Message FIFO. Replaces the old
	// cross-op TG MsgID field (INV4).
	InternalID int64
	ChatID     int64
	TopicID    int
	Body       string // Original tool notification body from PreToolUse
	Rich       bool   // true = message was sent as rich (Bot API 10.1), edits must match
	SessionID  string // owning session; used by CleanupSession on SessionEnd
}

// ToolUseMsgStore maps tool_use_id to sent TG message info.
type ToolUseMsgStore struct {
	mu      sync.RWMutex
	entries map[string]*ToolUseMsgEntry
}

func NewToolUseMsgStore() *ToolUseMsgStore {
	return &ToolUseMsgStore{entries: make(map[string]*ToolUseMsgEntry)}
}

func (s *ToolUseMsgStore) Store(toolUseID string, entry *ToolUseMsgEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[toolUseID] = entry
}

func (s *ToolUseMsgStore) Get(toolUseID string) (*ToolUseMsgEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[toolUseID]
	return e, ok
}

func (s *ToolUseMsgStore) Delete(toolUseID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, toolUseID)
}

// CleanupSession removes all tool-use entries owned by a session (called on SessionEnd, Hook FIFO,
// INV5) so an abandoned PreToolUse entry with no matching PostToolUse does not leak.
func (s *ToolUseMsgStore) CleanupSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.entries {
		if e.SessionID == sessionID {
			delete(s.entries, id)
		}
	}
}
