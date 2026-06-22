package stores

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

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

// PermDecision represents the result of a permission prompt.
type PermDecision struct {
	Decision string
	Updated  bool
}

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
	Questions          []QuestionMeta  // AskUserQuestion prompts for this entry
	MsgText            string          // original TG message text
	PermSuggestions    json.RawMessage // raw permission suggestions JSON
}

// EntrySnapshot is an immutable copy of key PendingWaitEntry fields.
type EntrySnapshot struct {
	UUID            string
	MsgID           int
	ChatID          int64
	TopicID         int
	ToolName        string
	ToolUseID       string
	SessionID       string
	TmuxTarget      string
	Payload         json.RawMessage
	Resolved        bool
	Questions       []QuestionMeta
	MsgText         string
	PermSuggestions json.RawMessage
}

func snapshotOf(e *PendingWaitEntry) EntrySnapshot {
	return EntrySnapshot{
		UUID:            e.UUID,
		MsgID:           e.MsgID,
		ChatID:          e.ChatID,
		TopicID:         e.TopicID,
		ToolName:        e.ToolName,
		ToolUseID:       e.ToolUseID,
		SessionID:       e.SessionID,
		TmuxTarget:      e.TmuxTarget,
		Payload:         e.Payload,
		Resolved:        e.Resolved,
		Questions:       deepCopyQuestions(e.Questions),
		MsgText:         e.MsgText,
		PermSuggestions: append([]byte(nil), e.PermSuggestions...),
	}
}

// deepCopyQuestions returns a deep copy of a QuestionMeta slice.
func deepCopyQuestions(qs []QuestionMeta) []QuestionMeta {
	if qs == nil {
		return nil
	}
	cp := make([]QuestionMeta, len(qs))
	for i, q := range qs {
		cp[i] = q
		if q.OptionLabels != nil {
			cp[i].OptionLabels = make([]string, len(q.OptionLabels))
			copy(cp[i].OptionLabels, q.OptionLabels)
		}
		if q.SelectedOptions != nil {
			cp[i].SelectedOptions = make(map[int]bool, len(q.SelectedOptions))
			for k, v := range q.SelectedOptions {
				cp[i].SelectedOptions[k] = v
			}
		}
	}
	return cp
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

// ResolveIfUnresolved atomically resolves a pending entry. Returns (won, snap, found).
// If the entry is already resolved, returns (false, snap, true).
// If the entry doesn't exist, returns (false, {}, false).
// If won: sets Resolved=true, ResolvedAt, delivers ev to Ch (if Live) or stores as Terminal.
func (s *PendingWaitStore) ResolveIfUnresolved(uuid string, ev WaitEvent) (won bool, snap EntrySnapshot, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return false, EntrySnapshot{}, false
	}
	snap = snapshotOf(e)
	if e.Resolved {
		return false, snap, true
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
	snap.Resolved = true
	return true, snap, true
}

// BackfillMsgID sets the MsgID, ChatID, TopicID, and MsgText on an existing entry.
func (s *PendingWaitStore) BackfillMsgID(uuid string, msgID int, chatID int64, topicID int, msgText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return
	}
	e.MsgID = msgID
	e.ChatID = chatID
	e.TopicID = topicID
	e.MsgText = msgText
}

// ToggleQuestionOption toggles the selected state of an option in a multi-select question.
// Returns the updated Questions slice or an error.
func (s *PendingWaitStore) ToggleQuestionOption(uuid string, qIdx int, optIdx int) ([]QuestionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return nil, fmt.Errorf("entry not found: %s", uuid)
	}
	if qIdx < 0 || qIdx >= len(e.Questions) {
		return nil, fmt.Errorf("question index out of range: %d", qIdx)
	}
	q := &e.Questions[qIdx]
	if q.SelectedOptions == nil {
		q.SelectedOptions = make(map[int]bool)
	}
	q.SelectedOptions[optIdx] = !q.SelectedOptions[optIdx]
	return deepCopyQuestions(e.Questions), nil
}

// SelectQuestionOption sets the selected single-choice option index for a question.
// Returns the updated Questions slice or an error.
func (s *PendingWaitStore) SelectQuestionOption(uuid string, qIdx int, optIdx int) ([]QuestionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return nil, fmt.Errorf("entry not found: %s", uuid)
	}
	if qIdx < 0 || qIdx >= len(e.Questions) {
		return nil, fmt.Errorf("question index out of range: %d", qIdx)
	}
	e.Questions[qIdx].SelectedOption = optIdx
	return deepCopyQuestions(e.Questions), nil
}

// GetQuestions returns a deep copy of the Questions slice for the given entry.
func (s *PendingWaitStore) GetQuestions(uuid string) ([]QuestionMeta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return nil, false
	}
	return deepCopyQuestions(e.Questions), true
}

// FindByTmuxTarget returns the oldest UNRESOLVED entry for the given tmux target (FIFO by seq).
// Both the argument and stored TmuxTarget are normalized via notify.FormatPaneID before comparison.
func (s *PendingWaitStore) FindByTmuxTarget(tmuxTarget string) (*EntrySnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	normalized := notify.FormatPaneID(tmuxTarget)
	var best *PendingWaitEntry
	for _, e := range s.entries {
		if notify.FormatPaneID(e.TmuxTarget) == normalized && !e.Resolved {
			if best == nil || e.seq < best.seq {
				best = e
			}
		}
	}
	if best == nil {
		return nil, false
	}
	snap := snapshotOf(best)
	return &snap, true
}

// GetSnapshot returns an immutable snapshot of the entry.
func (s *PendingWaitStore) GetSnapshot(uuid string) (EntrySnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[uuid]
	if !ok {
		return EntrySnapshot{}, false
	}
	return snapshotOf(e), true
}

// FindByMsgIDSnapshot returns the first entry snapshot with the given MsgID.
func (s *PendingWaitStore) FindByMsgIDSnapshot(msgID int) (EntrySnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.MsgID == msgID {
			return snapshotOf(e), true
		}
	}
	return EntrySnapshot{}, false
}

// FindBySessionSnapshots returns snapshots of all entries with the given SessionID.
func (s *PendingWaitStore) FindBySessionSnapshots(sessionID string) []EntrySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []EntrySnapshot
	for _, e := range s.entries {
		if e.SessionID == sessionID {
			result = append(result, snapshotOf(e))
		}
	}
	return result
}

// BeginLive marks the entry as live and returns a snapshot, a receive-only channel, and the generation.
// The caller reads from the channel for live events. Returns (snap, ch, gen, found).
func (s *PendingWaitStore) BeginLive(uuid string) (snap EntrySnapshot, ch <-chan WaitEvent, gen int, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uuid]
	if !ok {
		return EntrySnapshot{}, nil, 0, false
	}
	e.Generation++
	e.Live = true
	e.LiveGen = e.Generation
	return snapshotOf(e), e.Ch, e.Generation, true
}

// PeekTerminal returns a copy of Terminal without clearing it. Returns nil if none.
func (s *PendingWaitStore) PeekTerminal(uuid string) *WaitEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[uuid]
	if !ok || e.Terminal == nil {
		return nil
	}
	cp := *e.Terminal
	return &cp
}

// ClearTerminal clears the Terminal field. Used after successful delivery.
func (s *PendingWaitStore) ClearTerminal(uuid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[uuid]; ok {
		e.Terminal = nil
	}
}

// RestoreTerminal sets the Terminal field back (e.g., after failed ndjson delivery).
func (s *PendingWaitStore) RestoreTerminal(uuid string, ev *WaitEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[uuid]; ok {
		e.Terminal = ev
	}
}
