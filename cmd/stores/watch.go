package stores

import "sync"

// WatchEvent is the payload sent to session watchers.
type WatchEvent struct {
	Event   string `json:"event"`
	Agent   string `json:"agent"`
	Summary string `json:"summary"`
	Detail  string `json:"detail"`
}

// SessionWatchStore manages per-agent long-poll channels for session events.
type SessionWatchStore struct {
	mu      sync.Mutex
	waiters map[string][]chan WatchEvent
}

// NewSessionWatchStore creates an empty SessionWatchStore.
func NewSessionWatchStore() *SessionWatchStore {
	return &SessionWatchStore{
		waiters: make(map[string][]chan WatchEvent),
	}
}

// Register creates and registers a new channel for the given agent name.
func (sw *SessionWatchStore) Register(name string) chan WatchEvent {
	ch := make(chan WatchEvent, 1)
	sw.mu.Lock()
	sw.waiters[name] = append(sw.waiters[name], ch)
	sw.mu.Unlock()
	return ch
}

// Cancel removes a previously registered channel for the given agent name.
func (sw *SessionWatchStore) Cancel(name string, ch chan WatchEvent) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	list := sw.waiters[name]
	for i, c := range list {
		if c == ch {
			sw.waiters[name] = append(list[:i], list[i+1:]...)
			break
		}
	}
}

// Notify delivers an event to all registered watchers for the given agent name,
// then clears the waiter list for that agent.
func (sw *SessionWatchStore) Notify(agentName string, evt WatchEvent) {
	if agentName == "" {
		return
	}
	sw.mu.Lock()
	waiters := sw.waiters[agentName]
	if len(waiters) > 0 {
		for _, ch := range waiters {
			select {
			case ch <- evt:
			default:
			}
		}
		sw.waiters[agentName] = nil
	}
	sw.mu.Unlock()
}
