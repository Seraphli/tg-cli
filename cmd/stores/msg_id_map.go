package stores

import (
	"sync"
	"sync/atomic"
)

// msgIDEntry maps an internal message id to the delivered TG message id and its owning session.
type msgIDEntry struct {
	tgMsgID   int
	sessionID string
}

// MsgIDMap resolves an internal message id (allocated on the Hook FIFO via Allocate) to the delivered
// TG message id. The internal-id -> (TG-msg-id, sessionID) MAP is written AND read AND deleted ONLY by
// Message-FIFO ops (INV4/INV5); Allocate is an atomic counter bump that NEVER touches the map, so it is
// safe to call on the Hook FIFO.
type MsgIDMap struct {
	mu      sync.RWMutex
	entries map[int64]msgIDEntry
	next    atomic.Int64
}

func NewMsgIDMap() *MsgIDMap {
	return &MsgIDMap{entries: make(map[int64]msgIDEntry)}
}

// Allocate returns a fresh, monotonically increasing internal id. Atomic counter bump ONLY — it does
// NOT touch the map, so it is safe to call from the Hook FIFO (INV4).
func (m *MsgIDMap) Allocate() int64 {
	return m.next.Add(1)
}

// Set records the delivered TG message id (and owning session) for an internal id. Message FIFO only.
func (m *MsgIDMap) Set(internalID int64, tgMsgID int, sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[internalID] = msgIDEntry{tgMsgID: tgMsgID, sessionID: sessionID}
}

// Get returns the delivered TG message id for an internal id, and whether a mapping exists. Message FIFO only.
func (m *MsgIDMap) Get(internalID int64) (int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[internalID]
	return e.tgMsgID, ok
}

// Delete removes the mapping for a single internal id. Message FIFO only.
func (m *MsgIDMap) Delete(internalID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, internalID)
}

// DeleteSession removes all mappings owned by a session (called on SessionEnd). Message FIFO only.
func (m *MsgIDMap) DeleteSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.entries {
		if e.sessionID == sessionID {
			delete(m.entries, id)
		}
	}
}
