package stores

import (
	"sync"

	"github.com/Seraphli/tg-cli/internal/notify"
)

// QuestionMeta holds metadata for a single AskUserQuestion prompt.
type QuestionMeta struct {
	QuestionText    string
	Header          string
	NumOptions      int
	OptionLabels    []string
	MultiSelect     bool
	SelectedOptions map[int]bool
	SelectedOption  int
}

// ToolNotifyEntry tracks a tool notification message awaiting user interaction.
type ToolNotifyEntry struct {
	TmuxTarget  string
	ToolName    string
	Questions   []QuestionMeta
	ChatID      int64
	MsgText     string
	PendingUUID string
	Resolved    bool
}

// ToolNotifyStore maps Telegram message IDs to pending tool notification entries.
type ToolNotifyStore struct {
	mu      sync.RWMutex
	entries map[int]*ToolNotifyEntry
}

// NewToolNotifyStore creates an empty ToolNotifyStore.
func NewToolNotifyStore() *ToolNotifyStore {
	return &ToolNotifyStore{
		entries: make(map[int]*ToolNotifyEntry),
	}
}

// Store saves an entry for the given message ID.
func (ts *ToolNotifyStore) Store(msgID int, entry *ToolNotifyEntry) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.entries[msgID] = entry
}

// Get retrieves the entry for a message ID.
func (ts *ToolNotifyStore) Get(msgID int) (*ToolNotifyEntry, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	e, ok := ts.entries[msgID]
	return e, ok
}

// MarkResolved marks the entry for a message ID as resolved.
func (ts *ToolNotifyStore) MarkResolved(msgID int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if e, ok := ts.entries[msgID]; ok {
		e.Resolved = true
	}
}

// FindByTmuxTarget returns the first unresolved AskUserQuestion entry matching the target.
func (ts *ToolNotifyStore) FindByTmuxTarget(tmuxTarget string) (int, *ToolNotifyEntry, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	normalized := notify.FormatPaneID(tmuxTarget)
	for msgID, e := range ts.entries {
		if notify.FormatPaneID(e.TmuxTarget) == normalized && e.ToolName == "AskUserQuestion" && !e.Resolved {
			return msgID, e, true
		}
	}
	return 0, nil, false
}
