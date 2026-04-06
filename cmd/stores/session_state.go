package stores

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
)

// SessionInfo holds the tmux target, working directory, project dir, and agent name for a CC session.
type SessionInfo struct {
	TmuxTarget     string
	CWD            string
	ProjectDir     string
	TranscriptPath string
	Name           string
	Backend        string
}

// SessionEntry is the JSON-serializable form of SessionInfo.
type SessionEntry struct {
	TmuxTarget     string `json:"tmux_target"`
	CWD            string `json:"cwd"`
	ProjectDir     string `json:"project_dir,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	Backend        string `json:"backend,omitempty"`
	Name           string `json:"name,omitempty"`
}

// SessionStateStore tracks active CC sessions and their associated info.
type SessionStateStore struct {
	mu           sync.RWMutex
	sessions     map[string]SessionInfo // session_id -> SessionInfo
	pendingNames map[string]string      // tmuxTarget -> name (preserved across remove/add)
	configDir    string
	// GetPaneCWD retrieves the CWD for a tmux target; injected to avoid circular imports.
	GetPaneCWD func(target string) string
}

// NewSessionStateStore creates an empty SessionStateStore with the given config directory.
func NewSessionStateStore(configDir string) *SessionStateStore {
	return &SessionStateStore{
		sessions:     make(map[string]SessionInfo),
		pendingNames: make(map[string]string),
		configDir:    configDir,
	}
}

// Add registers or updates a session entry.
func (s *SessionStateStore) Add(sessionID, tmuxTarget, cwd, transcriptPath string) {
	s.mu.Lock()
	// Remove stale sessions using the same pane but different session IDs, preserving name
	staleName := ""
	for id, info := range s.sessions {
		if info.TmuxTarget == tmuxTarget && id != sessionID {
			if info.Name != "" {
				staleName = info.Name
			}
			delete(s.sessions, id)
		}
	}
	// Consume pending name from Remove() if no stale name found
	if staleName == "" {
		if pn, ok := s.pendingNames[tmuxTarget]; ok {
			staleName = pn
		}
	}
	delete(s.pendingNames, tmuxTarget)
	// If session already exists with a CWD, preserve it to avoid drift from cd commands
	if existing, ok := s.sessions[sessionID]; ok && existing.CWD != "" {
		existing.TmuxTarget = tmuxTarget
		if existing.ProjectDir == "" && transcriptPath != "" {
			existing.ProjectDir = filepath.Dir(transcriptPath)
		}
		if existing.TranscriptPath == "" && transcriptPath != "" {
			existing.TranscriptPath = transcriptPath
		}
		// Preserve name from existing entry
		s.sessions[sessionID] = existing
		s.mu.Unlock()
		s.save()
		return
	}
	// First registration: prefer tmux pane CWD as it reflects the launch directory
	if s.GetPaneCWD != nil {
		if tmuxCWD := s.GetPaneCWD(tmuxTarget); tmuxCWD != "" {
			cwd = tmuxCWD
		}
	}
	projectDir := ""
	if transcriptPath != "" {
		projectDir = filepath.Dir(transcriptPath)
	}
	s.sessions[sessionID] = SessionInfo{TmuxTarget: tmuxTarget, CWD: cwd, ProjectDir: projectDir, TranscriptPath: transcriptPath, Name: staleName}
	s.mu.Unlock()
	s.save()
}

// SetBackend updates the backend field for an existing session.
func (s *SessionStateStore) SetBackend(sessionID, backend string) {
	s.mu.Lock()
	if info, ok := s.sessions[sessionID]; ok {
		info.Backend = backend
		s.sessions[sessionID] = info
	}
	s.mu.Unlock()
}

// Remove deletes a session entry, preserving the name for the next Add on the same pane.
func (s *SessionStateStore) Remove(sessionID string) {
	s.mu.Lock()
	if info, ok := s.sessions[sessionID]; ok && info.Name != "" {
		s.pendingNames[info.TmuxTarget] = info.Name
	}
	delete(s.sessions, sessionID)
	s.mu.Unlock()
	s.save()
}

// ClearPendingName removes any preserved name for the given tmux target.
func (s *SessionStateStore) ClearPendingName(tmuxTarget string) {
	s.mu.Lock()
	delete(s.pendingNames, tmuxTarget)
	s.mu.Unlock()
}

// save persists the current session map to disk.
func (s *SessionStateStore) save() {
	s.mu.RLock()
	data := make(map[string]SessionEntry, len(s.sessions))
	for sid, info := range s.sessions {
		data[sid] = SessionEntry{TmuxTarget: info.TmuxTarget, CWD: info.CWD, ProjectDir: info.ProjectDir, TranscriptPath: info.TranscriptPath, Backend: info.Backend, Name: info.Name}
	}
	s.mu.RUnlock()
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	path := filepath.Join(s.configDir, "sessions.json")
	os.WriteFile(path, b, 0644)
}

// LoadFromFile restores sessions from the persisted file.
func (s *SessionStateStore) LoadFromFile() {
	path := filepath.Join(s.configDir, "sessions.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var data map[string]SessionEntry
	if err := json.Unmarshal(b, &data); err != nil {
		return
	}
	// Deduplicate by TmuxTarget: keep only the latest entry per pane
	seen := make(map[string]string) // TmuxTarget → sessionID
	for sid, entry := range data {
		if prev, ok := seen[entry.TmuxTarget]; ok {
			delete(data, prev)
		}
		seen[entry.TmuxTarget] = sid
	}
	s.mu.Lock()
	for sid, entry := range data {
		s.sessions[sid] = SessionInfo{TmuxTarget: entry.TmuxTarget, CWD: entry.CWD, ProjectDir: entry.ProjectDir, TranscriptPath: entry.TranscriptPath, Backend: entry.Backend, Name: entry.Name}
	}
	s.mu.Unlock()
	s.save()
}

// ValidateAlive removes sessions whose tmux pane no longer exists.
func (s *SessionStateStore) ValidateAlive() {
	s.mu.Lock()
	for sid, info := range s.sessions {
		target, err := injector.ParseTarget(info.TmuxTarget)
		if err != nil || !injector.SessionExists(target) {
			delete(s.sessions, sid)
			logger.Info(fmt.Sprintf("Removed stale session: %s -> %s", sid, info.TmuxTarget))
		}
	}
	s.mu.Unlock()
	s.save()
}

// All returns a snapshot copy of all sessions.
func (s *SessionStateStore) All() map[string]SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make(map[string]SessionInfo, len(s.sessions))
	for k, v := range s.sessions {
		cp[k] = v
	}
	return cp
}

// FindByTarget returns the session ID for a given tmux target.
func (s *SessionStateStore) FindByTarget(target string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	normalized := notify.FormatPaneID(target)
	for sid, info := range s.sessions {
		if notify.FormatPaneID(info.TmuxTarget) == normalized {
			return sid, true
		}
	}
	return "", false
}

// FindInfoByTarget returns a pointer to the SessionInfo for a given tmux target.
func (s *SessionStateStore) FindInfoByTarget(target string) *SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	normalized := notify.FormatPaneID(target)
	for _, info := range s.sessions {
		if notify.FormatPaneID(info.TmuxTarget) == normalized {
			cp := info
			return &cp
		}
	}
	return nil
}

// FindByCWD returns the SessionInfo for the first active session with matching CWD.
func (s *SessionStateStore) FindByCWD(cwd string) *SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, info := range s.sessions {
		if info.CWD == cwd {
			cp := info
			return &cp
		}
	}
	return nil
}

// SetName sets the agent name for the session with the given sessionID.
func (s *SessionStateStore) SetName(sessionID, name string) (bool, string) {
	s.mu.Lock()
	info, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return false, "session not found"
	}
	if name != "" {
		for sid, other := range s.sessions {
			if sid != sessionID && other.Name == name {
				target, parseErr := injector.ParseTarget(other.TmuxTarget)
				if parseErr != nil || !injector.SessionExists(target) {
					logger.Info(fmt.Sprintf("setName: auto-cleaned dead session %s (target=%s) holding name '%s'", sid[:8], other.TmuxTarget, name))
					delete(s.sessions, sid)
					continue
				}
				s.mu.Unlock()
				return false, fmt.Sprintf("name '%s' already used by session %s", name, sid[:8])
			}
		}
	}
	info.Name = name
	s.sessions[sessionID] = info
	s.mu.Unlock()
	s.save()
	return true, ""
}

// FindByName returns the SessionInfo for the first active session with matching name.
func (s *SessionStateStore) FindByName(name string) *SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, info := range s.sessions {
		if info.Name == name {
			cp := info
			return &cp
		}
	}
	return nil
}

// FindInfoByID returns the SessionInfo for a session ID.
func (s *SessionStateStore) FindInfoByID(sessionID string) *SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	cp := info
	return &cp
}
