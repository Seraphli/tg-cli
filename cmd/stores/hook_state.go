package stores

import (
	"fmt"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/logger"
)

// CCBusyTTL bridges spinner-less streaming pauses between cc MessageDisplay deltas (phase31 TC-b).
const CCBusyTTL = 4 * time.Second

// BusyState is the stored busy state of one session: idle, working, waiting or compacting. The 1 s busy
// loop stores it once per tick and the UserPromptSubmit hook stores working at once; every other place reads
// the stored value instead of the pane.
type BusyState int

const (
	BusyIdle BusyState = iota
	BusyWorking
	BusyWaiting    // cc stall banner: no data from the API for 20 s
	BusyCompacting // the CLI compacts the conversation (manual /compact or automatic), read from the pane
)

// busyEntry is the stored state plus, for BusyWaiting, the retry countdown copied from the pane.
type busyEntry struct {
	state     BusyState
	countdown string
}

// HookRunningStateStore tracks whether CC is running based on hook events.
// PreToolUse sets running; Stop sets idle. More reliable than pane title checks. It also holds the
// BusyState the 1 s busy loop stored for every session.
type HookRunningStateStore struct {
	mu         sync.RWMutex
	state      map[string]bool      // tmuxTarget → true=running, false=idle
	ccActivity map[string]time.Time // tmuxTarget → last cc MessageDisplay time
	busy       map[string]busyEntry // tmuxTarget → stored busy state
}

// NewHookRunningStateStore creates an empty HookRunningStateStore.
func NewHookRunningStateStore() *HookRunningStateStore {
	return &HookRunningStateStore{
		state:      make(map[string]bool),
		ccActivity: make(map[string]time.Time),
		busy:       make(map[string]busyEntry),
	}
}

// RecordCCActivity stamps the current time as the last cc MessageDisplay for the given target.
func (h *HookRunningStateStore) RecordCCActivity(tmuxTarget string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ccActivity[tmuxTarget] = time.Now()
}

// CCActive reports whether a cc MessageDisplay was recorded for tmuxTarget within ttl.
func (h *HookRunningStateStore) CCActive(tmuxTarget string, ttl time.Duration) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	last, ok := h.ccActivity[tmuxTarget]
	if !ok {
		return false
	}
	return time.Since(last) < ttl
}

// ClearCCActivity deletes the recorded cc activity for the given target.
func (h *HookRunningStateStore) ClearCCActivity(tmuxTarget string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.ccActivity, tmuxTarget)
}

// SetRunning marks the given target as running.
func (h *HookRunningStateStore) SetRunning(tmuxTarget string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state[tmuxTarget] = true
}

// SetIdle marks the given target as idle.
func (h *HookRunningStateStore) SetIdle(tmuxTarget string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state[tmuxTarget] = false
}

// IsRunning returns (running bool, known bool).
// If !known, the caller should fall back to a pane title check.
func (h *HookRunningStateStore) IsRunning(tmuxTarget string) (bool, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	running, known := h.state[tmuxTarget]
	return running, known
}

// SetBusyState stores the state (and the stall countdown) for tmuxTarget: the busy loop's classification, or
// working from the UserPromptSubmit hook.
func (h *HookRunningStateStore) SetBusyState(tmuxTarget string, state BusyState, countdown string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.busy[tmuxTarget] = busyEntry{state, countdown}
}

// GetBusyState returns the stored state and countdown of tmuxTarget; a target no tick has seen reads idle.
func (h *HookRunningStateStore) GetBusyState(tmuxTarget string) (BusyState, string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e := h.busy[tmuxTarget]
	return e.state, e.countdown
}

// PruneBusyStates drops the stored state of every target that is not in keep.
func (h *HookRunningStateStore) PruneBusyStates(keep map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for target := range h.busy {
		if !keep[target] {
			delete(h.busy, target)
		}
	}
}

// StopCooldownStore records the last Stop event time per target to prevent
// injection during CC's TUI transition state after Stop.
type StopCooldownStore struct {
	mu    sync.RWMutex
	times map[string]time.Time
}

// NewStopCooldownStore creates an empty StopCooldownStore.
func NewStopCooldownStore() *StopCooldownStore {
	return &StopCooldownStore{
		times: make(map[string]time.Time),
	}
}

// Record saves the current time as the last Stop for the given target.
func (s *StopCooldownStore) Record(tmuxTarget string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.times[tmuxTarget] = time.Now()
}

// HasRecentStop reports whether a Stop event was recorded for the target within the given duration.
func (s *StopCooldownStore) HasRecentStop(tmuxTarget string, within time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	lastStop, ok := s.times[tmuxTarget]
	if !ok {
		return false
	}
	return time.Since(lastStop) < within
}

// WaitIfNeeded sleeps for the remaining cooldown duration if needed.
func (s *StopCooldownStore) WaitIfNeeded(tmuxTarget string, cooldown time.Duration) {
	s.mu.RLock()
	lastStop, ok := s.times[tmuxTarget]
	s.mu.RUnlock()
	if !ok {
		return
	}
	elapsed := time.Since(lastStop)
	if elapsed < cooldown {
		wait := cooldown - elapsed
		logger.Debug(fmt.Sprintf("stopCooldown: waiting %v for target=%s", wait, tmuxTarget))
		time.Sleep(wait)
	}
}
