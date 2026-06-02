package stores

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// QueueStatus returns a map of target → item count for all non-empty queues.
func (iq *InjectQueueStore) QueueStatus() map[string]int {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	result := make(map[string]int)
	for target, items := range iq.queues {
		if len(items) > 0 {
			result[target] = len(items)
		}
	}
	return result
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

type InjectConfirmType int

const (
	ConfirmUserPromptSubmit InjectConfirmType = iota
	ConfirmAskAnswered
)

type confirmEntry struct {
	expectedType    InjectConfirmType
	expectedSnippet string
	ch              chan bool
}

// InjectConfirmStore manages per-target content-verified inject confirmation.
type InjectConfirmStore struct {
	mu      sync.Mutex
	entries map[string]*confirmEntry
}

// NewInjectConfirmStore creates an empty InjectConfirmStore.
func NewInjectConfirmStore() *InjectConfirmStore {
	return &InjectConfirmStore{
		entries: make(map[string]*confirmEntry),
	}
}

// Register creates a confirmation channel for the target with expected type and snippet.
func (ic *InjectConfirmStore) Register(tmuxTarget string, expectedType InjectConfirmType, expectedSnippet string) chan bool {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	snippet := expectedSnippet
	if len(snippet) > 50 {
		snippet = snippet[:50]
	}
	ch := make(chan bool, 1)
	ic.entries[tmuxTarget] = &confirmEntry{expectedType: expectedType, expectedSnippet: snippet, ch: ch}
	return ch
}

// NotifyUserPromptSubmit signals confirmation for UserPromptSubmit events with content matching.
func (ic *InjectConfirmStore) NotifyUserPromptSubmit(tmuxTarget, prompt string) {
	ic.mu.Lock()
	entry, ok := ic.entries[tmuxTarget]
	if !ok || entry.expectedType != ConfirmUserPromptSubmit {
		ic.mu.Unlock()
		return
	}
	delete(ic.entries, tmuxTarget)
	ic.mu.Unlock()
	matched := strings.Contains(prompt, entry.expectedSnippet)
	select {
	case entry.ch <- matched:
	default:
	}
}

// NotifyAskAnswered signals confirmation for AskUserQuestion answer events with content matching.
func (ic *InjectConfirmStore) NotifyAskAnswered(tmuxTarget string, answers map[string]string) {
	ic.mu.Lock()
	entry, ok := ic.entries[tmuxTarget]
	if !ok || entry.expectedType != ConfirmAskAnswered {
		ic.mu.Unlock()
		return
	}
	delete(ic.entries, tmuxTarget)
	ic.mu.Unlock()
	matched := false
	for _, ans := range answers {
		if strings.Contains(ans, entry.expectedSnippet) {
			matched = true
			break
		}
	}
	select {
	case entry.ch <- matched:
	default:
	}
}

// Cancel removes the confirmation entry without signaling.
func (ic *InjectConfirmStore) Cancel(tmuxTarget string) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	delete(ic.entries, tmuxTarget)
}
