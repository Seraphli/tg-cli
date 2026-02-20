package cmd

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

type customCmd struct {
	desc   string
	ccName string
}

var ccBuiltinCommands = map[string]string{
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
	"resume":         "Resume a conversation",
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

type pageCacheStore struct {
	mu       sync.RWMutex
	entries  map[int]*pageEntry
	sessions map[string][]int // sessionID → []messageID
}

type pageEntry struct {
	chunks     []string
	event      string
	project    string
	tmuxTarget string
	permRows   []tele.Row // non-nil for permission messages
	chatID     int64
}

var pages = &pageCacheStore{
	entries:  make(map[int]*pageEntry),
	sessions: make(map[string][]int),
}

func (pc *pageCacheStore) store(msgID int, sessionID string, entry *pageEntry) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.entries[msgID] = entry
	if sessionID != "" {
		pc.sessions[sessionID] = append(pc.sessions[sessionID], msgID)
	}
}

func (pc *pageCacheStore) get(msgID int) (*pageEntry, bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	e, ok := pc.entries[msgID]
	return e, ok
}

func (pc *pageCacheStore) cleanupSession(sessionID string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for _, msgID := range pc.sessions[sessionID] {
		delete(pc.entries, msgID)
	}
	delete(pc.sessions, sessionID)
}

type permDecision struct {
	Behavior           string          `json:"behavior"`
	Message            string          `json:"message,omitempty"`
	UpdatedPermissions json.RawMessage `json:"updatedPermissions,omitempty"`
}

type pendingPermStore struct {
	mu          sync.RWMutex
	targets     map[int]string
	suggestions map[int]json.RawMessage
	msgTexts    map[int]string
	chatIDs     map[int]int64
	uuids       map[int]string
}

var pendingPerms = &pendingPermStore{
	targets:     make(map[int]string),
	suggestions: make(map[int]json.RawMessage),
	msgTexts:    make(map[int]string),
	chatIDs:     make(map[int]int64),
	uuids:       make(map[int]string),
}

func (ps *pendingPermStore) create(msgID int, tmuxTarget string, suggestionsJSON json.RawMessage, msgText string, chatID int64, uuid string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.targets[msgID] = tmuxTarget
	ps.suggestions[msgID] = suggestionsJSON
	ps.msgTexts[msgID] = msgText
	ps.chatIDs[msgID] = chatID
	ps.uuids[msgID] = uuid
}

func (ps *pendingPermStore) resolve(msgID int, d permDecision) bool {
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

func (ps *pendingPermStore) getUUID(msgID int) (string, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	uuid, ok := ps.uuids[msgID]
	return uuid, ok
}

func (ps *pendingPermStore) getTarget(msgID int) (string, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	t, ok := ps.targets[msgID]
	return t, ok
}

func (ps *pendingPermStore) getSuggestions(msgID int) json.RawMessage {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.suggestions[msgID]
}

func (ps *pendingPermStore) getMsgText(msgID int) string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.msgTexts[msgID]
}

func (ps *pendingPermStore) getChatID(msgID int) int64 {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.chatIDs[msgID]
}

func (ps *pendingPermStore) cleanup(msgID int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.targets, msgID)
	delete(ps.suggestions, msgID)
	delete(ps.msgTexts, msgID)
	delete(ps.chatIDs, msgID)
	delete(ps.uuids, msgID)
}

type questionMeta struct {
	questionText    string
	header          string
	numOptions      int
	optionLabels    []string
	multiSelect     bool
	selectedOptions map[int]bool
	selectedOption  int
}

type toolNotifyEntry struct {
	tmuxTarget  string
	toolName    string
	questions   []questionMeta
	chatID      int64
	msgText     string
	pendingUUID string
	resolved    bool
}

type toolNotifyStore struct {
	mu      sync.RWMutex
	entries map[int]*toolNotifyEntry
}

var toolNotifs = &toolNotifyStore{
	entries: make(map[int]*toolNotifyEntry),
}

func (ts *toolNotifyStore) store(msgID int, entry *toolNotifyEntry) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.entries[msgID] = entry
}

func (ts *toolNotifyStore) get(msgID int) (*toolNotifyEntry, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	e, ok := ts.entries[msgID]
	return e, ok
}

func (ts *toolNotifyStore) markResolved(msgID int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if e, ok := ts.entries[msgID]; ok {
		e.resolved = true
	}
}

func (ts *toolNotifyStore) findByTmuxTarget(tmuxTarget string) (int, *toolNotifyEntry, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	for msgID, e := range ts.entries {
		if e.tmuxTarget == tmuxTarget && e.toolName == "AskUserQuestion" && !e.resolved {
			return msgID, e, true
		}
	}
	return 0, nil, false
}

type pendingFileStore struct {
	mu      sync.RWMutex
	entries map[int]string
}

var pendingFiles = &pendingFileStore{
	entries: make(map[int]string),
}

func (pfs *pendingFileStore) store(msgID int, uuid string) {
	pfs.mu.Lock()
	defer pfs.mu.Unlock()
	pfs.entries[msgID] = uuid
}

func (pfs *pendingFileStore) get(msgID int) (string, bool) {
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()
	uuid, ok := pfs.entries[msgID]
	return uuid, ok
}

func (pfs *pendingFileStore) delete(msgID int) {
	pfs.mu.Lock()
	defer pfs.mu.Unlock()
	delete(pfs.entries, msgID)
}

type sessionCountStore struct {
	mu     sync.Mutex
	counts map[string]int
	locks  map[string]*sync.Mutex
}

var sessionCounts = &sessionCountStore{
	counts: make(map[string]int),
	locks:  make(map[string]*sync.Mutex),
}

func (s *sessionCountStore) getLock(sessionID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[sessionID] == nil {
		s.locks[sessionID] = &sync.Mutex{}
	}
	return s.locks[sessionID]
}

func (s *sessionCountStore) cleanup(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counts, sessionID)
	delete(s.locks, sessionID)
}

type reactionEntry struct {
	chatID int64
	msgID  int
}

type reactionTrackerStore struct {
	mu      sync.Mutex
	entries map[string][]reactionEntry
}

var reactionTracker = &reactionTrackerStore{
	entries: make(map[string][]reactionEntry),
}

func (rt *reactionTrackerStore) record(tmuxTarget string, chatID int64, msgID int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.entries[tmuxTarget] = append(rt.entries[tmuxTarget], reactionEntry{chatID: chatID, msgID: msgID})
	logger.Debug(fmt.Sprintf("Reaction recorded: target=%s msg_id=%d", tmuxTarget, msgID))
}

func (rt *reactionTrackerStore) clearAndRemove(bot *tele.Bot, tmuxTarget string) {
	rt.mu.Lock()
	rEntries := rt.entries[tmuxTarget]
	delete(rt.entries, tmuxTarget)
	rt.mu.Unlock()
	if len(rEntries) > 0 {
		logger.Debug(fmt.Sprintf("Clearing %d reactions for target %s", len(rEntries), tmuxTarget))
	}
	for _, e := range rEntries {
		bot.Raw("setMessageReaction", map[string]interface{}{
			"chat_id":    e.chatID,
			"message_id": e.msgID,
			"reaction":   []interface{}{},
		})
	}
}
