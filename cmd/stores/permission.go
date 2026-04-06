package stores

import (
	"encoding/json"
	"sync"

	"github.com/Seraphli/tg-cli/internal/notify"
)

// PermDecision represents the result of a permission prompt.
type PermDecision struct {
	Behavior           string          `json:"behavior"`
	Message            string          `json:"message,omitempty"`
	UpdatedPermissions json.RawMessage `json:"updatedPermissions,omitempty"`
}

// PendingPermStore tracks in-flight permission requests keyed by Telegram message ID.
type PendingPermStore struct {
	mu          sync.RWMutex
	targets     map[int]string
	suggestions map[int]json.RawMessage
	msgTexts    map[int]string
	chatIDs     map[int]int64
	uuids       map[int]string
}

// NewPendingPermStore creates an empty PendingPermStore.
func NewPendingPermStore() *PendingPermStore {
	return &PendingPermStore{
		targets:     make(map[int]string),
		suggestions: make(map[int]json.RawMessage),
		msgTexts:    make(map[int]string),
		chatIDs:     make(map[int]int64),
		uuids:       make(map[int]string),
	}
}

// Create records a new pending permission request.
func (ps *PendingPermStore) Create(msgID int, tmuxTarget string, suggestionsJSON json.RawMessage, msgText string, chatID int64, uuid string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.targets[msgID] = tmuxTarget
	ps.suggestions[msgID] = suggestionsJSON
	ps.msgTexts[msgID] = msgText
	ps.chatIDs[msgID] = chatID
	ps.uuids[msgID] = uuid
}

// Resolve removes the entry and returns whether it existed.
func (ps *PendingPermStore) Resolve(msgID int, d PermDecision) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	_, ok := ps.targets[msgID]
	if !ok {
		return false
	}
	delete(ps.targets, msgID)
	delete(ps.suggestions, msgID)
	delete(ps.msgTexts, msgID)
	delete(ps.chatIDs, msgID)
	delete(ps.uuids, msgID)
	return true
}

// GetUUID returns the UUID for a message ID.
func (ps *PendingPermStore) GetUUID(msgID int) (string, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	uuid, ok := ps.uuids[msgID]
	return uuid, ok
}

// GetTarget returns the tmux target for a message ID.
func (ps *PendingPermStore) GetTarget(msgID int) (string, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	t, ok := ps.targets[msgID]
	return t, ok
}

// GetSuggestions returns the raw suggestions JSON for a message ID.
func (ps *PendingPermStore) GetSuggestions(msgID int) json.RawMessage {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.suggestions[msgID]
}

// GetMsgText returns the message text for a message ID.
func (ps *PendingPermStore) GetMsgText(msgID int) string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.msgTexts[msgID]
}

// GetChatID returns the chat ID for a message ID.
func (ps *PendingPermStore) GetChatID(msgID int) int64 {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.chatIDs[msgID]
}

// FindByTmuxTarget searches for a message ID by normalized tmux target.
func (ps *PendingPermStore) FindByTmuxTarget(tmuxTarget string) (int, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	normalized := notify.FormatPaneID(tmuxTarget)
	for msgID, t := range ps.targets {
		if notify.FormatPaneID(t) == normalized {
			return msgID, true
		}
	}
	return 0, false
}

// Cleanup removes all data for a message ID without resolving.
func (ps *PendingPermStore) Cleanup(msgID int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.targets, msgID)
	delete(ps.suggestions, msgID)
	delete(ps.msgTexts, msgID)
	delete(ps.chatIDs, msgID)
	delete(ps.uuids, msgID)
}
