package stores

import "sync"

// PendingFileStore maps Telegram message IDs to pending file upload UUIDs.
type PendingFileStore struct {
	mu      sync.RWMutex
	entries map[int]string
}

// NewPendingFileStore creates an empty PendingFileStore.
func NewPendingFileStore() *PendingFileStore {
	return &PendingFileStore{
		entries: make(map[int]string),
	}
}

// Store saves a UUID for the given message ID.
func (pfs *PendingFileStore) Store(msgID int, uuid string) {
	pfs.mu.Lock()
	defer pfs.mu.Unlock()
	pfs.entries[msgID] = uuid
}

// Get retrieves the UUID for a message ID.
func (pfs *PendingFileStore) Get(msgID int) (string, bool) {
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()
	uuid, ok := pfs.entries[msgID]
	return uuid, ok
}

// Delete removes the entry for a message ID.
func (pfs *PendingFileStore) Delete(msgID int) {
	pfs.mu.Lock()
	defer pfs.mu.Unlock()
	delete(pfs.entries, msgID)
}

// FindByUUID returns the message ID for a given UUID.
func (pfs *PendingFileStore) FindByUUID(uuid string) (int, bool) {
	pfs.mu.RLock()
	defer pfs.mu.RUnlock()
	for msgID, u := range pfs.entries {
		if u == uuid {
			return msgID, true
		}
	}
	return 0, false
}

// Remove removes the entry for a message ID (alias for Delete).
func (pfs *PendingFileStore) Remove(msgID int) {
	pfs.mu.Lock()
	defer pfs.mu.Unlock()
	delete(pfs.entries, msgID)
}
