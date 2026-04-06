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

// InjectItem represents a queued text to inject when CC becomes idle.
type InjectItem struct {
	Text       string    `json:"text"`
	ChatID     int64     `json:"chat_id"`
	TopicID    int       `json:"topic_id"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}

// InjectQueueStore manages per-target inject queues for when CC is busy.
type InjectQueueStore struct {
	mu         sync.Mutex
	queues     map[string][]InjectItem
	notifyMsgs map[string]int
	injectIDs  map[string]string
	configDir  string
}

// NewInjectQueueStore creates an empty InjectQueueStore with the given config directory.
func NewInjectQueueStore(configDir string) *InjectQueueStore {
	return &InjectQueueStore{
		queues:     make(map[string][]InjectItem),
		notifyMsgs: make(map[string]int),
		injectIDs:  make(map[string]string),
		configDir:  configDir,
	}
}

// Enqueue adds an item to the queue for the given tmux target.
func (iq *InjectQueueStore) Enqueue(tmuxTarget string, item InjectItem) {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	item.EnqueuedAt = time.Now()
	iq.queues[tmuxTarget] = append(iq.queues[tmuxTarget], item)
	// Generate inject ID on first enqueue for this target
	if _, ok := iq.injectIDs[tmuxTarget]; !ok {
		iq.injectIDs[tmuxTarget] = fmt.Sprintf("%x", time.Now().UnixNano()%0xFFFFFF)
	}
	iq.saveLocked()
}

// GetInjectID returns the inject ID for the given tmux target.
func (iq *InjectQueueStore) GetInjectID(tmuxTarget string) string {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	return iq.injectIDs[tmuxTarget]
}

// Flush removes and returns all queued items for the given tmux target.
func (iq *InjectQueueStore) Flush(tmuxTarget string) []InjectItem {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	items := iq.queues[tmuxTarget]
	delete(iq.queues, tmuxTarget)
	delete(iq.notifyMsgs, tmuxTarget)
	delete(iq.injectIDs, tmuxTarget)
	iq.saveLocked()
	return items
}

// HasItems reports whether the queue for a tmux target is non-empty.
func (iq *InjectQueueStore) HasItems(tmuxTarget string) bool {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	return len(iq.queues[tmuxTarget]) > 0
}

// SetNotifyMsg stores the Telegram message ID for the inject notification.
func (iq *InjectQueueStore) SetNotifyMsg(tmuxTarget string, msgID int) {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	iq.notifyMsgs[tmuxTarget] = msgID
}

// GetNotifyMsg returns the notify message ID for the given tmux target.
func (iq *InjectQueueStore) GetNotifyMsg(tmuxTarget string) (int, bool) {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	id, ok := iq.notifyMsgs[tmuxTarget]
	return id, ok
}

// ItemCount returns the number of queued items for the given tmux target.
func (iq *InjectQueueStore) ItemCount(tmuxTarget string) int {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	return len(iq.queues[tmuxTarget])
}

// GetTexts returns the text of all queued items for the given tmux target.
func (iq *InjectQueueStore) GetTexts(tmuxTarget string) []string {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	items := iq.queues[tmuxTarget]
	texts := make([]string, len(items))
	for i, item := range items {
		texts[i] = item.Text
	}
	return texts
}

func (iq *InjectQueueStore) saveLocked() {
	type persistData struct {
		Queues map[string][]InjectItem `json:"queues"`
	}
	data, _ := json.MarshalIndent(persistData{Queues: iq.queues}, "", "  ")
	path := filepath.Join(iq.configDir, "inject-queue.json")
	os.WriteFile(path, data, 0644)
}

// Load restores the inject queue from disk.
func (iq *InjectQueueStore) Load() {
	path := filepath.Join(iq.configDir, "inject-queue.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var persist struct {
		Queues map[string][]InjectItem `json:"queues"`
	}
	if json.Unmarshal(data, &persist) == nil && persist.Queues != nil {
		iq.mu.Lock()
		iq.queues = persist.Queues
		iq.mu.Unlock()
		logger.Info(fmt.Sprintf("Inject queue loaded: %d targets", len(persist.Queues)))
	}
}

// InjectConfirmStore manages per-target channels for post-inject UserPromptSubmit confirmation.
type InjectConfirmStore struct {
	mu       sync.Mutex
	channels map[string]chan struct{}
}

// NewInjectConfirmStore creates an empty InjectConfirmStore.
func NewInjectConfirmStore() *InjectConfirmStore {
	return &InjectConfirmStore{
		channels: make(map[string]chan struct{}),
	}
}

// Register creates a confirmation channel for the target and returns it.
// The caller should select on this channel with a timeout.
func (ic *InjectConfirmStore) Register(tmuxTarget string) chan struct{} {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	ch := make(chan struct{}, 1)
	ic.channels[tmuxTarget] = ch
	return ch
}

// Confirm signals the confirmation channel for the target (if registered).
func (ic *InjectConfirmStore) Confirm(tmuxTarget string) {
	ic.mu.Lock()
	ch, ok := ic.channels[tmuxTarget]
	if ok {
		delete(ic.channels, tmuxTarget)
	}
	ic.mu.Unlock()
	if ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Cancel removes the confirmation channel without signaling.
func (ic *InjectConfirmStore) Cancel(tmuxTarget string) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	delete(ic.channels, tmuxTarget)
}
