package stores

import (
	"encoding/json"
	"sync"
	"time"
)

// WaitEvent is delivered to a live connect handler or cached as Terminal.
type WaitEvent struct {
	Type   string          `json:"type"`             // "answer" | "cancel"
	Output json.RawMessage `json:"output,omitempty"` // set for "answer"
}

// PendingWaitEntry holds all state for one pending hook wait (one PreToolUse/permission request).
// Removal lifecycle: entries are removed ONLY by (a) the connect handler after delivering a
// terminal/answer, (b) SweepUndelivered, or (c) SessionEnd cleanup. Push NEVER removes.
type PendingWaitEntry struct {
	UUID               string
	MsgID              int
	ChatID             int64
	TopicID            int
	ToolName           string
	ToolUseID          string // may be "" if PermissionRequest input omits it
	ToolInputCanonical string // canonical(tool_input) — primary correlation key, computed at register
	SessionID          string
	TmuxTarget         string
	Payload            json.RawMessage // original enriched payload
	Ch                 chan WaitEvent  // buffered cap 1; the live connect handler reads it
	Terminal           *WaitEvent     // set if a terminal arrives with no live reader (delivered on next connect)
	Generation         int            // bumped on each (re)connect
	LiveGen            int            // generation of the currently-live handler (0 = none)
	Live               bool           // a connect handler is actively reading Ch right now
	Resolved           bool           // a terminal was produced; freeze done; awaiting delivery+removal
	ResolvedAt         int64          // unix secs when Resolved set (for the undelivered-terminal sweep)
	seq                int            // insertion-order counter for FIFO among collisions
}

// PendingWaitStore maps uuid -> PendingWaitEntry with a sync.RWMutex.
type PendingWaitStore struct {
	mu      sync.RWMutex
	entries map[string]*PendingWaitEntry
	nextSeq int
}

// NewPendingWaitStore creates an empty PendingWaitStore.
func NewPendingWaitStore() *PendingWaitStore {
	return &PendingWaitStore{
		entries: make(map[string]*PendingWaitEntry),
	}
}

// Register stores the entry under e.UUID. Allocates Ch if nil.
func (s *PendingWaitStore) Register(e *PendingWaitEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Ch == nil {
		e.Ch = make(chan WaitEvent, 1)
	}
	s.nextSeq++
	e.seq = s.nextSeq
	s.entries[e.UUID] = e
}

// Get returns the entry for uuid.
func (s *PendingWaitStore) Get(uuid string) (*PendingWaitEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[uuid]
	return e, ok
}

// Remove deletes the entry for uuid.
func (s *PendingWaitStore) Remove(uuid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, uuid)
}

// FindByMsgID returns the first entry with the given MsgID.
func (s *PendingWaitStore) FindByMsgID(msgID int) (*PendingWaitEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.MsgID == msgID {
			return e, true
		}
	}
	return nil, false
}

// FindBySession returns all entries with the given SessionID.
func (s *PendingWaitStore) FindBySession(sessionID string) []*PendingWaitEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*PendingWaitEntry
	for _, e := range s.entries {
		if e.SessionID == sessionID {
			result = append(result, e)
		}
	}
	return result
}

// FindMatch returns an UNRESOLVED entry matching the correlation key.
// If toolUseID != "" and an unresolved entry has that ToolUseID, it is returned immediately
// (stronger key). Otherwise returns the oldest unresolved entry (by insertion seq) with
// matching SessionID, ToolName, and ToolInputCanonical.
func (s *PendingWaitStore) FindMatch(sessionID, toolName, toolInputCanonical, toolUseID string) (*PendingWaitEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *PendingWaitEntry
	for _, e := range s.entries {
		if e.Resolved {
			continue
		}
		if toolUseID != "" && e.ToolUseID == toolUseID {
			return e, true
		}
		if e.SessionID == sessionID && e.ToolName == toolName && e.ToolInputCanonical == toolInputCanonical {
			if best == nil || e.seq < best.seq {
				best = e
			}
		}
	}
	if best != nil {
		return best, true
	}
	return nil, false
}

// Push marks the entry Resolved, delivers ev to a live reader (non-blocking) or caches it as
// Terminal. Push NEVER removes the entry — removal is the caller's responsibility.
func (s *PendingWaitStore) Push(uuid string, ev WaitEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return false
	}
	e.Resolved = true
	e.ResolvedAt = time.Now().Unix()
	if e.Live {
		select {
		case e.Ch <- ev:
		default:
		}
	} else {
		e.Terminal = &ev
	}
	return true
}

// BumpGeneration increments Generation and returns the new value (0 if unknown).
func (s *PendingWaitStore) BumpGeneration(uuid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return 0
	}
	e.Generation++
	return e.Generation
}

// TakeTerminal returns and clears e.Terminal (nil if none/unknown).
func (s *PendingWaitStore) TakeTerminal(uuid string) *WaitEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return nil
	}
	t := e.Terminal
	e.Terminal = nil
	return t
}

// SetLive marks the entry as having an active live handler with the given generation.
func (s *PendingWaitStore) SetLive(uuid string, gen int, ch chan WaitEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return
	}
	e.Live = true
	e.LiveGen = gen
}

// ClearLive clears the Live flag only if LiveGen == gen (prevents an old handler's disconnect
// from clearing a newer connection's live state).
func (s *PendingWaitStore) ClearLive(uuid string, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return
	}
	if e.LiveGen == gen {
		e.Live = false
	}
}

// CurrentGeneration returns the current Generation (0 if unknown).
func (s *PendingWaitStore) CurrentGeneration(uuid string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[uuid]
	if !ok {
		return 0
	}
	return e.Generation
}

// SweepUndelivered returns uuids of entries where Terminal != nil && Resolved &&
// (nowUnix - ResolvedAt) > ttlSecs.
func (s *PendingWaitStore) SweepUndelivered(ttlSecs int) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().Unix()
	var result []string
	for uuid, e := range s.entries {
		if e.Terminal != nil && e.Resolved && (now-e.ResolvedAt) > int64(ttlSecs) {
			result = append(result, uuid)
		}
	}
	return result
}
