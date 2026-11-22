package stores

import (
	"sync"
	"time"
)

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
	// The edit state below is shared by the Hook FIFO (a new line), the stream ticker and the Message FIFO (the
	// edit): every access goes through the store methods, under the store lock.
	Text       string    // latest full text of the message (all lines so far)
	Tool       string    // tool of the last line (log only)
	LineCount  int       // number of lines in Text (log only)
	Dirty      bool      // Text is newer than the message on Telegram
	LastEdit   time.Time // last send or edit of the message: the edit window starts here
	FloodUntil time.Time // after a FloodError: no edit before this time
	Seq        int64     // number of edits taken so far
	Applied    int64     // Seq of the newest edit that ran (Claim)
}

// CompactEdit is one edit of a compact tool message.
type CompactEdit struct {
	InternalID int64
	ChatID     int64
	Text       string
	Tool       string
	Lines      int
	Wait       time.Duration // flood wait still running when the edit was taken
	Seq        int64         // the entry's Seq when the edit was taken: a higher Seq is newer text
	entry      *CompactToolEntry
}

// take marks the entry clean as of now and returns its edit (caller holds the store lock).
func (e *CompactToolEntry) take(now time.Time) CompactEdit {
	e.Dirty, e.LastEdit = false, now
	e.Seq++
	return CompactEdit{InternalID: e.InternalID, ChatID: e.ChatID, Text: e.Text, Tool: e.Tool, Lines: e.LineCount, Wait: max(e.FloodUntil.Sub(now), 0), Seq: e.Seq, entry: e}
}

// Claim reports whether the edit may run: false when a newer edit of the same message already ran, because the
// edits reach the Message FIFO in the order they were enqueued, which can differ from the order they were taken
// in. The edit keeps its entry, so this also holds after the entry was discarded.
func (s *CompactToolStore) Claim(e CompactEdit) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.entry.Applied > e.Seq {
		return false
	}
	e.entry.Applied = e.Seq
	return true
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

// SetText stores the latest full text of the session's compact message and marks it dirty (a new tool line).
func (s *CompactToolStore) SetText(sessionID, text, tool string, lines int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[sessionID]; ok {
		e.Text, e.Tool, e.LineCount, e.Dirty = text, tool, lines, true
	}
}

// TakeDue returns the edit of every dirty message whose window (since its last send or edit) and flood wait are
// over, and marks those messages clean. The stream ticker calls it.
func (s *CompactToolStore) TakeDue(window time.Duration) map[string]CompactEdit {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	due := map[string]CompactEdit{}
	for id, e := range s.entries {
		if e.Dirty && !now.Before(e.FloodUntil) && now.Sub(e.LastEdit) >= window {
			due[id] = e.take(now)
		}
	}
	return due
}

// TakePending returns the session's pending edit regardless of the window (a message is about to go out below)
// and marks the message clean; nil when nothing is pending.
func (s *CompactToolStore) TakePending(sessionID string) *CompactEdit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takePendingLocked(sessionID)
}

func (s *CompactToolStore) takePendingLocked(sessionID string) *CompactEdit {
	e, ok := s.entries[sessionID]
	if !ok || !e.Dirty {
		return nil
	}
	edit := e.take(time.Now())
	return &edit
}

// ResetAndTake atomically discards the session's compact entry and returns the discarded entry's InternalID (0
// if none) and its pending edit (nil when clean). The caller (Hook FIFO, INV5) enqueues ONE Message-FIFO op that
// runs the pending edit and then the msg:id-delete for the returned id, so both are ordered after any prior ops
// on that id and before anything enqueued later.
func (s *CompactToolStore) ResetAndTake(sessionID string) (int64, *CompactEdit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[sessionID]
	if !ok {
		return 0, nil
	}
	pending := s.takePendingLocked(sessionID)
	delete(s.entries, sessionID)
	return e.InternalID, pending
}

// Retry marks the message dirty again after its edit hit a flood wait (or found a full queue): the next edit may
// not go out before notBefore and the window is over. Returns false when the message was discarded meanwhile.
func (s *CompactToolStore) Retry(sessionID string, internalID int64, notBefore time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[sessionID]
	if !ok || e.InternalID != internalID {
		return false
	}
	e.Dirty, e.FloodUntil, e.LastEdit = true, notBefore, time.Time{}
	return true
}
