package stores

import (
	"fmt"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/logger"
)

// HookRunningStateStore tracks whether CC is running based on hook events.
// PreToolUse sets running; Stop sets idle. More reliable than pane title checks.
type HookRunningStateStore struct {
	mu    sync.RWMutex
	state map[string]bool // tmuxTarget → true=running, false=idle
}

// NewHookRunningStateStore creates an empty HookRunningStateStore.
func NewHookRunningStateStore() *HookRunningStateStore {
	return &HookRunningStateStore{
		state: make(map[string]bool),
	}
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
