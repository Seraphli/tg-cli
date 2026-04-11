package stores

import (
	"sync"

	tele "gopkg.in/telebot.v3"
)

// CustomCmd represents a user-defined custom command.
type CustomCmd struct {
	Desc   string
	CCName string
}

// CCBuiltinCommands maps CC slash command names to descriptions.
var CCBuiltinCommands = map[string]string{
	"clear":          "Clear conversation history",
	"compact":        "Compact conversation",
	"config":         "Open config",
	"context":        "Visualize context usage",
	"copy":           "Copy last response to clipboard",
	"cost":           "Show token usage stats",
	"debug":          "Debug current session",
	"doctor":         "Check installation health",
	"exit":           "Exit REPL",
	"export":         "Export conversation to file",
	"fast":           "Toggle fast mode",
	"help":           "Show help",
	"init":           "Initialize project CLAUDE.md",
	"mcp":            "Manage MCP servers",
	"memory":         "Edit CLAUDE.md memory",
	"model":          "Switch AI model",
	"permissions":    "View/update permissions",
	"plan":           "Enter plan mode",
	"rename":         "Rename current session",
	"rewind":         "Rewind conversation",
	"stats":          "Show usage stats",
	"status":         "Show status",
	"statusline":     "Configure status line",
	"tasks":          "List background tasks",
	"teleport":       "Resume remote session",
	"theme":          "Change color theme",
	"todos":          "List TODO items",
	"usage":          "Show plan usage limits",
	"vim":            "Toggle vim mode",
	"terminal_setup": "Configure terminal",
}

// PageEntry holds the paginated content and metadata for a sent message.
type PageEntry struct {
	Chunks            []string
	Event             string
	Project           string
	CWD               string
	TmuxTarget        string
	PermRows          []tele.Row // non-nil for permission messages
	RawMode           bool       // true = plain text, no HTML parse mode
	ChatID            int64
	CLICommand        string
	AgentName         string
	Backend           string
	ContextUsedPct    int // -1 means no data
	ContextUsedTokens int
	ContextWindowSize int
}

// PageCacheStore stores paginated message entries indexed by Telegram message ID.
type PageCacheStore struct {
	mu       sync.RWMutex
	entries  map[int]*PageEntry
	sessions map[string][]int // sessionID → []messageID
}

// NewPageCacheStore creates an empty PageCacheStore.
func NewPageCacheStore() *PageCacheStore {
	return &PageCacheStore{
		entries:  make(map[int]*PageEntry),
		sessions: make(map[string][]int),
	}
}

// Store saves an entry for the given message ID and optional session ID.
func (pc *PageCacheStore) Store(msgID int, sessionID string, entry *PageEntry) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.entries[msgID] = entry
	if sessionID != "" {
		pc.sessions[sessionID] = append(pc.sessions[sessionID], msgID)
	}
}

// Get retrieves the entry for a message ID.
func (pc *PageCacheStore) Get(msgID int) (*PageEntry, bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	e, ok := pc.entries[msgID]
	return e, ok
}

// CleanupSession removes all entries associated with a session ID.
func (pc *PageCacheStore) CleanupSession(sessionID string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for _, msgID := range pc.sessions[sessionID] {
		delete(pc.entries, msgID)
	}
	delete(pc.sessions, sessionID)
}
