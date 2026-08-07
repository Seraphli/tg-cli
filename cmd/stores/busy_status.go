package stores

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/logger"
)

// BusyStatusEntry tracks the in-Telegram status message for a (chatID, topicID) route.
type BusyStatusEntry struct {
	ChatID      int64     `json:"chat_id"`
	TopicID     int       `json:"topic_id"`
	MsgID       int       `json:"msg_id"`
	StartedAt   time.Time `json:"started_at"`
	SentAt      time.Time `json:"sent_at"`
	LastEditAt  time.Time `json:"last_edit_at"`
	LastFloatAt time.Time `json:"last_float_at"`
	IdleSince   time.Time `json:"idle_since"`
}

// BusyStatusStore persists and manages busy-status messages per (chatID, topicID) route.
// acting serializes outgoing API calls per key so a slow 1s tick never races the next.
type BusyStatusStore struct {
	mu        sync.Mutex
	entries   map[string]*BusyStatusEntry
	acting    map[string]bool
	configDir string
}

// NewBusyStatusStore creates an empty BusyStatusStore backed by the given config directory.
func NewBusyStatusStore(configDir string) *BusyStatusStore {
	return &BusyStatusStore{
		entries:   make(map[string]*BusyStatusEntry),
		acting:    make(map[string]bool),
		configDir: configDir,
	}
}

func busyKey(chatID int64, topicID int) string {
	return fmt.Sprintf("%d:%d", chatID, topicID)
}

// Reserve atomically checks-and-creates a placeholder entry (MsgID==0, all timestamps zero) for the
// key. Returns (copy, true) when created, (copy, false) when an entry already exists.
// Persists on create.
func (s *BusyStatusStore) Reserve(chatID int64, topicID int) (BusyStatusEntry, bool) {
	key := busyKey(chatID, topicID)
	s.mu.Lock()
	if existing, ok := s.entries[key]; ok {
		cp := *existing
		s.mu.Unlock()
		return cp, false
	}
	e := &BusyStatusEntry{ChatID: chatID, TopicID: topicID}
	s.entries[key] = e
	s.saveLocked()
	cp := *e
	s.mu.Unlock()
	return cp, true
}

// Get returns a defensive copy of the entry for (chatID, topicID), or zero value + false if absent.
func (s *BusyStatusStore) Get(chatID int64, topicID int) (BusyStatusEntry, bool) {
	key := busyKey(chatID, topicID)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return BusyStatusEntry{}, false
	}
	return *e, true
}

// Update overwrites the stored entry for e.ChatID/e.TopicID with a copy of e and persists.
func (s *BusyStatusStore) Update(e BusyStatusEntry) {
	key := busyKey(e.ChatID, e.TopicID)
	s.mu.Lock()
	cp := e
	s.entries[key] = &cp
	s.saveLocked()
	s.mu.Unlock()
}

// Delete removes the entry for (chatID, topicID) and persists.
func (s *BusyStatusStore) Delete(chatID int64, topicID int) {
	key := busyKey(chatID, topicID)
	s.mu.Lock()
	_, had := s.entries[key]
	delete(s.entries, key)
	if had {
		s.saveLocked()
	}
	s.mu.Unlock()
	if had {
		logger.Info(fmt.Sprintf("BusyStatus.Delete: chat=%d topic=%d", chatID, topicID))
	}
}

// GetAll returns defensive copies of all current entries.
func (s *BusyStatusStore) GetAll() []BusyStatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]BusyStatusEntry, 0, len(s.entries))
	for _, e := range s.entries {
		result = append(result, *e)
	}
	return result
}

// Clear removes all entries from the store and persists.
func (s *BusyStatusStore) Clear() {
	s.mu.Lock()
	s.entries = make(map[string]*BusyStatusEntry)
	s.saveLocked()
	s.mu.Unlock()
}

// TryBeginAction atomically claims the per-key action slot. Returns false if a claim already
// exists (action in flight); returns true and marks the slot on success. Network I/O must run
// OUTSIDE the store mutex; the caller must defer EndAction on every return path.
func (s *BusyStatusStore) TryBeginAction(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acting[key] {
		return false
	}
	s.acting[key] = true
	return true
}

// EndAction clears the per-key action claim.
func (s *BusyStatusStore) EndAction(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.acting, key)
}

func (s *BusyStatusStore) saveLocked() {
	data, _ := json.MarshalIndent(s.entries, "", "  ")
	path := filepath.Join(s.configDir, "busy_status.json")
	os.WriteFile(path, data, 0644)
}

// Load reads the persisted entries from disk into the store.
func (s *BusyStatusStore) Load() {
	path := filepath.Join(s.configDir, "busy_status.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var loaded map[string]*BusyStatusEntry
	if json.Unmarshal(data, &loaded) == nil && loaded != nil {
		s.mu.Lock()
		s.entries = loaded
		s.mu.Unlock()
		logger.Info(fmt.Sprintf("BusyStatus loaded: %d entries", len(loaded)))
	}
}
