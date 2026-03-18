package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
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
	cwd        string
	tmuxTarget string
	permRows   []tele.Row // non-nil for permission messages
	rawMode    bool        // true = plain text, no HTML parse mode
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

func (ps *pendingPermStore) findByTmuxTarget(tmuxTarget string) (int, bool) {
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
	normalized := notify.FormatPaneID(tmuxTarget)
	for msgID, e := range ts.entries {
		if notify.FormatPaneID(e.tmuxTarget) == normalized && e.toolName == "AskUserQuestion" && !e.resolved {
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

func (s *pendingFileStore) findByUUID(uuid string) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for msgID, u := range s.entries {
		if u == uuid {
			return msgID, true
		}
	}
	return 0, false
}

func (s *pendingFileStore) remove(msgID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, msgID)
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

// sessionInfo holds the tmux target, working directory, project dir, and agent name for a CC session.
type sessionInfo struct {
	tmuxTarget string
	cwd        string
	projectDir string
	name       string
}

// sessionStateStore tracks active CC sessions and their associated info.
type sessionStateStore struct {
	mu           sync.RWMutex
	sessions     map[string]sessionInfo // session_id -> sessionInfo
	pendingNames map[string]string      // tmuxTarget -> name (preserved across remove/add)
}

var sessionState = &sessionStateStore{sessions: make(map[string]sessionInfo), pendingNames: make(map[string]string)}

func (s *sessionStateStore) add(sessionID, tmuxTarget, cwd, transcriptPath string) {
	s.mu.Lock()
	// Remove stale sessions using the same pane but different session IDs, preserving name
	staleName := ""
	for id, info := range s.sessions {
		if info.tmuxTarget == tmuxTarget && id != sessionID {
			if info.name != "" {
				staleName = info.name
			}
			delete(s.sessions, id)
		}
	}
	// Consume pending name from remove() if no stale name found
	if staleName == "" {
		if pn, ok := s.pendingNames[tmuxTarget]; ok {
			staleName = pn
		}
	}
	delete(s.pendingNames, tmuxTarget)
	// If session already exists with a CWD, preserve it to avoid drift from cd commands
	if existing, ok := s.sessions[sessionID]; ok && existing.cwd != "" {
		existing.tmuxTarget = tmuxTarget
		if existing.projectDir == "" && transcriptPath != "" {
			existing.projectDir = filepath.Dir(transcriptPath)
		}
		// Preserve name from existing entry
		s.sessions[sessionID] = existing
		s.mu.Unlock()
		s.save()
		return
	}
	// First registration: prefer tmux pane CWD as it reflects the launch directory
	if tmuxCWD := getPaneCWD(tmuxTarget); tmuxCWD != "" {
		cwd = tmuxCWD
	}
	projectDir := ""
	if transcriptPath != "" {
		projectDir = filepath.Dir(transcriptPath)
	}
	s.sessions[sessionID] = sessionInfo{tmuxTarget: tmuxTarget, cwd: cwd, projectDir: projectDir, name: staleName}
	s.mu.Unlock()
	s.save()
}

func (s *sessionStateStore) remove(sessionID string) {
	s.mu.Lock()
	if info, ok := s.sessions[sessionID]; ok && info.name != "" {
		s.pendingNames[info.tmuxTarget] = info.name
	}
	delete(s.sessions, sessionID)
	s.mu.Unlock()
	s.save()
}

func (s *sessionStateStore) clearPendingName(tmuxTarget string) {
	s.mu.Lock()
	delete(s.pendingNames, tmuxTarget)
	s.mu.Unlock()
}

// sessionEntry is the JSON-serializable form of sessionInfo.
type sessionEntry struct {
	TmuxTarget string `json:"tmux_target"`
	CWD        string `json:"cwd"`
	ProjectDir string `json:"project_dir,omitempty"`
	Name       string `json:"name,omitempty"`
}

// save persists the current session map to disk.
func (s *sessionStateStore) save() {
	s.mu.RLock()
	data := make(map[string]sessionEntry, len(s.sessions))
	for sid, info := range s.sessions {
		data[sid] = sessionEntry{TmuxTarget: info.tmuxTarget, CWD: info.cwd, ProjectDir: info.projectDir, Name: info.name}
	}
	s.mu.RUnlock()
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	path := filepath.Join(config.GetConfigDir(), "sessions.json")
	os.WriteFile(path, b, 0644)
}

// loadFromFile restores sessions from the persisted file.
func (s *sessionStateStore) loadFromFile() {
	path := filepath.Join(config.GetConfigDir(), "sessions.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var data map[string]sessionEntry
	if err := json.Unmarshal(b, &data); err != nil {
		return
	}
	// Deduplicate by tmuxTarget: keep only the latest entry per pane
	seen := make(map[string]string) // tmuxTarget → sessionID
	for sid, entry := range data {
		if prev, ok := seen[entry.TmuxTarget]; ok {
			delete(data, prev)
		}
		seen[entry.TmuxTarget] = sid
	}
	s.mu.Lock()
	for sid, entry := range data {
		s.sessions[sid] = sessionInfo{tmuxTarget: entry.TmuxTarget, cwd: entry.CWD, projectDir: entry.ProjectDir, name: entry.Name}
	}
	s.mu.Unlock()
	s.save()
}

// validateAlive removes sessions whose tmux pane no longer exists.
func (s *sessionStateStore) validateAlive() {
	s.mu.Lock()
	for sid, info := range s.sessions {
		target, err := injector.ParseTarget(info.tmuxTarget)
		if err != nil || !injector.SessionExists(target) {
			delete(s.sessions, sid)
			logger.Info(fmt.Sprintf("Removed stale session: %s -> %s", sid, info.tmuxTarget))
		}
	}
	s.mu.Unlock()
	s.save()
}


func (s *sessionStateStore) all() map[string]sessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make(map[string]sessionInfo, len(s.sessions))
	for k, v := range s.sessions {
		cp[k] = v
	}
	return cp
}

func (s *sessionStateStore) findByTarget(target string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	normalized := notify.FormatPaneID(target)
	for sid, info := range s.sessions {
		if notify.FormatPaneID(info.tmuxTarget) == normalized {
			return sid, true
		}
	}
	return "", false
}

func (s *sessionStateStore) findInfoByTarget(target string) *sessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	normalized := notify.FormatPaneID(target)
	for _, info := range s.sessions {
		if notify.FormatPaneID(info.tmuxTarget) == normalized {
			cp := info
			return &cp
		}
	}
	return nil
}

// findByCWD returns the sessionInfo for the first active session with matching CWD, or nil.
func (s *sessionStateStore) findByCWD(cwd string) *sessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, info := range s.sessions {
		if info.cwd == cwd {
			cp := info
			return &cp
		}
	}
	return nil
}

// setName sets the agent name for the session with the given sessionID.
func (s *sessionStateStore) setName(sessionID, name string) (bool, string) {
	s.mu.Lock()
	info, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return false, "session not found"
	}
	if name != "" {
		for sid, other := range s.sessions {
			if sid != sessionID && other.name == name {
				s.mu.Unlock()
				return false, fmt.Sprintf("name '%s' already used by session %s", name, sid[:8])
			}
		}
	}
	info.name = name
	s.sessions[sessionID] = info
	s.mu.Unlock()
	s.save()
	return true, ""
}

// findByName returns the sessionInfo for the first active session with matching name, or nil.
func (s *sessionStateStore) findByName(name string) *sessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, info := range s.sessions {
		if info.name == name {
			cp := info
			return &cp
		}
	}
	return nil
}

// findInfoByID returns the sessionInfo for a session ID, or nil.
func (s *sessionStateStore) findInfoByID(sessionID string) *sessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	cp := info
	return &cp
}

type reactionEntry struct {
	chatID int64
	msgID  int
}

type reactionTrackerStore struct {
	mu      sync.Mutex
	pending map[string][]reactionEntry // Injected, waiting for UserPromptSubmit
	active  map[string][]reactionEntry // Confirmed by UserPromptSubmit, showing ✍
}

var reactionTracker = &reactionTrackerStore{
	pending: make(map[string][]reactionEntry),
	active:  make(map[string][]reactionEntry),
}

func (rt *reactionTrackerStore) recordPending(tmuxTarget string, chatID int64, msgID int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.pending[tmuxTarget] = append(rt.pending[tmuxTarget], reactionEntry{chatID: chatID, msgID: msgID})
	logger.Debug(fmt.Sprintf("Reaction pending recorded: target=%s msg_id=%d", tmuxTarget, msgID))
}

// promotePending appends pending entries to active with ✍ reaction (replacing 👀).
func (rt *reactionTrackerStore) promotePending(bot *tele.Bot, tmuxTarget string) {
	rt.mu.Lock()
	newEntries := rt.pending[tmuxTarget]
	delete(rt.pending, tmuxTarget)
	if len(newEntries) > 0 {
		rt.active[tmuxTarget] = append(rt.active[tmuxTarget], newEntries...)
	}
	rt.mu.Unlock()

	// Set ✍ on newly promoted entries (replacing 👀)
	for _, e := range newEntries {
		bot.Raw("setMessageReaction", map[string]interface{}{
			"chat_id":    e.chatID,
			"message_id": e.msgID,
			"reaction":   []interface{}{map[string]interface{}{"type": "emoji", "emoji": "✍"}},
		})
	}
	if len(newEntries) > 0 {
		logger.Debug(fmt.Sprintf("Reactions promoted: target=%s promoted=%d", tmuxTarget, len(newEntries)))
	}
}

type mergeBuffer struct {
	items       []string
	chatID      int64
	tmuxTarget  string
	notifyMsgID int
}

type mergeBufferStore struct {
	mu      sync.RWMutex
	buffers map[string]*mergeBuffer
}

var mergeBuffers = &mergeBufferStore{
	buffers: make(map[string]*mergeBuffer),
}

func mergeKey(chatID int64) string {
	return fmt.Sprintf("%d", chatID)
}

func (ms *mergeBufferStore) start(key string, chatID int64, tmuxTarget string, notifyMsgID int) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.buffers[key] = &mergeBuffer{
		chatID:      chatID,
		tmuxTarget:  tmuxTarget,
		notifyMsgID: notifyMsgID,
	}
}

func (ms *mergeBufferStore) get(key string) *mergeBuffer {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.buffers[key]
}

func (ms *mergeBufferStore) add(key, content string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if buf, ok := ms.buffers[key]; ok {
		buf.items = append(buf.items, content)
	}
}

func (ms *mergeBufferStore) addAndGetInfo(key, content string) ([]string, int, int64, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	buf, ok := ms.buffers[key]
	if !ok {
		return nil, 0, 0, false
	}
	buf.items = append(buf.items, content)
	itemsCopy := make([]string, len(buf.items))
	copy(itemsCopy, buf.items)
	return itemsCopy, buf.notifyMsgID, buf.chatID, true
}

func (ms *mergeBufferStore) finish(key string) (*mergeBuffer, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	buf, ok := ms.buffers[key]
	if ok {
		delete(ms.buffers, key)
	}
	return buf, ok
}

type cronJob struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Mode        string    `json:"mode"`
	Schedule    string    `json:"schedule"`
	Once        bool      `json:"once"`
	Prompt      string    `json:"prompt"`
	AgentName   string    `json:"agent_name,omitempty"`
	TmuxTarget  string    `json:"tmux_target,omitempty"`
	CWD         string    `json:"cwd,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	MaxTurns    int       `json:"max_turns,omitempty"`
	LastRun     time.Time `json:"last_run,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type cronJobStore struct {
	mu   sync.RWMutex
	jobs map[string]*cronJob
}

var cronJobs = &cronJobStore{jobs: make(map[string]*cronJob)}

func (cs *cronJobStore) add(job *cronJob) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if job.Name != "" {
		for _, j := range cs.jobs {
			if j.Name == job.Name {
				return fmt.Errorf("name '%s' already exists", job.Name)
			}
		}
	}
	cs.jobs[job.ID] = job
	cs.saveLocked()
	return nil
}

func (cs *cronJobStore) remove(idOrName string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if _, ok := cs.jobs[idOrName]; ok {
		delete(cs.jobs, idOrName)
		cs.saveLocked()
		return true
	}
	for id, j := range cs.jobs {
		if j.Name == idOrName {
			delete(cs.jobs, id)
			cs.saveLocked()
			return true
		}
	}
	return false
}

func (cs *cronJobStore) findByName(name string) *cronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	for _, j := range cs.jobs {
		if j.Name == name {
			cp := *j
			return &cp
		}
	}
	return nil
}

func (cs *cronJobStore) findByIDOrName(idOrName string) *cronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if j, ok := cs.jobs[idOrName]; ok {
		cp := *j
		return &cp
	}
	for _, j := range cs.jobs {
		if j.Name == idOrName {
			cp := *j
			return &cp
		}
	}
	return nil
}

func (cs *cronJobStore) update(idOrName string, updates map[string]string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var job *cronJob
	if j, ok := cs.jobs[idOrName]; ok {
		job = j
	} else {
		for _, j := range cs.jobs {
			if j.Name == idOrName {
				job = j
				break
			}
		}
	}
	if job == nil {
		return false
	}
	if v, ok := updates["prompt"]; ok {
		job.Prompt = v
	}
	if v, ok := updates["schedule"]; ok {
		job.Schedule = v
	}
	if v, ok := updates["agent_name"]; ok {
		job.AgentName = v
	}
	if v, ok := updates["name"]; ok {
		for _, j := range cs.jobs {
			if j.ID != job.ID && j.Name == v {
				return false
			}
		}
		job.Name = v
	}
	if v, ok := updates["max_turns"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			job.MaxTurns = n
		}
	}
	cs.saveLocked()
	return true
}

func (cs *cronJobStore) get(id string) *cronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	j := cs.jobs[id]
	return j
}

func (cs *cronJobStore) all() []*cronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	result := make([]*cronJob, 0, len(cs.jobs))
	for _, j := range cs.jobs {
		cp := *j
		result = append(result, &cp)
	}
	return result
}

func (cs *cronJobStore) updateLastRun(id string, t time.Time) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if j, ok := cs.jobs[id]; ok {
		j.LastRun = t
		cs.saveLocked()
	}
}

func (cs *cronJobStore) updateSessionID(id, sessionID string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if j, ok := cs.jobs[id]; ok {
		j.SessionID = sessionID
		cs.saveLocked()
	}
}

func (cs *cronJobStore) saveLocked() {
	jobs := make([]*cronJob, 0, len(cs.jobs))
	for _, j := range cs.jobs {
		jobs = append(jobs, j)
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(config.GetConfigDir(), "cron-jobs.json")
	os.WriteFile(path, data, 0644)
}

func (cs *cronJobStore) load() {
	path := filepath.Join(config.GetConfigDir(), "cron-jobs.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var jobs []*cronJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, j := range jobs {
		cs.jobs[j.ID] = j
	}
	logger.Info(fmt.Sprintf("Loaded %d cron jobs", len(jobs)))
}
