package stores

import "sync"

// ToolUseMsgEntry tracks a PreToolUse notification message for PostToolUse updates.
type ToolUseMsgEntry struct {
	MsgID   int
	ChatID  int64
	TopicID int
	Body    string // Original tool notification body from PreToolUse
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
