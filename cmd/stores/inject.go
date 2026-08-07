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

// ReEnqueue prepends items back to the front of the queue for the given target.
func (iq *InjectQueueStore) ReEnqueue(tmuxTarget string, items []InjectItem) {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	iq.queues[tmuxTarget] = append(items, iq.queues[tmuxTarget]...)
	if _, ok := iq.injectIDs[tmuxTarget]; !ok {
		iq.injectIDs[tmuxTarget] = fmt.Sprintf("%x", time.Now().UnixNano()%0xFFFFFF)
	}
	iq.saveLocked()
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

// PurgeMatching removes queued items for the given tmux target whose text contains any of the
// provided markers. Returns the number of items removed. Used on @ channel close to drop stale
// forwards/replies that would otherwise be flushed into the pane after the channel is gone.
func (iq *InjectQueueStore) PurgeMatching(tmuxTarget string, markers ...string) int {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	items := iq.queues[tmuxTarget]
	if len(items) == 0 {
		return 0
	}
	kept := make([]InjectItem, 0, len(items))
	removed := 0
	for _, it := range items {
		match := false
		for _, m := range markers {
			if m != "" && strings.Contains(it.Text, m) {
				match = true
				break
			}
		}
		if match {
			removed++
			continue
		}
		kept = append(kept, it)
	}
	if removed == 0 {
		return 0
	}
	if len(kept) == 0 {
		delete(iq.queues, tmuxTarget)
		delete(iq.injectIDs, tmuxTarget)
		delete(iq.notifyMsgs, tmuxTarget)
	} else {
		iq.queues[tmuxTarget] = kept
	}
	iq.saveLocked()
	return removed
}

// HasItems reports whether the queue for a tmux target is non-empty.
func (iq *InjectQueueStore) HasItems(tmuxTarget string) bool {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	return len(iq.queues[tmuxTarget]) > 0
}

// ClearTarget drops the inject queue AND the notify/inject-id metadata for a tmux target whose pane is
// confirmed dead. Deletes all three maps even when the queue is empty (a metadata-only leftover must not
// survive a target death). Returns the number of QUEUED ITEMS dropped (0 when only metadata was cleared).
// Logs each dropped item (target + text preview + reason) plus a summary count — only when items were
// actually dropped.
func (iq *InjectQueueStore) ClearTarget(tmuxTarget string) int {
	iq.mu.Lock()
	items := iq.queues[tmuxTarget]
	_, hadNotify := iq.notifyMsgs[tmuxTarget]
	_, hadID := iq.injectIDs[tmuxTarget]
	if len(items) == 0 && !hadNotify && !hadID {
		iq.mu.Unlock()
		return 0
	}
	delete(iq.queues, tmuxTarget)
	delete(iq.notifyMsgs, tmuxTarget)
	delete(iq.injectIDs, tmuxTarget)
	iq.saveLocked()
	iq.mu.Unlock()
	for _, it := range items {
		preview := it.Text
		if len(preview) > 80 {
			preview = preview[:80]
		}
		logger.Info(fmt.Sprintf("InjectQueue.ClearTarget: dropped orphaned item target=%s text=%q reason=target-dead", tmuxTarget, preview))
	}
	if len(items) > 0 {
		logger.Info(fmt.Sprintf("InjectQueue.ClearTarget: cleared %d item(s) for dead target=%s", len(items), tmuxTarget))
	}
	return len(items)
}

// ClearDeadTargets clears the queue+metadata for every QUEUED target whose pane is no longer alive.
// SessionState-independent: reaps orphans left by removal paths that bypass CleanDeadSession
// (startup ValidateAlive, SetName auto-clean) and by a queue persisted for a target that died while the
// bot was down. Returns total items cleared.
func (iq *InjectQueueStore) ClearDeadTargets(sessionExists func(target string) bool) int {
	iq.mu.Lock()
	targets := make([]string, 0, len(iq.queues))
	for t := range iq.queues {
		targets = append(targets, t)
	}
	iq.mu.Unlock()
	total := 0
	for _, t := range targets {
		if !sessionExists(t) {
			total += iq.ClearTarget(t)
		}
	}
	return total
}

// SetNotifyMsg stores the Telegram message ID for the inject notification.
// No-op when the target has no queued items (prevents metadata resurrection after ClearTarget).
func (iq *InjectQueueStore) SetNotifyMsg(tmuxTarget string, msgID int) {
	iq.mu.Lock()
	defer iq.mu.Unlock()
	if len(iq.queues[tmuxTarget]) == 0 {
		return
	}
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

// InjectMode is the delivery mode chosen by the event-driven inject-queue router (R2).
type InjectMode int

const (
	// InjectModeQueuedCommand injects the merged queued text as a CC queued command (Force inject-while-busy).
	// Used on PreToolUse(non-AskUserQuestion) within the window and on the 5s timeout.
	InjectModeQueuedCommand InjectMode = iota
	// InjectModeAskQCustomReply delivers the merged queued text as the AskUserQuestion custom reply.
	// Used on PreToolUse(AskUserQuestion) within the window.
	InjectModeAskQCustomReply
	// InjectModeIdle waits for idle then injects WITHOUT Force. Used on Stop within the window.
	InjectModeIdle
)

// RouteInjectMode is the PURE routing DECISION (R2/R5): given the triggering hook event and tool name,
// it returns the delivery mode. Kept side-effect-free so it is unit-testable without tmux/TG.
//   - "PreToolUse" + tool != "AskUserQuestion" -> queued-command
//   - "PreToolUse" + tool == "AskUserQuestion" -> AskQ-custom-reply
//   - "Stop"                                   -> idle
//   - "" (5s timeout, no subsequent hook)      -> queued-command (case-1 behavior)
func RouteInjectMode(event, toolName string) InjectMode {
	if event == "Stop" {
		return InjectModeIdle
	}
	if event == "PreToolUse" && toolName == "AskUserQuestion" {
		return InjectModeAskQCustomReply
	}
	// PreToolUse(non-AskUserQuestion) and the "" timeout event both map to queued-command.
	return InjectModeQueuedCommand
}

// InjectRouteStore holds the per-target event-driven routing state for the inject queue (R2). It is a
// dedicated sibling of InjectConfirmStore — NOT a reuse of it — so the single-slot, type-gated
// delivery-confirmation store is never overwritten/blocked by a routing decision.
//
// Exactly-once semantics: ArmRoute atomically CLAIMs the target. Only ONE pending routing decision may
// exist per target at a time; a second MD-final (they can fire multiple times per turn) or a supplement
// check (R4) that races the first is rejected by the claim. Resolve/Timeout release the claim.
type InjectRouteStore struct {
	mu     sync.Mutex
	routes map[string]*injectRoute
}

type injectRoute struct {
	ch       chan string // receives the triggering hook event ("PreToolUse"/"Stop"); closed/drained on release
	toolName chan string // paired tool name for the event (buffered, 1)
	deadline time.Time
}

// NewInjectRouteStore creates an empty InjectRouteStore.
func NewInjectRouteStore() *InjectRouteStore {
	return &InjectRouteStore{routes: make(map[string]*injectRoute)}
}

// ArmRoute atomically claims a routing window for the target. Returns (eventCh, toolCh, true) to the
// EXACTLY-ONE caller that wins the claim; returns (nil, nil, false) if a window is already armed. The
// winner owns the window and MUST call Release(target) when done (after routing on an event or a timeout).
func (ir *InjectRouteStore) ArmRoute(tmuxTarget string, window time.Duration) (<-chan string, <-chan string, bool) {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	if _, exists := ir.routes[tmuxTarget]; exists {
		return nil, nil, false
	}
	r := &injectRoute{ch: make(chan string, 1), toolName: make(chan string, 1), deadline: time.Now().Add(window)}
	ir.routes[tmuxTarget] = r
	return r.ch, r.toolName, true
}

// IsArmed reports whether a routing window is currently claimed for the target.
func (ir *InjectRouteStore) IsArmed(tmuxTarget string) bool {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	_, ok := ir.routes[tmuxTarget]
	return ok
}

// SignalEvent delivers a subsequent hook event ("PreToolUse"/"Stop") to an armed routing window. Returns
// true if the event was accepted by a waiting window; false if no window is armed or one already received
// an event (only the FIRST subsequent hook routes). Non-blocking.
func (ir *InjectRouteStore) SignalEvent(tmuxTarget, event, toolName string) bool {
	ir.mu.Lock()
	r, ok := ir.routes[tmuxTarget]
	ir.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case r.ch <- event:
		select {
		case r.toolName <- toolName:
		default:
		}
		return true
	default:
		return false
	}
}

// Release removes the routing claim for the target so a subsequent MD-final can arm a fresh window.
func (ir *InjectRouteStore) Release(tmuxTarget string) {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	delete(ir.routes, tmuxTarget)
}
