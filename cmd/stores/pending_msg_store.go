package stores

import (
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/logger"
)

// SendResult holds the outcome of a TG send operation.
type SendResult struct {
	MsgID         int
	ChatID        int64
	TopicID       int
	MsgText       string
	MainErr       error
	SecondaryErrs []error
}

// msgIDCoords holds the TG send coordinates for a pending notification.
type msgIDCoords struct {
	MsgID   int
	ChatID  int64
	MsgText string
	TopicID int
}

// PendingMsgStore tracks TG send coordinates and deferred EDIT callbacks for pending notifications.
// Replaces NotifOpQueue's in-memory state. No worker goroutine — all decisions are atomic under one mutex.
type PendingMsgStore struct {
	mu        sync.Mutex
	msgIDs    map[string]msgIDCoords
	deferred  map[string]func(msgID int, chatID int64, msgText string, topicID int)
	closed    map[string]int64 // uuid -> unix timestamp tombstone
	lastSweep int64
}

// NewPendingMsgStore creates a new PendingMsgStore.
func NewPendingMsgStore() *PendingMsgStore {
	return &PendingMsgStore{
		msgIDs:   make(map[string]msgIDCoords),
		deferred: make(map[string]func(int, int64, string, int)),
		closed:   make(map[string]int64),
	}
}

// EditOrDefer atomically decides what to do with an EDIT for uuid.
// If uuid is closed → discard, return false.
// If coords exist → delete coords, run editFunc outside lock with those coords, return true.
// Otherwise → store editFunc in deferred map, return false.
// TG I/O (editFunc) runs OUTSIDE the lock.
func (s *PendingMsgStore) EditOrDefer(uuid string, editFunc func(msgID int, chatID int64, msgText string, topicID int)) bool {
	s.mu.Lock()
	if _, closed := s.closed[uuid]; closed {
		s.mu.Unlock()
		logger.Debug("PendingMsgStore: EditOrDefer discarded (closed tombstone) uuid=" + uuid)
		return false
	}
	coords, hasCoords := s.msgIDs[uuid]
	if hasCoords {
		delete(s.msgIDs, uuid)
		s.mu.Unlock()
		editFunc(coords.MsgID, coords.ChatID, coords.MsgText, coords.TopicID)
		return true
	}
	// No coords yet — defer for when SetAndDrain fires
	s.deferred[uuid] = editFunc
	s.mu.Unlock()
	return false
}

// SetAndDrain stores the send coordinates for uuid, then atomically drains any deferred EDIT.
// If uuid is closed → discard.
// TG I/O (deferred edit) runs OUTSIDE the lock.
func (s *PendingMsgStore) SetAndDrain(uuid string, msgID int, chatID int64, msgText string, topicID int) {
	s.mu.Lock()
	if _, closed := s.closed[uuid]; closed {
		s.mu.Unlock()
		return
	}
	coords := msgIDCoords{MsgID: msgID, ChatID: chatID, MsgText: msgText, TopicID: topicID}
	deferred := s.deferred[uuid]
	if deferred != nil {
		// Drain: take the deferred edit, drop coords (edit will run immediately)
		delete(s.deferred, uuid)
		s.mu.Unlock()
		deferred(coords.MsgID, coords.ChatID, coords.MsgText, coords.TopicID)
		return
	}
	s.msgIDs[uuid] = coords
	s.mu.Unlock()
}

// Get returns the stored coordinates for uuid (used for reconnect path).
func (s *PendingMsgStore) Get(uuid string) (msgID int, chatID int64, msgText string, topicID int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	coords, exists := s.msgIDs[uuid]
	if !exists {
		return 0, 0, "", 0, false
	}
	return coords.MsgID, coords.ChatID, coords.MsgText, coords.TopicID, true
}

// Delete marks uuid as closed (tombstone) and removes any stored state.
// Uses throttled sweep of old tombstones (mirrors notif_op_queue.go pattern).
func (s *PendingMsgStore) Delete(uuid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	s.closed[uuid] = now
	delete(s.msgIDs, uuid)
	delete(s.deferred, uuid)
	// Throttled sweep: remove tombstones older than 60s, at most every 10s
	if now-s.lastSweep > 10 {
		s.lastSweep = now
		for u, ts := range s.closed {
			if now-ts > 60 {
				delete(s.closed, u)
			}
		}
	}
}

// Sweep removes closed tombstones older than ttl. Called from periodic goroutine.
// ONLY removes closed tombstones, NOT live msgIDs.
func (s *PendingMsgStore) Sweep(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-ttl).Unix()
	for u, ts := range s.closed {
		if ts < cutoff {
			delete(s.closed, u)
		}
	}
}
