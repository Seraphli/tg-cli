package stores

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// CommandStatsStore tracks per-command invocation counts. Thread-safe, persisted to JSON.
type CommandStatsStore struct {
	mu     sync.Mutex
	path   string
	dirty  bool
	counts map[string]int
}

// NewCommandStatsStore creates an empty CommandStatsStore. Call LoadFromDisk to hydrate from disk.
func NewCommandStatsStore(configDir string) *CommandStatsStore {
	return &CommandStatsStore{
		path:   filepath.Join(configDir, "command_stats.json"),
		counts: make(map[string]int),
	}
}

// Record increments the counter for cmdName and marks the store dirty.
func (s *CommandStatsStore) Record(cmdName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[cmdName]++
	s.dirty = true
}

// GetAll returns a snapshot copy of the current counts map.
func (s *CommandStatsStore) GetAll() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.counts))
	for k, v := range s.counts {
		out[k] = v
	}
	return out
}

// IsDirty reports whether there have been Record calls since the last SaveToDisk.
func (s *CommandStatsStore) IsDirty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirty
}

// LoadFromDisk reads counts from the JSON file. Missing file is not an error.
func (s *CommandStatsStore) LoadFromDisk() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Unmarshal(data, &s.counts)
}

// SaveToDisk writes counts to the JSON file and clears the dirty flag on success.
func (s *CommandStatsStore) SaveToDisk() error {
	s.mu.Lock()
	data, err := json.MarshalIndent(s.counts, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.path, data, 0644); err != nil {
		return err
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}
