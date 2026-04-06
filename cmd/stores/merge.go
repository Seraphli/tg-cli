package stores

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Seraphli/tg-cli/internal/logger"
)

// MergeBuffer holds a pending merge of multiple content items for a single chat.
type MergeBuffer struct {
	Items       []string `json:"items"`
	ChatID      int64    `json:"chat_id"`
	TmuxTarget  string   `json:"tmux_target"`
	NotifyMsgID int      `json:"notify_msg_id"`
}

// MergeBufferStore manages per-key merge buffers with persistence.
type MergeBufferStore struct {
	mu        sync.RWMutex
	buffers   map[string]*MergeBuffer
	configDir string
}

// NewMergeBufferStore creates an empty MergeBufferStore with the given config directory.
func NewMergeBufferStore(configDir string) *MergeBufferStore {
	return &MergeBufferStore{
		buffers:   make(map[string]*MergeBuffer),
		configDir: configDir,
	}
}

// MergeKey returns the string key for a chat ID.
func MergeKey(chatID int64) string {
	return fmt.Sprintf("%d", chatID)
}

// Start initializes a new merge buffer for the given key.
func (ms *MergeBufferStore) Start(key string, chatID int64, tmuxTarget string, notifyMsgID int) {
	ms.mu.Lock()
	ms.buffers[key] = &MergeBuffer{
		ChatID:      chatID,
		TmuxTarget:  tmuxTarget,
		NotifyMsgID: notifyMsgID,
	}
	ms.saveLocked()
	ms.mu.Unlock()
}

// Get returns the buffer for the given key, or nil.
func (ms *MergeBufferStore) Get(key string) *MergeBuffer {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.buffers[key]
}

// Add appends content to the buffer for the given key.
func (ms *MergeBufferStore) Add(key, content string) {
	ms.mu.Lock()
	if buf, ok := ms.buffers[key]; ok {
		buf.Items = append(buf.Items, content)
	}
	ms.saveLocked()
	ms.mu.Unlock()
}

// AddAndGetInfo appends content and returns a snapshot of items, notifyMsgID, chatID, and existence.
func (ms *MergeBufferStore) AddAndGetInfo(key, content string) ([]string, int, int64, bool) {
	ms.mu.Lock()
	buf, ok := ms.buffers[key]
	if !ok {
		ms.mu.Unlock()
		return nil, 0, 0, false
	}
	buf.Items = append(buf.Items, content)
	itemsCopy := make([]string, len(buf.Items))
	copy(itemsCopy, buf.Items)
	notifyMsgID := buf.NotifyMsgID
	chatID := buf.ChatID
	ms.saveLocked()
	ms.mu.Unlock()
	return itemsCopy, notifyMsgID, chatID, true
}

// Finish removes and returns the buffer for the given key.
func (ms *MergeBufferStore) Finish(key string) (*MergeBuffer, bool) {
	ms.mu.Lock()
	buf, ok := ms.buffers[key]
	if ok {
		delete(ms.buffers, key)
	}
	ms.saveLocked()
	ms.mu.Unlock()
	return buf, ok
}

func (ms *MergeBufferStore) saveLocked() {
	type persistData struct {
		Buffers map[string]*MergeBuffer `json:"buffers"`
	}
	data, _ := json.MarshalIndent(persistData{Buffers: ms.buffers}, "", "  ")
	path := filepath.Join(ms.configDir, "merge-buffers.json")
	os.WriteFile(path, data, 0644)
}

// Load restores merge buffers from disk.
func (ms *MergeBufferStore) Load() {
	path := filepath.Join(ms.configDir, "merge-buffers.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var persist struct {
		Buffers map[string]*MergeBuffer `json:"buffers"`
	}
	if json.Unmarshal(data, &persist) == nil && persist.Buffers != nil {
		ms.mu.Lock()
		ms.buffers = persist.Buffers
		ms.mu.Unlock()
		logger.Info(fmt.Sprintf("Merge buffers loaded: %d active", len(persist.Buffers)))
	}
}
